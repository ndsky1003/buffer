# buffer

一个高性能的自适应缓冲池，用于复用 `*bytes.Buffer` 与 `[]byte`，消除热路径上的内存分配、降低 GC 压力。

核心思路：用**环形队列 + 自旋锁**替代依赖 GC 的 `sync.Pool`，并通过 **EMA（指数加权移动平均）算法**自适应校准 buffer 大小——流量增大时快速扩容，减小时缓慢缩容，同时把异常大的 buffer 直接丢给 GC，防止内存虚高。

## 特性

- **0 分配**：池命中时 `Get`/`Put` 零内存分配
- **自适应校准**：EMA 快涨慢跌，自动收敛到合适水位
- **智能丢弃**：超过 `校准水位 × MaxPercent` 的 buffer 自动回收，防止大包污染池子
- **分级池**：应对多峰流量（如 2KB 与 2MB 长期并存），支持手动分界或按 2 的幂自动分桶
- **泛型**：`*bytes.Buffer` 与 `[]byte` 开箱即用，也可自定义类型
- **无第三方依赖**

## 架构

各类型的依赖关系（上层组合/依赖下层）：

```
应用层（调用方 Get/Put）
│
├── Pool[T]                单池：EMA 校准 + 智能丢弃
│   └── AdaptiveRingPool[T]    底层存储：自适应伸缩环形队列
│       └── SpinLock               自旋锁
│
├── ShardedPool[T]         分片池：高并发吞吐扩展
│   └── Pool[T] × N            按 key 路由到 N 个独立 Pool
│       └── AdaptiveRingPool[T]
│           └── SpinLock
│
├── ClassPool[T]           分级池：多峰流量按 size 分档
│   └── Pool[T] × N            按 size 路由到 N 个独立 Pool
│       └── AdaptiveRingPool[T]
│           └── SpinLock
│
├── ShardedClassPool[T]    分片分级池：高并发 + 多峰流量
│   └── ClassPool[T] × N       按 key 路由到 N 个独立 ClassPool
│       └── Pool[T] × M            按 size 路由到 M 个独立 Pool
│           └── AdaptiveRingPool[T]
│               └── SpinLock
│
└── FixedRingPool[T]       固定容量：满时覆盖最旧，热路径最短
    └── SpinLock
```

各层职责：

| 类型 | 职责 | 适用场景 |
|---|---|---|
| `SpinLock` | 自旋锁，短临界区下比 `sync.Mutex` 快 2~2.6 倍 | 底层基础设施 |
| `AdaptiveRingPool[T]` | 环形队列，按命中率伸缩容量（miss 多扩容、hit 多缩容） | `Pool` 的默认存储 |
| `FixedRingPool[T]` | 环形队列，容量固定、满时覆盖最旧，无伸缩统计 | 容量可预知、追求极致吞吐 |
| `Pool[T]` | 核心池，EMA 校准 buffer 大小 + 智能丢弃 | 单峰流量、低到中并发 |
| `ShardedPool[T]` | 分片池，组合 N 个 `Pool` 按 key 路由 | 高并发（游戏服、网关） |
| `ClassPool[T]` | 分级池，组合 N 个 `Pool` 按 size 路由 | 多峰流量（2KB 与 2MB 并存） |
| `ShardedClassPool[T]` | 分片分级池，组合 N 个 `ClassPool` 按 key 路由、每片再按 size 分档 | 高并发 + 多峰流量（RPC 网络库） |

## 性能

> 测试环境：Apple M1（8 物理核），Go 1.21，负载 `Get → Write 1024B → Reset → Put`，全部 **0 分配**。

### 各池吞吐（ns/op，越低越好）

| 实现 | 单核 | 8 核 | 32 核 | 64 核 |
|---|---|---|---|---|
| `FixedRingPool` | 29.3 | 79.2 | 109 | 113 |
| `AdaptiveRingPool` | 35.2 | 97.6 | 126 | 135 |
| `Pool`（含 EMA） | 39.6 | 117 | 175 | 201 |

换算 QPS（Mops/s = 1000 / ns）：

| 实现 | 单核 | 64 核 |
|---|---|---|
| `FixedRingPool` | 34.1 | 8.9 |
| `AdaptiveRingPool` | 28.4 | 7.4 |
| `Pool` | 25.2 | 5.0 |

### 分片扩展性（高并发场景）

单 `Pool` 是单锁，多核争用下吞吐不升反降；`ShardedPool` 按 key 分片后吞吐随并发扩展：

| 并发 | 单 `Pool` | `ShardedPool`（按 key） |
|---|---|---|
| 1 核 | 38.7 ns | 39.2 ns |
| 8 核 | 119 ns | 34 ns |
| 32 核 | 177 ns | 12.5 ns |
| 64 核 | 187 ns | **10.0 ns** |

### 锁选型依据（SpinLock vs sync.Mutex）

| 并发 | SpinLock | sync.Mutex |
|---|---|---|
| 单核 | 33.9 ns | 40.3 ns |
| 64 核 | 169 ns | 324 ns |

自旋锁在「短临界区 + 高争用」下显著优于 `sync.Mutex`（多核争用快约 2 倍），这是环形队列选它的原因。

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

单池是「一把自旋锁」，多核争用下吞吐不升反降（约 25 Mops/s 单核、争用后降到 5~8 Mops/s）。高并发场景（游戏服、网关）用**分片池**，把锁争用分散到多个分片，吞吐随分片数扩展：

```go
sp := buffer.NewShardedBufferPool(0) // 0 = 默认按 CPU 核数分片

shardKey := hash(connID) // 用连接 ID 等稳定标识做 key
buf := sp.Get(shardKey)
defer sp.Put(shardKey, buf)
```

**关键**：`Get`/`Put` 必须传**同一个 key**（如同一连接的标识），对象才会始终回到同一分片、不跨分片漂移。分片后多核吞吐可提升 10 倍以上。

## 高并发 + 多峰流量：分片分级池

当两种需求叠加（典型如 RPC 网络库：每连接并发处理，且既有小的心跳包又有大的 payload）时，用**分片分级池**——外层按 key 分片、内层每片按 size 分档：

```go
sp := buffer.NewShardedByteAutoClassPool(0) // 0 = 默认按 CPU 核数分片

shardKey := hash(connID)
buf := sp.Get(shardKey, uint64(payloadSize)) // 按 key 选分片、按 size 选桶
defer sp.Put(shardKey, buf)
```

`Get(key, size)` / `Put(key, v)` 的 key 语义与分片池一致（须稳定，如连接 ID 的哈希）；`size` 语义与分级池一致（期望大小，自动分桶时向上取整到 2 的幂）。也支持手动分界 `NewShardedByteClassPool(shardCount, boundaries)`。

## 工作原理

1. **环形队列复用**：`AdaptiveRingPool` 用自旋锁实现，`Get`/`Put` 命中时零分配（字段紧凑排列——锁已串行化访问，缓存行填充反而降低吞吐）。
2. **EMA 快涨慢跌**：水位上涨因子 `0.4`（对新值敏感、涨得快），下跌因子 `0.8`（对历史执着、跌得慢），并加 `1.05` 溢价，避免反复 `Grow`。
3. **智能丢弃**：归还时若 buffer 容量远超当前水位（`cap > calibratedSz × MaxPercent`），直接丢弃交给 GC，防止内存虚高。

## 注意事项

- **借了要尽快还**：`Get` 后务必配对 `Put`，且 buffer 生命周期要短。长期持有会占满池子、失去复用意义。
- **校准统计的是 `Len`（实际使用量）**：正常 `Write` 数据即可让水位准确收敛；只 `Grow` 不 `Write` 时库会回退用 `Cap` 感知扩容，但缩容仍依赖真实 `Write`。
- **单池是单自旋锁**：极高并发（几十核以上的全局热点）下扩展性受限（约 150ns/op 天花板）。若遇此场景，优先考虑按业务分多个池，而非换锁（实测自旋锁在该临界区下优于 `sync.Mutex`）。
