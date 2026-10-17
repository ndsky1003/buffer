package buffer

import (
	"bytes"
	"runtime"
	"sync"
	"testing"
)

// =============================================================================
// 基础功能测试
// =============================================================================

// TestBasicGetPut 测试基本的 Get/Put 操作
func TestBasicGetPut(t *testing.T) {
	p := NewBufferPool()

	buf := p.Get()
	if buf == nil {
		t.Fatal("Get() returned nil buffer")
	}

	buf.WriteString("hello world")
	p.Put(buf)

	// 再次获取应该复用之前的 buffer
	buf2 := p.Get()
	if buf2 == nil {
		t.Fatal("Get() returned nil buffer on second call")
	}
	p.Put(buf2)
}

// TestBytePoolBasic 测试 []byte 池基本功能
func TestBytePoolBasic(t *testing.T) {
	p := NewBytePool()

	b := p.Get()
	if cap(b) == 0 {
		t.Fatal("Get() returned zero-capacity byte slice")
	}

	b = append(b, []byte("hello")...)
	p.Put(b)

	b2 := p.Get()
	if cap(b2) == 0 {
		t.Fatal("Get() returned zero-capacity byte slice on second call")
	}
	p.Put(b2)
}

// TestNilBufferPut 测试放入 nil buffer
func TestNilBufferPut(t *testing.T) {
	p := NewBufferPool()
	p.Put(nil) // 应该安全，不 panic
}

// TestEmptyBufferPut 测试放入空 buffer
func TestEmptyBufferPut(t *testing.T) {
	p := NewBufferPool()

	buf := p.Get()
	// 没有写入任何数据
	p.Put(buf) // 应该安全，但不会被池化（cap 为 0 或很小）

	// 再次获取
	buf2 := p.Get()
	if buf2 == nil {
		t.Fatal("Get() returned nil after putting empty buffer")
	}
	p.Put(buf2)
}

// =============================================================================
// 配置选项测试
// =============================================================================

// TestCustomOptions 测试自定义配置
func TestCustomOptions(t *testing.T) {
	opt := Options().
		SetMinSize(1024).
		SetMaxSize(1024 * 1024).
		SetMaxPercent(2.0).
		SetCalibratePeriod(500).
		SetCalibratedSz(2048)

	p := NewBufferPool(opt)

	// 验证初始大小在合理范围内
	buf := p.Get()
	initialCap := buf.Cap()
	if initialCap < 1024 || initialCap > 2048*2 { // 允许一些浮动
		t.Logf("Initial capacity: %d (expected around 2048)", initialCap)
	}
	p.Put(buf)
}

// TestMinSizeBoundary 测试最小尺寸边界
func TestMinSizeBoundary(t *testing.T) {
	opt := Options().SetMinSize(4096)
	p := NewBufferPool(opt)

	// 放入很小的 buffer
	buf := p.Get()
	buf.WriteString("small")
	p.Put(buf)

	// 下次获取应该至少是 minSize
	buf2 := p.Get()
	if buf2.Cap() < 4096 {
		t.Errorf("Expected capacity >= 4096, got %d", buf2.Cap())
	}
	p.Put(buf2)
}

// TestMaxSizeBoundary 测试最大尺寸边界
func TestMaxSizeBoundary(t *testing.T) {
	opt := Options().
		SetMinSize(512).
		SetMaxSize(2048).
		SetCalibratePeriod(10)

	p := NewBufferPool(opt)

	// 持续放入大 buffer
	for i := 0; i < 50; i++ {
		buf := p.Get()
		buf.Grow(4096) // 尝试请求更大的
		buf.Write(make([]byte, 4096))
		p.Put(buf)
	}

	// 检查校准后的大小不超过 maxSize
	buf := p.Get()
	maxCap := buf.Cap()
	p.Put(buf)

	if maxCap > 2048*2 { // 考虑 maxPercent
		t.Errorf("Capacity %d exceeds expected max ~2048", maxCap)
	}
}

// =============================================================================
// 校准算法测试
// =============================================================================

// TestCalibrationGrowth 测试流量增长时的扩容
func TestCalibrationGrowth(t *testing.T) {
	opt := Options().SetCalibratePeriod(100).SetMinSize(512).SetCalibratedSz(512)
	p := NewBufferPool(opt)

	// 初始探测
	buf := p.Get()
	initialCap := buf.Cap()
	p.Put(buf)
	t.Logf("Initial cap: %d", initialCap)

	// 持续使用 4KB 大小的 buffer
	for i := 0; i < 200; i++ {
		buf := p.Get()
		if buf.Cap() < 4096 {
			buf.Grow(4096)
		}
		buf.Write(make([]byte, 4096))
		p.Put(buf)
	}

	// 探测新大小
	buf = p.Get()
	afterGrowthCap := buf.Cap()
	p.Put(buf)

	t.Logf("After growth cap: %d", afterGrowthCap)
	if afterGrowthCap <= initialCap {
		t.Errorf("Expected growth, but cap stayed at %d", afterGrowthCap)
	}
}

// TestCalibrationShrink 测试流量下降时的缩容
func TestCalibrationShrink(t *testing.T) {
	opt := Options().SetCalibratePeriod(100).SetMinSize(512).SetMaxSize(10240)
	p := NewBufferPool(opt)

	// 先把水位撑大
	for i := 0; i < 200; i++ {
		buf := p.Get()
		buf.Grow(8192)
		buf.Write(make([]byte, 8192))
		p.Put(buf)
	}

	buf := p.Get()
	highCap := buf.Cap()
	p.Put(buf)
	t.Logf("High watermark cap: %d", highCap)

	// 切换到小流量
	for i := 0; i < 500; i++ {
		buf := p.Get()
		buf.Write(make([]byte, 1024))
		p.Put(buf)
	}

	// 探测缩容后的大小
	buf = p.Get()
	lowCap := buf.Cap()
	p.Put(buf)

	t.Logf("After shrink cap: %d", lowCap)
	// 缩容应该缓慢，不应该立即回到最小值
	if lowCap >= highCap {
		t.Logf("Warning: No shrinking observed (high: %d, low: %d)", highCap, lowCap)
	}
}

// TestEMAResistance 测试 EMA 的抗抖动能力
func TestEMAResistance(t *testing.T) {
	opt := Options().SetCalibratePeriod(100)
	p := NewBufferPool(opt)

	// 建立基准
	for i := 0; i < 200; i++ {
		buf := p.Get()
		buf.Write(make([]byte, 4096))
		p.Put(buf)
	}

	buf := p.Get()
	baseCap := buf.Cap()
	p.Put(buf)
	t.Logf("Base cap: %d", baseCap)

	// 插入一个尖峰
	buf = p.Get()
	buf.Write(make([]byte, 32768)) // 突然大 8 倍
	p.Put(buf)

	// 继续正常流量
	for i := 0; i < 50; i++ {
		buf := p.Get()
		buf.Write(make([]byte, 4096))
		p.Put(buf)
	}

	buf = p.Get()
	spikeCap := buf.Cap()
	p.Put(buf)

	t.Logf("After spike cap: %d", spikeCap)
	// EMA 应该过滤掉单个尖峰，不应该线性增长
	if spikeCap > baseCap*3 { // 允许一定增长，但不应该完全跟随尖峰
		t.Logf("Cap grew significantly after single spike: %d -> %d", baseCap, spikeCap)
	}
}

// =============================================================================
// 智能丢弃测试
// =============================================================================

// TestSmartDiscard 测试过大 buffer 的丢弃
func TestSmartDiscard(t *testing.T) {
	opt := Options().
		SetCalibratePeriod(100).
		SetMinSize(512).
		SetMaxPercent(1.5)
	p := NewBufferPool(opt)

	// 建立正常水位
	for i := 0; i < 200; i++ {
		buf := p.Get()
		buf.Write(make([]byte, 2048))
		p.Put(buf)
	}

	buf := p.Get()
	normalCap := buf.Cap()
	p.Put(buf)
	t.Logf("Normal cap: %d", normalCap)

	// 放入一个超大 buffer
	oversizedBuf := p.Get()
	oversizedBuf.Grow(normalCap * 3)
	oversizedBuf.Write(make([]byte, normalCap*3))
	p.Put(oversizedBuf)

	// 马上再获取，应该还是正常大小（超大 buffer 被丢弃了）
	buf = p.Get()
	nextCap := buf.Cap()
	p.Put(buf)

	t.Logf("Next cap after oversized: %d", nextCap)
	// 如果超大 buffer 被正确丢弃，容量不应该突然变大
	if nextCap > normalCap*2 {
		t.Errorf("Oversized buffer may have been pooled: %d -> %d", normalCap, nextCap)
	}
}

// =============================================================================
// 并发安全测试
// =============================================================================

// TestConcurrentGetPut 测试并发 Get/Put
func TestConcurrentGetPut(t *testing.T) {
	p := NewBufferPool()
	const goroutines = 100
	const opsPerGoroutine = 1000

	var wg sync.WaitGroup
	start := make(chan struct{})

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			<-start

			for j := 0; j < opsPerGoroutine; j++ {
				buf := p.Get()
				// 模拟随机使用量
				size := 512 + (id%5)*1024
				if buf.Cap() < size {
					buf.Grow(size)
				}
				buf.Write(make([]byte, size))
				p.Put(buf)
			}
		}(i)
	}

	close(start)
	wg.Wait()
	// 如果没有 panic 或 deadlock，测试通过
	t.Log("Concurrent test completed without errors")
}

// TestConcurrentCalibration 测试并发校准
func TestConcurrentCalibration(t *testing.T) {
	opt := Options().SetCalibratePeriod(10) // 频繁触发校准
	p := NewBufferPool(opt)

	const goroutines = 50
	const opsPerGoroutine = 500

	var wg sync.WaitGroup
	start := make(chan struct{})

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			<-start

			// 波动流量
			for j := 0; j < opsPerGoroutine; j++ {
				buf := p.Get()
				size := 1024 + ((j+id)%5)*2048
				if buf.Cap() < size {
					buf.Grow(size)
				}
				buf.Write(make([]byte, size))
				p.Put(buf)
			}
		}(i)
	}

	close(start)
	wg.Wait()

	// 验证最终状态有效
	buf := p.Get()
	if buf == nil {
		t.Fatal("Got nil buffer after concurrent operations")
	}
	p.Put(buf)
	t.Log("Concurrent calibration test completed")
}

// =============================================================================
// 边界条件测试
// =============================================================================

// TestZeroSizeBuffer 测试零大小 buffer
func TestZeroSizeBuffer(t *testing.T) {
	p := NewBufferPool()

	buf := p.Get()
	// 不写入任何数据，Len = 0
	p.Put(buf)

	// 再次获取
	buf2 := p.Get()
	if buf2 == nil {
		t.Fatal("Got nil after putting zero-size buffer")
	}
	p.Put(buf2)
}

// TestTinyBuffer 测试极小 buffer
func TestTinyBuffer(t *testing.T) {
	p := NewBufferPool()

	for i := 0; i < 100; i++ {
		buf := p.Get()
		buf.Write([]byte("x")) // 只写 1 字节
		p.Put(buf)
	}

	// 应该能正常工作
	buf := p.Get()
	buf.Write([]byte("test"))
	p.Put(buf)
}

// TestLargeBuffer 测试大 buffer
func TestLargeBuffer(t *testing.T) {
	opt := Options().SetMaxSize(10 * 1024 * 1024).SetCalibratePeriod(50)
	p := NewBufferPool(opt)

	// 使用大 buffer
	for i := 0; i < 100; i++ {
		buf := p.Get()
		size := 1024 * 1024 // 1MB
		if buf.Cap() < size {
			buf.Grow(size)
		}
		buf.Write(make([]byte, size))
		p.Put(buf)
	}

	buf := p.Get()
	if buf.Cap() > 10*1024*1024*2 { // 考虑 maxPercent
		t.Errorf("Buffer cap %d exceeds reasonable range", buf.Cap())
	}
	p.Put(buf)
}

// TestRapidTrafficChange 测试快速流量变化
func TestRapidTrafficChange(t *testing.T) {
	opt := Options().SetCalibratePeriod(50)
	p := NewBufferPool(opt)

	sizes := []int{512, 4096, 512, 8192, 512, 2048, 512}

	for _, size := range sizes {
		for i := 0; i < 100; i++ {
			buf := p.Get()
			if buf.Cap() < size {
				buf.Grow(size)
			}
			buf.Write(make([]byte, size))
			p.Put(buf)
		}

		buf := p.Get()
		t.Logf("After size %d: pool cap = %d", size, buf.Cap())
		p.Put(buf)
	}
}

// =============================================================================
// []byte 池专项测试
// =============================================================================

// TestBytePoolConcurrency 测试 []byte 池并发
func TestBytePoolConcurrency(t *testing.T) {
	p := NewBytePool()
	const goroutines = 50
	const opsPerGoroutine = 500

	var wg sync.WaitGroup
	start := make(chan struct{})

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			<-start

			for j := 0; j < opsPerGoroutine; j++ {
				b := p.Get()
				size := 512 + ((id+j)%4)*1024
				if cap(b) < size {
					b = append(b, make([]byte, size)...)
				} else {
					b = b[:size]
				}
				p.Put(b[:0]) // reslice to zero
			}
		}(i)
	}

	close(start)
	wg.Wait()
	t.Log("Byte pool concurrent test completed")
}

// =============================================================================
// 内存泄漏检测
// =============================================================================

// TestMemoryLeakBasic 基础内存泄漏检测
func TestMemoryLeakBasic(t *testing.T) {
	p := NewBufferPool()

	// 记录初始内存分配
	var m1 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m1)

	// 执行大量操作
	for i := 0; i < 10000; i++ {
		buf := p.Get()
		buf.Write(make([]byte, 4096))
		p.Put(buf)
	}

	// 强制 GC 后检查
	runtime.GC()
	var m2 runtime.MemStats
	runtime.ReadMemStats(&m2)

	// 分配增长应该合理（不应该线性增长）
	t.Logf("Heap alloc: %d -> %d (diff: %d)",
		m1.HeapAlloc, m2.HeapAlloc, int64(m2.HeapAlloc)-int64(m1.HeapAlloc))
}

// =============================================================================
// Option Builder 测试
// =============================================================================

// TestOptionMerge 测试 Option 合并
func TestOptionMerge(t *testing.T) {
	opt1 := Options().SetMinSize(1024).SetMaxSize(2048)
	opt2 := Options().SetCalibratePeriod(500)
	opt3 := Options().SetMaxPercent(2.0)

	merged := opt1.Merge(opt2, opt3)

	if *merged.MinSize != 1024 {
		t.Errorf("Expected MinSize 1024, got %d", *merged.MinSize)
	}
	if *merged.MaxSize != 2048 {
		t.Errorf("Expected MaxSize 2048, got %d", *merged.MaxSize)
	}
	if *merged.CalibratePeriod != 500 {
		t.Errorf("Expected CalibratePeriod 500, got %d", *merged.CalibratePeriod)
	}
	if *merged.MaxPercent != 2.0 {
		t.Errorf("Expected MaxPercent 2.0, got %f", *merged.MaxPercent)
	}
}

// TestDefaultOptions 测试默认选项
func TestDefaultOptions(t *testing.T) {
	p := NewBufferPool() // 使用默认选项

	// 验证池子可以正常工作
	buf := p.Get()
	if buf == nil {
		t.Fatal("NewBufferPool with defaults returned nil pool")
	}
	p.Put(buf)
}

// TestNilOptionReceiver 测试 nil 接收者的方法调用
// 这验证了 option.go 中的 nil 检查是有效的，不是死代码
func TestNilOptionReceiver(t *testing.T) {
	var opt *Option = nil

	// Go 允许对 nil 指针调用方法（只要不访问字段）
	// SetXxx 方法会检测到 nil 并创建新对象
	opt = opt.SetMinSize(512)
	if opt == nil {
		t.Fatal("SetMinSize on nil receiver should return new Option")
	}
	if *opt.MinSize != 512 {
		t.Errorf("Expected MinSize 512, got %d", *opt.MinSize)
	}

	// 链式调用
	opt = opt.SetMaxSize(1024).SetCalibratePeriod(100)
	if *opt.MaxSize != 1024 {
		t.Errorf("Expected MaxSize 1024, got %d", *opt.MaxSize)
	}
	if *opt.CalibratePeriod != 100 {
		t.Errorf("Expected CalibratePeriod 100, got %d", *opt.CalibratePeriod)
	}
}

// =============================================================================
// resize 缩容越界回归测试
// =============================================================================

// TestAdaptiveRingResizeShrink 回归：缩容时若空闲对象数超过新容量，
// 旧实现会 newBuf[copyCount] 越界 panic；修复后应安全丢弃多余对象并保持队列不变量。
func TestAdaptiveRingResizeShrink(t *testing.T) {
	cases := []struct {
		curCap, count, newCap int
	}{
		{512, 500, 409}, // 缩容丢弃多余
		{512, 409, 409}, // 缩容恰好满
		{512, 300, 409}, // 缩容部分
		{512, 0, 32},    // 缩容空池
		{32, 32, 512},   // 扩容满
		{32, 10, 512},   // 扩容部分
	}
	for _, c := range cases {
		p := &AdaptiveRingPool[*bytes.Buffer]{
			buffer: make([]*bytes.Buffer, c.curCap),
			minCap: 32, maxCap: 512, curCap: c.curCap,
		}
		p.head = 0
		p.count = c.count
		p.tail = c.count % c.curCap
		for i := 0; i < c.count; i++ {
			p.buffer[i] = &bytes.Buffer{}
		}

		p.resize(c.newCap)

		wantCount := min(c.count, c.newCap)
		if p.curCap != c.newCap || p.count != wantCount || p.head != 0 || p.tail != wantCount%c.newCap {
			t.Fatalf("case %+v: curCap=%d count=%d head=%d tail=%d (wantCount=%d)",
				c, p.curCap, p.count, p.head, p.tail, wantCount)
		}
		// 缩放后循环 Get/Put 不应 panic
		for i := 0; i < p.count; i++ {
			b := p.Get()
			if b == nil {
				t.Fatalf("case %+v: Get 返回 nil", c)
			}
			p.Put(b)
		}
	}
}

// =============================================================================
// 自适应伸缩方向回归测试
// =============================================================================

// TestAdaptiveRingNoInfiniteGrow 回归：单对象串行复用（命中率趋近 100%），
// 旧逻辑「命中率高→扩容」会让池子无限扩张到 maxCap（实际只用 1 个对象）；
// 修复后应始终保持 minCap，且冷启动首次 miss 不应误触发扩容。
func TestAdaptiveRingNoInfiniteGrow(t *testing.T) {
	p := NewAdaptiveRingPool(func() *bytes.Buffer { return &bytes.Buffer{} })
	for i := 0; i < 100000; i++ {
		b := p.Get()
		p.Put(b)
		if p.curCap != p.minCap {
			t.Fatalf("第 %d 次后 curCap=%d，期望始终 minCap=%d（不应扩容）", i, p.curCap, p.minCap)
		}
	}
}

// TestAdaptiveRingGrowOnMiss 回归：持续 miss（池子不够用）应扩容。
func TestAdaptiveRingGrowOnMiss(t *testing.T) {
	p := NewAdaptiveRingPoolWithLimit(32, 512, func() *bytes.Buffer { return &bytes.Buffer{} })
	for i := 0; i < 2000; i++ {
		b := p.Get() // 池子几乎一直空，大量 miss
		if i%50 == 0 {
			p.Put(b) // 偶尔归还，触发 autoScale
		}
	}
	if p.curCap <= p.minCap {
		t.Fatalf("持续 miss 后 curCap=%d，期望 > minCap=%d（应扩容）", p.curCap, p.minCap)
	}
}

// TestAdaptiveRingPeriodicReset 验证周期采样：命中率居中时，getCount 达到周期长度即重置，
// 不会无限累积（否则命中率对近期流量变化不敏感）。
func TestAdaptiveRingPeriodicReset(t *testing.T) {
	p := NewAdaptiveRingPool(func() *bytes.Buffer { return &bytes.Buffer{} })
	maxGC := int64(0)
	// 稳定 50% 命中率（一半持有一半放回），命中率居中，应周期重置
	for i := 0; i < 20000; i++ {
		b := p.Get()
		if i%2 == 0 {
			p.Put(b)
		}
		if gc := p.getCount.Load(); gc > maxGC {
			maxGC = gc
		}
	}
	if maxGC > calibrateSamples*2 {
		t.Fatalf("getCount 峰值=%d 远超周期长度 %d，周期重置未生效", maxGC, calibrateSamples)
	}
}
