package buffer

import (
	"fmt"
	"sync"
	"testing"
)

// ============ 模块 1：SpinLock 互斥 ============
// 理想步骤：N 个 goroutine 并发 Lock → 递增共享计数器 → Unlock。
// 正确性判据：计数器最终 == N×perG，无竞争丢失（配合 go test -race 验证无数据竞争）。
func TestVerifySpinLock(t *testing.T) {
	var lock SpinLock
	var counter int64
	const goroutines = 32
	const perG = 10000
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < perG; j++ {
				lock.Lock()
				counter++
				lock.Unlock()
			}
		}()
	}
	wg.Wait()
	want := int64(goroutines * perG)
	fmt.Printf("\n=== 模块1：SpinLock 互斥 ===\n")
	fmt.Printf("计数器 = %d，期望 = %d（32×10000）\n", counter, want)
	if counter != want {
		t.Errorf("SpinLock 互斥失败：%d != %d", counter, want)
	}
}

// ============ 模块 2：Option 配置 ============
// 理想步骤：Options() → 链式 SetXxx → Merge（delta 非 nil 字段覆盖 base）。
func TestVerifyOption(t *testing.T) {
	fmt.Printf("\n=== 模块2：Option 配置 ===\n")
	o := Options().
		SetMinSize(1024).
		SetMaxSize(1 << 20).
		SetMaxPercent(2.0).
		SetCalibratePeriod(500).
		SetCalibratedSz(2048)
	fmt.Printf("链式设置: MinSize=%d MaxSize=%d MaxPercent=%.1f CalibratePeriod=%d CalibratedSz=%d\n",
		*o.MinSize, *o.MaxSize, *o.MaxPercent, *o.CalibratePeriod, *o.CalibratedSz)
	if *o.MinSize != 1024 || *o.MaxSize != 1<<20 || *o.MaxPercent != 2.0 || *o.CalibratePeriod != 500 || *o.CalibratedSz != 2048 {
		t.Errorf("链式设置错误")
	}

	// Merge：delta 只覆盖非 nil 字段
	base := Options().SetMinSize(512).SetMaxSize(64 << 20).SetCalibratedSz(1024)
	delta := Options().SetMinSize(4096).SetMaxPercent(3.0)
	merged := base.Merge(delta)
	fmt.Printf("Merge 后: MinSize=%d（覆盖） MaxSize=%d（保留） MaxPercent=%.1f（覆盖） CalibratedSz=%d（保留）\n",
		*merged.MinSize, *merged.MaxSize, *merged.MaxPercent, *merged.CalibratedSz)
	if *merged.MinSize != 4096 || *merged.MaxPercent != 3.0 {
		t.Errorf("Merge 应覆盖 MinSize=4096 / MaxPercent=3.0，实际 %d / %f", *merged.MinSize, *merged.MaxPercent)
	}
	if *merged.MaxSize != 64<<20 || *merged.CalibratedSz != 1024 {
		t.Errorf("Merge 应保留 MaxSize=64MB / CalibratedSz=1024，实际 %d / %d", *merged.MaxSize, *merged.CalibratedSz)
	}

	// nil 接收者安全
	var nilOpt *Option
	nilOpt = nilOpt.SetMinSize(2048)
	fmt.Printf("nil 接收者 SetMinSize 后 MinSize=%d\n", *nilOpt.MinSize)
	if nilOpt == nil || *nilOpt.MinSize != 2048 {
		t.Errorf("nil 接收者 SetMinSize 失败")
	}
}

// ============ 模块 3：AdaptiveRingPool 伸缩 ============
// 理想步骤：miss 多（Get 不 Put）→ 扩容；hit 多（Get 立即 Put）→ 缩容。
// 预期：初始 32，miss 1000 次后扩容到 32×1.2=38，hit 1000 次后缩容到 max(38×0.8, 32)=32。
func TestVerifyAdaptiveRingScale(t *testing.T) {
	fmt.Printf("\n=== 模块3：AdaptiveRingPool 伸缩 ===\n")
	newCount := 0
	p := NewAdaptiveRingPoolWithLimit(32, 512, func() int {
		newCount++
		return newCount
	})
	fmt.Printf("初始 curCap=%d（minCap=32）\n", p.curCap)

	// 扩容：Get 1000 次不 Put（池空，全 miss）
	held := make([]int, 0, 1000)
	for i := 0; i < 1000; i++ {
		held = append(held, p.Get())
	}
	fmt.Printf("miss 1000 次后新建数=%d（应=1000，全 miss）\n", newCount)
	for i := 0; i < 1000; i++ {
		p.Put(held[i])
	}
	fmt.Printf("触发 autoScale 后 curCap=%d count=%d（应扩容到 38）\n", p.curCap, p.count)
	if p.curCap != 38 {
		t.Errorf("miss 后应扩容到 38，实际 %d", p.curCap)
	}

	// 缩容：Get/Put 循环 1000 次（池有对象，全 hit）
	for i := 0; i < 1000; i++ {
		p.Put(p.Get())
	}
	fmt.Printf("hit 1000 次后 curCap=%d count=%d（应缩容到 32，且 count 不越界）\n", p.curCap, p.count)
	if p.curCap != 32 {
		t.Errorf("hit 后应缩容到 32，实际 %d", p.curCap)
	}
	if p.count > p.curCap {
		t.Errorf("缩容后 count=%d 越界（> curCap=%d）", p.count, p.curCap)
	}
}

// ============ 模块 4：FixedRingPool 覆盖语义 ============
// 理想步骤：满时 Put 覆盖最旧（head），保持最新对象。
// 预期：capacity=3，Put 1,2,3 后 Put 4 覆盖最旧的 1，Get 顺序 = 2,3,4。
func TestVerifyFixedRingPool(t *testing.T) {
	fmt.Printf("\n=== 模块4：FixedRingPool 覆盖语义 ===\n")
	p := NewFixedRingPool(3, func() int { return -1 })
	p.Put(1)
	p.Put(2)
	p.Put(3) // 满
	p.Put(4) // 覆盖最旧的 1
	a := p.Get()
	b := p.Get()
	c := p.Get()
	fmt.Printf("Put 1,2,3,4 后 Get 顺序 = %d,%d,%d（期望 2,3,4，1 被覆盖）\n", a, b, c)
	if a != 2 || b != 3 || c != 4 {
		t.Errorf("覆盖语义错误：%d,%d,%d != 2,3,4", a, b, c)
	}

	// 空池 Get 走 New
	d := p.Get()
	fmt.Printf("空池 Get 走 New = %d（期望 -1）\n", d)
	if d != -1 {
		t.Errorf("空池应走 New 返回 -1，实际 %d", d)
	}
}

// ============ 模块 5：Pool 校准 + 智能丢弃 + len=0 ============
// 理想步骤：Get（len=0）→ 写满 size → Put（校准）；放 cap 远超水位的 buffer → 丢弃。
func TestVerifyPool(t *testing.T) {
	fmt.Printf("\n=== 模块5：Pool（len=0 + 校准收敛 + 智能丢弃）===\n")
	p := NewBytePool()
	b := p.Get()
	fmt.Printf("Get len=%d cap=%d（期望 len=0，cap=初始水位 max(512,1024)=1024）\n", len(b), cap(b))
	if len(b) != 0 {
		t.Errorf("Get len=%d != 0（resetFunc 返回值 bug）", len(b))
	}
	p.Put(b)

	// 校准收敛：写 3000（初始水位 1024 → 涨到 ~3000）
	for round := 0; round < 10; round++ {
		for i := 0; i < 1000; i++ {
			x := p.Get()
			if uint64(cap(x)) < 3000 {
				x = make([]byte, 0, 3000)
			}
			x = x[:3000]
			p.Put(x)
		}
		fmt.Printf("  写 3000 第%2d 次校准后 calibratedSz = %d\n", round+1, p.calibratedSz.Load())
	}
	sz := p.calibratedSz.Load()
	fmt.Printf("写 3000 收敛后 calibratedSz = %d（期望趋近 3000）\n", sz)
	if sz < 3000 || sz > 3100 {
		t.Errorf("写 3000 后 calibratedSz=%d，应收敛到 [3000, 3100]", sz)
	}

	// 智能丢弃：单独池，水位 1024，放 cap=2000（2000 > 1024×1.5=1536 → 丢弃）
	p2 := NewBytePool()
	nb := make([]byte, 0, 1024)
	nb = nb[:1024]
	p2.Put(nb) // 1024 <= 1536，不丢弃
	before := p2.pool.count
	big := make([]byte, 0, 2000)
	big = big[:2000]
	p2.Put(big) // 2000 > 1536，丢弃
	after := p2.pool.count
	fmt.Printf("丢弃: 放正常 1024 后 count=%d，放 2000 后 count=%d（应不变，2000 被丢弃）\n", before, after)
	if after != before {
		t.Errorf("cap=2000 应被丢弃（> 1024×1.5），count %d -> %d", before, after)
	}
}

// ============ 模块 6：ShardedPool 分片独立 ============
// 理想步骤：Get(key) → 写 → Put(key, v)，不同 key 独立校准、互不污染。
func TestVerifyShardedPool(t *testing.T) {
	fmt.Printf("\n=== 模块6：ShardedPool 分片独立 ===\n")
	sp := NewShardedBytePool(4)
	for i := 0; i < 20000; i++ {
		b0 := sp.Get(0)
		if uint64(cap(b0)) < 2<<10 {
			b0 = make([]byte, 0, 2<<10)
		}
		b0 = b0[:2<<10]
		sp.Put(0, b0)

		b1 := sp.Get(1)
		if uint64(cap(b1)) < 2<<20 {
			b1 = make([]byte, 0, 2<<20)
		}
		b1 = b1[:2<<20]
		sp.Put(1, b1)
	}
	p0 := sp.Get(0)
	p1 := sp.Get(1)
	fmt.Printf("key=0(2K) cap = %d\n", cap(p0))
	fmt.Printf("key=1(2M) cap = %d\n", cap(p1))
	sp.Put(0, p0)
	sp.Put(1, p1)
	if cap(p0) >= 64<<10 {
		t.Errorf("key=0 cap=%d 被 key=1 污染（应 < 64K）", cap(p0))
	}
	if cap(p1) < 2<<20 {
		t.Errorf("key=1 cap=%d 未校准（应 >= 2M）", cap(p1))
	}
}

// ============ 模块 7：ClassPool 分级归拢 + 自动分桶 ============
// 理想步骤：Get(size) → 写满 → Put（按 cap 归拢），小/大请求各自归到正确桶、误差 ≤2×。
func TestVerifyClassPool(t *testing.T) {
	fmt.Printf("\n=== 模块7：ClassPool 分级归拢 + 自动分桶 ===\n")
	cp := NewByteAutoClassPool()
	for i := 0; i < 30000; i++ {
		if i%10 == 0 {
			b := cp.Get(3 << 20) // 3M → roundUp 到 4M 桶
			if uint64(cap(b)) < 3<<20 {
				b = make([]byte, 0, 3<<20)
			}
			b = b[:3<<20]
			cp.Put(b)
		} else {
			b := cp.Get(3 << 10) // 3K → roundUp 到 4K 桶
			if uint64(cap(b)) < 3<<10 {
				b = make([]byte, 0, 3<<10)
			}
			b = b[:3<<10]
			cp.Put(b)
		}
	}
	small := cp.Get(3 << 10)
	large := cp.Get(3 << 20)
	fmt.Printf("3K 请求 cap = %d（应 4K，误差 %.2fx）\n", cap(small), float64(cap(small))/float64(3<<10))
	fmt.Printf("3M 请求 cap = %d（应 4M，误差 %.2fx）\n", cap(large), float64(cap(large))/float64(3<<20))
	cp.Put(small)
	cp.Put(large)
	if uint64(cap(small)) < 3<<10 || uint64(cap(small)) > 6<<10 {
		t.Errorf("3K cap=%d 超出 [3K, 6K]（误差 ≤2×）", cap(small))
	}
	if uint64(cap(large)) < 3<<20 || uint64(cap(large)) > 6<<20 {
		t.Errorf("3M cap=%d 超出 [3M, 6M]（误差 ≤2×）", cap(large))
	}
}
