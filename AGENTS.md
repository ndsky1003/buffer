# AGENTS.md

## 概述

高性能 buffer 池库：用环形队列（`AdaptiveRingPool`）+ `sync.Mutex` 替代 `sync.Pool`，通过 EMA 算法自适应调整 buffer 大小。Go 泛型（Go 1.21），支持 `*bytes.Buffer` 与 `[]byte`。

- 模块路径：`github.com/ndsky1003/buffer/v3`（注意 `/v3` 后缀；包名为 `buffer`）
- 依赖仅 `golang.org/x/sys`（`cpu.CacheLinePad`，SPSC 队列防伪共享用）、无 CI、无 Makefile。

## 常用命令

```bash
go build ./...
go test ./...                     # 完整测试，约 9s（含并发测试）
go test -run TestName -v          # 运行单个测试
go test -bench .                  # 基准测试
```

- `TestQPSManual`、`TestEvolution`、`TestCooldownDebug` 通过 stdout 打印结果而非断言。
- `BenchmarkMaxQPS*` 会故意泄漏 goroutine（在 benchmark 中可接受）。

## 架构

泛型核心 `Pool[T]` 通过三个函数指针抽象 `*bytes.Buffer` 与 `[]byte` 的差异：

- `makeFunc(size)`：按给定容量新建对象
- `resetFunc(v)`：归还前重置（`bytes.Buffer.Reset` / slice 的 `b[:0]`）
- `statFunc(v)`：返回 `(used, cap)`，即实际使用量（`Len`）与底层容量（`Cap`）

`Pool[T]` 的 EMA 校准（`pool.go`）：

- `emaUpFactor = 0.4`（涨得快）、`emaDownFactor = 0.8`（跌得慢）、`premiumFactor = 1.05`（额外 5% 余量）
- 每 `calibratePeriod` 次 Put 校准一次（默认 1000）
- 归还时若 `cap > calibratedSz * maxPercent` 直接丢弃交给 GC（防止超大 buffer 滞留）

`AdaptiveRingPool[T]`（`AdaptiveRingPool.go`）：`sync.Mutex` 保护的环形队列，按命中率自适应扩容/缩容。字段**紧凑排列**——锁已串行化访问，实测缓存行填充反而破坏局部性、降低吞吐（多核下慢 1.3~1.7×）。

`FixedRingPool[T]`（`1.FixedRingPool.go`）：固定容量的环形对象池，无伸缩、无命中率统计，热路径更短，实测比 `AdaptiveRingPool` 快约 17%~26%。队列满时**覆盖最旧**（丢弃最旧的、保留新归还的），保持池子为最新对象。锁用 `sync.Mutex`。适合容量可预先确定、追求极致吞吐的场景。`SetEvict(fn)` 登记淘汰回调，返回自身可链式（`NewFixedRingPool(cap, newFn).SetEvict(fn)`）。

`ShardedFixedRingPool[T]`（`2.ShardedFixedRingPool.go`）：在 `FixedRingPool` 基础上按 `uint32` key 分片（外层分片、内层每片一个 `FixedRingPool`），解决单锁串行化 + 缓存行伪共享导致的并发暴跌——实测并发吞吐从 ~195ns/op 降到 ~9ns/op（约 21 倍，超过单 goroutine 的 15ns/op）。无校准、无 reset，`Get` 复用对象须自行 reset。`SetEvict(fn)` 传播到所有分片，返回自身可链式。

## 关键陷阱

- **校准基准是 `Len`（实际使用量），不是 `Cap`**：`Put` 里 `used = Len`，仅当 `Len==0`（只 `Grow` 不 `Write`）时回退用 `Cap`。缩容靠 `Len`，所以测试/基准要验证「写不满→缩容」必须真实 `Write` 数据（只 `Grow` 会让 `Len` 全 0，走 `Cap` 回退，测不出缩容）。见 `simulation_test.go`。
- **`AdaptiveRingPool` 伸缩方向别改反**：`autoScale` 是「命中率**低**（miss 多，池子不够用）→ **扩容**；命中率**高**（hit 多，池子过剩）→ **缩容**」。命中率 = `Get` 时池子有空闲对象的比例。曾因写成反的导致「单对象串行复用」无限扩容到 `maxCap`（见 `TestAdaptiveRingNoInfiniteGrow` 回归测试）。
- `Put(nil)` 安全（`statFunc` 返回 cap 0，直接 return）。
- **`resetFunc` 的返回值必须被使用**：`Pool.Put` 里要 `p.pool.Put(p.resetFunc(b))` 而非 `p.resetFunc(b); p.pool.Put(b)`。`[]byte` 的 resetFunc 是 `b[:0]`（返回 len=0 的**新 slice header**，并不原地改 `b`），忽略返回值会归还 len=原始长度的 slice → 下次 `Get` 复用时 `len != 0`、旧数据未清空（zero-copy 大忌）。`*bytes.Buffer` 的 `b.Reset()` 是原地修改所以看不出来，但 `[]byte` 池会踩坑。见 `sharded_autoclass_verify_test.go` 的 `TestShardedByteAutoClassPoolGetAndRoute`。
- **`Get` 返回的 cap 是「复用 buffer 的实际 cap」，不是 `calibratedSz`**：`calibratedSz` 只在「新建 buffer」时决定 make 的 size；一旦 ring 里有了更大的 buffer，串行复用下 `Get` 恒返回历史最大 cap，看不到校准水位的涨跌。要验证 `calibratedSz` 收敛必须直接读字段（如 `shards[0].classes[i].pool.calibratedSz.Load()`），别用 `Get` 的 cap 断言。
- **缩容靠 `used > minSize` 的采样（严格大于）**：写「恰好 == minSize」的数据不采样（`used > minSize` 为 false），水位不降；写「略大于 minSize」（如 2049 vs minSize 2048）才触发慢跌缩容。这是「保底水位」的设计，不是 bug，但测试缩容要用 `> minSize` 的量。
- **`ShardedFixedRingPool` 无自动 reset**：内层 `FixedRingPool` 不做 reset，`Get` 复用的对象残留旧数据，须调用方自行清理——`*bytes.Buffer` 要 `Reset()`，`[]byte` 归还时传 `b[:0]`（否则下次 `Get` 的 slice `len != 0`，残留旧数据）。见 `ShardedFixedRingPool_test.go` 的 `TestShardedFixedRingPoolNoAutoReset`、`TestShardedFixedBytePoolReslice`。
- **覆盖最旧会丢弃被淘汰对象 → 非 GC 资源必须 `SetEvict`**：`FixedRingPool.Put` 满时覆盖 head，旧对象默认直接交给 GC。池化 `*bytes.Buffer`/`[]byte` 无害；池化 fd、CGO 内存、连接、外部句柄时必须 `SetEvict(fn)` 登记释放回调（`ShardedFixedRingPool.SetEvict` 会传播到所有分片），否则资源泄漏。`evict` 必须在 `p.mu.Unlock()` **之后**调用（否则回调内回用 `Get`/`Put` 会死锁，见 `TestFixedRingPoolEvictMayReenterPool`）；`Put` 里要在锁内取 `p.evict` 快照，避免与 `SetEvict` 争用。evict 只会收到池内**空闲**对象，已 `Get` 走的不触发（见 `TestFixedRingPoolEvictSkipsInUseObject`）。
- **SPSC 队列「先读槽位、后发布 r」的顺序不能反**：`SpinSPSCQueue.Dequeue` 必须先 `item := slots[idx]`（并 `slots[idx]=nil`），**再** `atomic.Store(&q.r, r+1)`。若反过来（先推进 r 再读槽位，比如用 `CompareAndSwapUint64(&q.r,...)` 抢下标），生产者会看到 r 已前进、在回绕时覆盖正在被读的槽位 → 数据竞争（读到错数据）。出队后 `slots[idx]=nil` 必须在 `Store(r)` 之前做，否则 nil 写与生产者的回绕覆盖竞争。见 `TestSpinSPSCQueueFIFOConcurrent`（`-race` 下捕获）。此外：`w`/`r` 的原子读写必须保留（跨 goroutine 的同步边），但生产者读自己的 `w`、消费者读自己的 `r` 可用普通读（单写者）。

## 结构

- `pool.go` — 核心 `Pool[T]`：EMA 校准 + 智能丢弃。
- `api.go` — `NewBufferPool` / `NewBytePool` 便捷构造函数。
- `option.go` — `Options()` builder（`SetMinSize/SetMaxSize/SetMaxPercent/SetCalibratePeriod/SetCalibratedSz`），方法对 nil 接收者安全，可链式调用。
- `AdaptiveRingPool.go` — `AdaptiveRingPool[T]` 环形队列：`sync.Mutex` + 自适应扩容/缩容（紧凑排列，无缓存行填充）。
- `1.FixedRingPool.go` — `FixedRingPool[T]` 固定容量环形对象池：无伸缩、无统计，满时覆盖最旧，热路径更短。锁用 `sync.Mutex`。`SetEvict(fn)` 登记淘汰回调（池化非 GC 资源时必需）。
- `2.ShardedFixedRingPool.go` — `ShardedFixedRingPool[T]` 分片固定容量池：外层按 key（`uint32`）分片、内层每片一个 `FixedRingPool`，高并发 + 容量可预知。`SetEvict(fn)` 传播到所有分片。
- `ShardedPool.go` — `ShardedPool[T]` 分片池：按 key 分散到多个内层 Pool，避免单锁争用、吞吐随分片数扩展。注意须用**显式 key**（Get/Put 传同一 key），对象才不跨分片漂移；不要用轮询/随机路由（会导致漂移和额外分配）。
- `ShardedClassPool.go` — `ShardedClassPool[T]` 分片分级池：外层按 key 分片（高并发）、内层每片一个 `ClassPool` 按 size 分档（多峰流量）。`Get(key, size)` / `Put(key, v)`，key 语义同 ShardedPool。
- `hash.go` — `Hash(uint64) uint32` / `HashString(string) uint32`：分片 key 哈希（FNV-1a / 高低位混合）。
- `0.SpinSPSCQueue.go` — `SpinSPSCQueue[T]` 无锁 SPSC 环形队列：**严格单生产者单消费者**、无锁无信号量，`slots []*T` 存指针（避免拷贝大 T）。w/r 单调下标用 `&mask` 取槽位，w 与 r 之间用 `cpu.CacheLinePad`（`golang.org/x/sys/cpu`）隔缓存行（防伪共享）。`Close()` 后 Enqueue 拒绝、Dequeue 排空返回 nil。含 `SpinSPSCQueue_test.go`。

## 公共 API

```go
buffer.NewBufferPool(opts ...*Option) // *Pool[*bytes.Buffer]
buffer.NewBytePool(opts ...*Option)   // *Pool[[]byte]
buffer.NewPool(makeFunc, resetFunc, statFunc, opts ...) // 泛型构造
pool.Get() / pool.Put(v)

// 分级池：按对象大小自动归拢到不同水位（应对多峰流量，如 2KB 与 2MB 并存）
buffer.NewBufferClassPool(boundaries []uint64, opts ...) // *ClassPool[*bytes.Buffer]
buffer.NewByteClassPool(boundaries []uint64, opts ...)   // *ClassPool[[]byte]

// 自动分桶：按 2 的幂自动分级，无需指定分界，误差 ≤2×（推荐）
buffer.NewBufferAutoClassPool(opts ...) // *ClassPool[*bytes.Buffer]
buffer.NewByteAutoClassPool(opts ...)   // *ClassPool[[]byte]

// 分片池：按 key 分散到多个内层 Pool，高并发下吞吐随分片数扩展
buffer.NewShardedBufferPool(shardCount, opts ...) // *ShardedPool[*bytes.Buffer]，Get/Put 需传 key
buffer.NewShardedBytePool(shardCount, opts ...)   // *ShardedPool[[]byte]

// 分片固定容量池：外层按 key 分片、内层每片一个 FixedRingPool（高并发 + 容量可预知）
buffer.NewShardedFixedRingPool(shardCount, capacity, newFunc) // *ShardedFixedRingPool[T]，Get/Put 需传 uint32 key
// 淘汰回调：池满「覆盖最旧」时被淘汰的对象交给回调（池化 fd/连接/CGO 内存等非 GC 资源时必需）
// 返回接收者自身，可链式：NewFixedRingPool(cap, newFn).SetEvict(fn)
fixedRingPool.SetEvict(fn)      // FixedRingPool[T]，传 nil 清除
sharded.SetEvict(fn)            // ShardedFixedRingPool[T]，传播到所有分片

// 无锁 SPSC 队列：单生产者单消费者搬运对象（存指针，无锁无信号量）
buffer.NewSpinSPSCQueue[T](cap) // *SpinSPSCQueue[T]，cap 向上取整到 2 的幂
q.Enqueue(item *T) bool         // 满/关闭返回 false
q.Dequeue() *T                  // 空则自旋；Close 后排空返回 nil
q.TryDequeue() *T               // 空返回 nil（不阻塞）
q.Close() / q.Closed()          // 关闭 / 查询

// 分片分级池：外层按 key 分片、内层按 size 分档（高并发 + 多峰流量，如 RPC 网络库）
buffer.NewShardedBufferClassPool(shardCount, boundaries, opts ...) // *ShardedClassPool[*bytes.Buffer]，Get(key,size)/Put(key,v)
buffer.NewShardedByteClassPool(shardCount, boundaries, opts ...)   // *ShardedClassPool[[]byte]
buffer.NewShardedBufferAutoClassPool(shardCount, opts ...) // 按 2 的幂自动分桶
buffer.NewShardedByteAutoClassPool(shardCount, opts ...)

// 分片 key 哈希：把连接 ID / 会话 token 等稳定标识映射成 uint32 key
buffer.Hash(uint64) uint32        // 整型 → 高低位混合
buffer.HashString(string) uint32  // 字符串 → FNV-1a
```

默认配置（`New` 中）：`MinSize=512`、`MaxSize=64MB`、`CalibratePeriod=1000`、`MaxPercent=1.5`、`CalibratedSz=1024`。

## 多峰流量：用 ClassPool，不要用单 Pool

`Pool` 只有一个 EMA 校准水位（`calibratedSz`），面对「2KB 与 2MB 长期并存」这类多峰流量时，水位会被大对象拉高并「钉」在高位，导致小对象被迫用大 buffer（内存虚高）。此时用 `ClassPool`：

- `NewBufferClassPool(boundaries, ...)`：手动指定分界，每档独立校准、互不干扰。
- `NewBufferAutoClassPool(...)`：按 2 的幂自动分桶（512B、1K、2K、4K…），无需指定分界；`Get(size)` 会把 size 向上取整到 2 的幂再选桶，`Put` 按实际容量归拢，误差 ≤2×。多峰流量首选这个。

两个 API 都是 `Get(size)` 按期望大小选档、`Put` 按实际容量归拢。

## 高并发 + 多峰流量：用 ShardedClassPool

`ShardedPool` 解决高并发（分片、免单锁争用），`ClassPool` 解决多峰流量（分档、免水位被拉高）。两者叠加的场景（如 RPC 网络库：每连接并发处理，且既有小的心跳包又有大的 payload）用 `ShardedClassPool`：外层按 key 分片、内层每片一个 `ClassPool` 按 size 分档。API 为 `Get(key, size)` / `Put(key, v)`，key 语义同 ShardedPool（须显式稳定 key，如连接 ID 的哈希）。

## 约定

- 代码注释与 README 使用中文。
