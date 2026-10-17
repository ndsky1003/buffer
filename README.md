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

## 工作原理

1. **环形队列复用**：`AdaptiveRingPool` 用缓存行填充（防伪共享）+ 自旋锁实现，`Get`/`Put` 命中时零分配。
2. **EMA 快涨慢跌**：水位上涨因子 `0.4`（对新值敏感、涨得快），下跌因子 `0.8`（对历史执着、跌得慢），并加 `1.05` 溢价，避免反复 `Grow`。
3. **智能丢弃**：归还时若 buffer 容量远超当前水位（`cap > calibratedSz × MaxPercent`），直接丢弃交给 GC，防止内存虚高。

## 注意事项

- **借了要尽快还**：`Get` 后务必配对 `Put`，且 buffer 生命周期要短。长期持有会占满池子、失去复用意义。
- **校准统计的是 `Len`（实际使用量）**：正常 `Write` 数据即可让水位准确收敛；只 `Grow` 不 `Write` 时库会回退用 `Cap` 感知扩容，但缩容仍依赖真实 `Write`。
- **单池是单自旋锁**：极高并发（几十核以上的全局热点）下扩展性受限（约 150ns/op 天花板）。若遇此场景，优先考虑按业务分多个池，而非换锁（实测自旋锁在该临界区下优于 `sync.Mutex`）。
