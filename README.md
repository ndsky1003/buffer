# buffer

一个高性能的自适应缓冲池，用于复用 `*bytes.Buffer` 与 `[]byte`，消除热路径上的内存分配、降低 GC 压力。

核心思路：用**环形队列 + `sync.Mutex`**替代依赖 GC 的 `sync.Pool`，并通过 **EMA（指数加权移动平均）算法**自适应校准 buffer 大小——流量增大时快速扩容，减小时缓慢缩容，同时把异常大的 buffer 直接丢给 GC，防止内存虚高。

## 特性

- **0 分配**：池命中时 `Get`/`Put` 零内存分配
- **自适应校准**：EMA 快涨慢跌，自动收敛到合适水位
- **智能丢弃**：超过 `校准水位 × MaxPercent` 的 buffer 自动回收，防止大包污染池子
- **分级池**：应对多峰流量（如 2KB 与 2MB 长期并存），支持手动分界或按 2 的幂自动分桶
- **泛型**：`*bytes.Buffer` 与 `[]byte` 开箱即用，也可自定义类型
- **无第三方依赖**（仅 `golang.org/x/sys`，为 `cpu.CacheLinePad` 防伪共享填充）

## 架构

各类型的依赖关系（上层组合/依赖下层）：

```
应用层（调用方 Get/Put）
│
├── Pool[T]                单池：EMA 校准 + 智能丢弃
│   └── AdaptiveRingPool[T]    底层存储：自适应伸缩环形队列
│       └── sync.Mutex
│
├── ShardedPool[T]         分片池：高并发吞吐扩展
│   └── Pool[T] × N            按 key 路由到 N 个独立 Pool
│       └── AdaptiveRingPool[T]
│           └── sync.Mutex
│
├── ClassPool[T]           分级池：多峰流量按 size 分档
│   └── Pool[T] × N            按 size 路由到 N 个独立 Pool
│       └── AdaptiveRingPool[T]
│           └── sync.Mutex
│
├── ShardedClassPool[T]    分片分级池：高并发 + 多峰流量
│   └── ClassPool[T] × N       按 key 路由到 N 个独立 ClassPool
│       └── Pool[T] × M            按 size 路由到 M 个独立 Pool
│           └── AdaptiveRingPool[T]
│               └── sync.Mutex
│
├── FixedRingPool[T]       固定容量：满时覆盖最旧，热路径最短
│   └── sync.Mutex
│
└── ShardedFixedRingPool[T]  分片固定容量：高并发 + 容量可预知
    └── FixedRingPool[T] × N     按 key 路由到 N 个独立 FixedRingPool
        └── sync.Mutex

└── SpinSPSCQueue[T]        无锁 SPSC 环形队列：单生产者单消费者搬运对象（独立于池）
```

各层职责：

| 类型 | 职责 | 适用场景 |
|---|---|---|
| `AdaptiveRingPool[T]` | 环形队列，按命中率伸缩容量（miss 多扩容、hit 多缩容） | `Pool` 的默认存储 |
| `FixedRingPool[T]` | 环形队列，容量固定、满时覆盖最旧，无伸缩统计 | 容量可预知、追求极致吞吐 |
| `ShardedFixedRingPool[T]` | 分片固定容量池：按 key 路由到 N 个独立 `FixedRingPool` | 高并发 + 容量可预知 |
| `Pool[T]` | 核心池，EMA 校准 buffer 大小 + 智能丢弃 | 单峰流量、低到中并发 |
| `ShardedPool[T]` | 分片池，组合 N 个 `Pool` 按 key 路由 | 高并发（游戏服、网关） |
| `ClassPool[T]` | 分级池，组合 N 个 `Pool` 按 size 路由 | 多峰流量（2KB 与 2MB 并存） |
| `ShardedClassPool[T]` | 分片分级池，组合 N 个 `ClassPool` 按 key 路由、每片再按 size 分档 | 高并发 + 多峰流量（RPC 网络库） |
| `SpinSPSCQueue[T]` | 无锁 SPSC 环形队列，单生产者单消费者搬运对象（存指针，避免拷贝大对象） | 日志采集→落盘、单连接读→处理、pipeline 环节间传递 |

## 性能

> 测试环境：Apple M1（8 核），Go 1.27，全部 **0 分配**。负载：`FixedRingPool`/`AdaptiveRingPool` 为 `Get → Reset → Write 1024B → Put`，`Pool` 为 `Get → Write 1024B → Put`（Put 内自动 reset）。命令：`go test -run '^$' -bench 'BenchmarkFixedRingPool|BenchmarkAdaptiveRingPool|BenchmarkBufferPool|BenchmarkShardedFixedRingPool' -benchtime=2s -cpu=1,8`。

### 各池吞吐（ns/op，越低越好）

| 实现 | 单 goroutine | 8 goroutine 争用 |
|---|---|---|
| `FixedRingPool` | 39 | 241 |
| `AdaptiveRingPool` | 41 | 246 |
| `Pool`（含 EMA） | 32 | 265 |
| `ShardedFixedRingPool` | 38 | **23** |

- 前三行都是「单把锁」：单 goroutine 下 30~40ns；8 个 goroutine 抢同一把锁时吞吐掉到 ~240~290ns（下降约 6~8 倍）。
- `ShardedFixedRingPool` 按 key 分片（分片数 = CPU 核数），每个 goroutine 落到独立分片、几乎无争用，吞吐不随并发下降（~23ns）。
- 结论：高并发务必走 `Sharded*` 系列；单把锁的池只适合低并发，或作为分片池的内层。

### 锁选型

所有池统一用 `sync.Mutex`（自旋锁已移除）。旧版「自旋锁比 `sync.Mutex` 快 2 倍」的结论来自 x86 多核机器；在 M1 上实测：自旋锁只在低争用（≤4 并发）领先（单 goroutine 约快 45%），≥8 并发时反而比 `sync.Mutex` 慢 2~10 倍（惊群 + 无 CPU 亲和 + 异步抢占），故弃用自旋锁。

## 安装

```bash
go get github.com/ndsky1003/buffer/v3
```

## 快速开始

### `*bytes.Buffer`

```go
package main

import (
	"github.com/ndsky1003/buffer/v3"
)

var bufPool = buffer.NewBufferPool(
	buffer.Options().
		SetMinSize(512).      // 最小 512B
		SetMaxSize(64 << 20), // 最大 64MB
)

func handle() {
	buf := bufPool.Get()
	defer bufPool.Put(buf) // 用完记得归还

	buf.WriteString("hello world")
	// ...
}
```

### `[]byte`

```go
var bytePool = buffer.NewBytePool()

b := bytePool.Get()
defer bytePool.Put(b)
b = append(b, []byte("data")...)
// ...
```

## 配置项

```go
buffer.Options().
	SetMinSize(512).          // 最小 buffer 大小
	SetMaxSize(64 << 20).     // 最大 buffer 大小（防止 OOM）
	SetMaxPercent(1.5).       // 丢弃阈值：cap 超过 calibratedSz × 该值则丢给 GC
	SetCalibratePeriod(1000). // 每多少次 Put 校准一次
	SetCalibratedSz(1024)     // 初始校准大小
```

| 方法 | 默认 | 说明 |
|---|---|---|
| `SetMinSize` | 512 | 缩容下限，校准水位不会低于它 |
| `SetMaxSize` | 64MB | 扩容上限，防止单包过大导致 OOM |
| `SetMaxPercent` | 1.5 | 归还时若 `cap > calibratedSz × 该值`，buffer 直接丢弃 |
| `SetCalibratePeriod` | 1000 | 每 N 次 Put 触发一次 EMA 校准 |
| `SetCalibratedSz` | 1024 | 初始水位猜测值 |

## 多峰流量：分级池

单池只有一个校准水位。当流量长期存在多个量级（例如 2KB 与 2MB 并存）时，水位会被大对象拉高，导致小对象被迫用大 buffer、内存虚高。此时用分级池：

### 自动分桶（推荐）

按 2 的幂自动分桶（512B、1K、2K、4K…），无需指定分界，误差 ≤2×：

```go
cp := buffer.NewBufferAutoClassPool()

size := estimatePayloadSize()
buf := cp.Get(uint64(size)) // 内部向上取整到 2 的幂，自动选桶
defer cp.Put(buf)           // 按实际容量自动归拢到正确桶
```

### 手动分界

若已知确切的峰分布，可手动指定分界让误差更小：

```go
// 两档：<64K、>=64K
cp := buffer.NewBufferClassPool([]uint64{64 << 10})

buf := cp.Get(uint64(size))
defer cp.Put(buf)
```

分界要落在两个峰之间、离峰至少 8~16 倍，避免 buffer 跨档。

## 高并发：分片池

单池是「一把互斥锁」，多核争用下吞吐不升反降（约 25 Mops/s 单核、争用后降到 5~8 Mops/s）。高并发场景（游戏服、网关）用**分片池**，把锁争用分散到多个分片，吞吐随分片数扩展：

```go
sp := buffer.NewShardedBufferPool(0) // 0 = 默认按 CPU 核数分片

shardKey := buffer.Hash(uint64(connID)) // 用连接 ID 等稳定标识做 key
buf := sp.Get(shardKey)
defer sp.Put(shardKey, buf)
```

**关键**：`Get`/`Put` 必须传**同一个 key**（如同一连接的标识），对象才会始终回到同一分片、不跨分片漂移。分片后多核吞吐可提升 10 倍以上。

key 的计算用库内置的 `buffer.Hash`（整型）或 `buffer.HashString`（字符串）：

```go
shardKey := buffer.Hash(uint64(connID)) // 整型 ID（自增 ID、时间戳等）
shardKey := buffer.HashString(sessionTok) // 字符串标识（会话 token、UUID 等）
```

若 `connID` 本身是自增整数，低位天然均匀，`uint32(connID)` 即可；哈希只是为了把「低位分布不均」的标识（如时间戳、字符串）打散。

## 高并发 + 容量可预知：分片固定容量池

如果容量能预先确定、且追求极致吞吐（不需要 EMA 校准），用 `ShardedFixedRingPool`——外层按 key 分片、内层每片一个固定容量 `FixedRingPool`：

```go
sp := buffer.NewShardedFixedRingPool(0, 64, func() *bytes.Buffer {
	return bytes.NewBuffer(make([]byte, 0, 1024))
}) // 分片数=CPU 核数、每片缓存 64 个、初始 1KB

buf := sp.Get(connID) // key 为任意整型，内部取低 32 位路由
buf.Reset() // 复用对象不自动 reset，须自行清理
defer sp.Put(connID, buf)
```

与 `ShardedPool`（内层含 EMA 校准 + 自动 reset）的区别：本池无校准、无自动 reset，热路径更短，但调用方须负责 reset（`*bytes.Buffer` 用 `Reset()`，`[]byte` 归还时传 `b[:0]`）。`Get`/`Put` 的 key 为 `uint32`，取低 log2(分片数) 位路由；若 key 低位分布不均（如时间戳），可先用 `buffer.Hash` 混合。

### 池化需要显式释放的资源：SetEvict

`FixedRingPool` / `ShardedFixedRingPool` 满时会**覆盖最旧**，被淘汰的对象默认直接交给 GC。池化 `*bytes.Buffer`、`[]byte` 无所谓；但池化 fd、CGO 内存、mmap、下游连接、外部句柄这类**需要显式释放**的资源时，被淘汰的对象没人 `Close()` 就是资源泄漏。用 `SetEvict` 登记释放回调：

```go
pool := buffer.NewShardedFixedRingPool(0, 64, newUpstreamConn).
	SetEvict(func(c *UpstreamConn) { c.Close() }) // 为所有分片登记；FixedRingPool 上同名方法，均返回自身可链式
```

回调语义：

- 只在「池满被覆盖」时触发，且只会收到池内的**空闲**对象——已 `Get` 走、尚未归还的对象不会被淘汰（它们在槽位里残留的引用被覆盖时不触发）。
- 在池锁**外**执行，因此回调里可以安全回用本池的 `Get`/`Put`，不会死锁；但回调运行在调用 `Put` 的那个 goroutine 上，别在里面做长阻塞操作。
- 淘汰顺序是最旧优先，池内始终保留最新归还的对象。
- 传 `nil` 清除回调。

## 高并发 + 多峰流量：分片分级池

当两种需求叠加（典型如 RPC 网络库：每连接并发处理，且既有小的心跳包又有大的 payload）时，用**分片分级池**——外层按 key 分片、内层每片按 size 分档：

```go
sp := buffer.NewShardedByteAutoClassPool(0) // 0 = 默认按 CPU 核数分片

shardKey := buffer.Hash(uint64(connID))
buf := sp.Get(shardKey, uint64(payloadSize)) // 按 key 选分片、按 size 选桶
defer sp.Put(shardKey, buf)
```

`Get(key, size)` / `Put(key, v)` 的 key 语义与分片池一致（须稳定，如连接 ID 的哈希）；`size` 语义与分级池一致（期望大小，自动分桶时向上取整到 2 的幂）。也支持手动分界 `NewShardedByteClassPool(shardCount, boundaries)`。

## 无锁 SPSC 队列：跨 goroutine 搬运对象

`SpinSPSCQueue` 与上面的池**不是一类东西**：池负责**复用**对象，它负责在**一个生产者 goroutine 与一个消费者 goroutine**之间**传递**对象。无锁、无信号量，追求极低的搬运开销。

```go
q := buffer.NewSpinSPSCQueue[*bytes.Buffer](1024) // 容量向上取整到 2 的幂

// 生产者：采集/接收，把 buffer 交给消费者
go func() {
	for {
		buf := fetch()
		for !q.Enqueue(buf) { // 满则自旋重试，或做背压
			runtime.Gosched()
		}
	}
}()

// 消费者：取出处理（如落盘）
for {
	buf := q.Dequeue() // 空则自旋等待；Close 后排空返回 nil
	handle(buf)
	bufPool.Put(buf) // 处理完归还到对象池（队列与池配合使用）
}
```

### 使用规范（违反即数据竞争）

1. **严格单生产者、单消费者**：只能有一个 goroutine 调 `Enqueue`、一个 goroutine 调 `Dequeue`/`TryDequeue`。多生产者或多消费者会产生数据竞争，且**库不做任何检测**。
2. **元素是指针**：`Enqueue(item *T)` 只存指针、不拷贝，对象由调用方分配；**入队后所有权移交消费者**，不得再修改。
3. **处理「满」**：`Enqueue` 满返回 `false`，没有阻塞式入队——调用方决定重试、丢弃还是背压。
4. **关闭顺序**：先停生产者，再 `Close()`；消费者 `Dequeue` 会排空剩余元素后返回 `nil`，用 `Closed()` 区分「空」与「已关闭」。
5. **`TryDequeue` 用 nil 表示空**：若元素类型可能合法为 nil（如 `*bytes.Buffer`），别只靠 nil 判断状态，结合 `Closed()`。

### 适用 / 不适用场景

| 适用 | 不适用 |
|---|---|
| 单生产者 → 单消费者搬运（日志采集→落盘、单连接读→处理、pipeline 环节间传递、事件分发） | 多生产者或多消费者：改用 `channel`（有锁/有调度开销）或专门的 MPMC 队列 |
| 追求低延迟、零锁、免唤醒开销 | 需要阻塞/超时等待、信号量公平性、优先级调度 |
| 对象已由对象池管理，队列只负责搬运指针 | 元素需要长期滞留（有界，满即拒） |
| 生产者/消费者角色固定（一个 reader 一个 writer） | 需要动态增减生产/消费者数量 |

> 典型组合：`SpinSPSCQueue` 负责搬运 + `*Pool` 负责复用——生产者在队列里传递从池 `Get` 的 buffer，消费者用完 `Put` 回池。

## 工作原理

1. **环形队列复用**：`AdaptiveRingPool` 用 `sync.Mutex` 实现，`Get`/`Put` 命中时零分配（字段紧凑排列——锁已串行化访问，缓存行填充反而降低吞吐）。
2. **EMA 快涨慢跌**：水位上涨因子 `0.4`（对新值敏感、涨得快），下跌因子 `0.8`（对历史执着、跌得慢），并加 `1.05` 溢价，避免反复 `Grow`。
3. **智能丢弃**：归还时若 buffer 容量远超当前水位（`cap > calibratedSz × MaxPercent`），直接丢弃交给 GC，防止内存虚高。

## 注意事项

- **借了要尽快还**：`Get` 后务必配对 `Put`，且 buffer 生命周期要短。长期持有会占满池子、失去复用意义。
- **校准统计的是 `Len`（实际使用量）**：正常 `Write` 数据即可让水位准确收敛；只 `Grow` 不 `Write` 时库会回退用 `Cap` 感知扩容，但缩容仍依赖真实 `Write`。
- **单池是单把锁**：极高并发（几十核以上的全局热点）下扩展性受限。若遇此场景，优先考虑按业务分多个池，而非换锁。
