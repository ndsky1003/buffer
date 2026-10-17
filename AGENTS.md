# AGENTS.md

## 概述

高性能 buffer 池库：用环形队列（`AdaptiveRingPool`）+ 自旋锁替代 `sync.Pool`，通过 EMA 算法自适应调整 buffer 大小。Go 泛型（Go 1.21），支持 `*bytes.Buffer` 与 `[]byte`。

- 模块路径：`github.com/ndsky1003/buffer/v3`（注意 `/v3` 后缀；包名为 `buffer`）
- 无第三方依赖（无 `go.sum`）、无 CI、无 Makefile。

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

`Pool[T]` 的 EMA 校准（`buffer.go`）：

- `emaUpFactor = 0.4`（涨得快）、`emaDownFactor = 0.8`（跌得慢）、`premiumFactor = 1.05`（额外 5% 余量）
- 每 `calibratePeriod` 次 Put 校准一次（默认 1000）
- 归还时若 `cap > calibratedSz * maxPercent` 直接丢弃交给 GC（防止超大 buffer 滞留）

`AdaptiveRingPool[T]`（`pool.go`）：用 64 字节缓存行填充（`_ [N]byte`）防止伪共享，自旋锁保护；按命中率自适应扩容/缩容。

## 关键陷阱

- **校准基准是 `Len`（实际使用量），不是 `Cap`**：`Put` 里 `used = Len`，仅当 `Len==0`（只 `Grow` 不 `Write`）时回退用 `Cap`。缩容靠 `Len`，所以测试/基准要验证「写不满→缩容」必须真实 `Write` 数据（只 `Grow` 会让 `Len` 全 0，走 `Cap` 回退，测不出缩容）。见 `simulation_test.go`。
- `Put(nil)` 安全（`statFunc` 返回 cap 0，直接 return）。

## 结构

- `buffer.go` — 核心 `Pool[T]`：EMA 校准 + 智能丢弃。
- `api.go` — `NewBufferPool` / `NewBytePool` 便捷构造函数。
- `option.go` — `Options()` builder（`SetMinSize/SetMaxSize/SetMaxPercent/SetCalibratePeriod/SetCalibratedSz`），方法对 nil 接收者安全，可链式调用。
- `pool.go` — `AdaptiveRingPool[T]` 环形队列：缓存行填充 + 自旋锁 + 自适应扩容/缩容。
- `SpinLock.go` — 自旋锁实现。

## 公共 API

```go
buffer.NewBufferPool(opts ...*Option) // *Pool[*bytes.Buffer]
buffer.NewBytePool(opts ...*Option)   // *Pool[[]byte]
buffer.New(makeFunc, resetFunc, statFunc, opts ...) // 泛型构造
pool.Get() / pool.Put(v)

// 分级池：按对象大小自动归拢到不同水位（应对多峰流量，如 2KB 与 2MB 并存）
buffer.NewBufferClassPool(boundaries []uint64, opts ...) // *ClassPool[*bytes.Buffer]
buffer.NewByteClassPool(boundaries []uint64, opts ...)   // *ClassPool[[]byte]

// 自动分桶：按 2 的幂自动分级，无需指定分界，误差 ≤2×（推荐）
buffer.NewBufferAutoClassPool(opts ...) // *ClassPool[*bytes.Buffer]
buffer.NewByteAutoClassPool(opts ...)   // *ClassPool[[]byte]
```

默认配置（`New` 中）：`MinSize=512`、`MaxSize=64MB`、`CalibratePeriod=1000`、`MaxPercent=1.5`、`CalibratedSz=1024`。

## 多峰流量：用 ClassPool，不要用单 Pool

`Pool` 只有一个 EMA 校准水位（`calibratedSz`），面对「2KB 与 2MB 长期并存」这类多峰流量时，水位会被大对象拉高并「钉」在高位，导致小对象被迫用大 buffer（内存虚高）。此时用 `ClassPool`：

- `NewBufferClassPool(boundaries, ...)`：手动指定分界，每档独立校准、互不干扰。
- `NewBufferAutoClassPool(...)`：按 2 的幂自动分桶（512B、1K、2K、4K…），无需指定分界；`Get(size)` 会把 size 向上取整到 2 的幂再选桶，`Put` 按实际容量归拢，误差 ≤2×。多峰流量首选这个。

两个 API 都是 `Get(size)` 按期望大小选档、`Put` 按实际容量归拢。

## 约定

- 代码注释与 README 使用中文。
