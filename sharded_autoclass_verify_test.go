package buffer

import (
	"fmt"
	"sync"
	"testing"
)

// ========== 步骤 0：池结构 ==========
// 验证 NewShardedByteAutoClassPool 的分片数、档位数、自动分桶（roundUp）。
func TestShardedByteAutoClassPoolStructure(t *testing.T) {
	sp := NewShardedByteAutoClassPool(4)
	fmt.Printf("=== 步骤0：池结构 ===\n")
	fmt.Printf("分片数 = %d (mask=%d)\n", len(sp.shards), sp.mask)
	fmt.Printf("档位数 = %d\n", len(sp.shards[0].classes))
	fmt.Printf("roundUp 已设置 = %v\n", sp.shards[0].roundUp != nil)
	fmt.Printf("各档 [minSize, maxSize):\n")
	for i, c := range sp.shards[0].classes {
		fmt.Printf("  classes[%2d]: [%8d, %8d)\n", i, c.pool.minSize, c.maxSize)
	}
	if len(sp.shards) != 4 {
		t.Errorf("分片数应=4（shardCount=4 已为 2 的幂），实际 %d", len(sp.shards))
	}
	// 512B~64MB 按 2 的幂：分界 [1024,2048,...,32MB] 共 16 个，NewClassPool 生成 16 档 + 末档 = 17 档
	if len(sp.shards[0].classes) != 17 {
		t.Errorf("档位数应=17，实际 %d", len(sp.shards[0].classes))
	}
	if sp.shards[0].roundUp == nil {
		t.Error("自动分桶应设置 roundUp")
	}
}

// ========== 步骤 1-2：Get 语义 + 自动分桶路由 ==========
// 按理想步骤走：Get → 写满 size（模拟网络读入）→ Put，观察稳定 cap 是否落在正确桶、误差 ≤2×。
func TestShardedByteAutoClassPoolGetAndRoute(t *testing.T) {
	sp := NewShardedByteAutoClassPool(4)
	fmt.Printf("\n=== 步骤1-2：Get 语义 + 自动分桶路由 ===\n")
	fmt.Printf("%-10s %-10s %-10s %-8s %s\n", "size", "roundUp", "cap", "误差", "len")
	cases := []uint64{512, 1000, 2048, 2049, 3000, 4096, 100 << 10, 3 << 20}
	for _, size := range cases {
		// 预热到稳定：写满 size，让校准收敛（size <= MinSize 时 cap 停在 MinSize）
		for i := 0; i < 3000; i++ {
			b := sp.Get(0, size)
			if uint64(cap(b)) < size {
				b = make([]byte, 0, size)
			}
			b = b[:size] // 扩展 len 模拟「写满 size」，Put 时 used = len = size
			sp.Put(0, b)
		}
		probe := sp.Get(0, size)
		r := roundUpPow2(size)
		ratio := float64(cap(probe)) / float64(size)
		fmt.Printf("%-10d %-10d %-10d %-8.2fx %d\n", size, r, cap(probe), ratio, len(probe))
		sp.Put(0, probe)
		// 断言 1：Get 返回 len=0（make([]byte, 0, size) 的语义，resetFunc 也是 b[:0]）
		if len(probe) != 0 {
			t.Errorf("Get(%d) 应返回 len=0，实际 %d", size, len(probe))
		}
		// 断言 2：cap >= size（够用）且 cap <= 2*size（自动分桶误差 ≤2× 的承诺）
		if uint64(cap(probe)) < size || uint64(cap(probe)) > size*2 {
			t.Errorf("Get(%d) cap=%d 超出误差 2 倍范围 [%d, %d]", size, cap(probe), size, size*2)
		}
	}
}

// ========== 步骤 3：校准收敛（EMA 快涨） ==========
// 写 3000 到 [2048,4096) 档（MinSize=2048，初始水位 2048），观察 calibratedSz 从 2048 快涨到 ~3000。
// 预期（EMA 公式推导）：
//
//	第1次校准 2048*0.4+3000*0.6=2619.2 → ×1.05=2750
//	第2次校准 2750*0.4+3000*0.6=2900   → ×1.05=3045
//	第3次起 newMax(3000)<=oldSz → 下跌分支 0.8/0.2 缓慢逼近 3000
//
// 注意：这里直接读 calibratedSz（新建 buffer 时用的 size），而非 Get 返回的 cap——
// Get 复用 ring 里已有 buffer 时返回的是「上次 make 的实际 cap」，串行复用下恒为 3000，看不到水位变化。
func TestShardedByteAutoClassPoolCalibrateUp(t *testing.T) {
	sp := NewShardedByteAutoClassPool(4)
	fmt.Printf("\n=== 步骤3：校准收敛（写 3000 到 2048 档，EMA 快涨）===\n")
	key := uint32(0)
	p := sp.shards[0].classes[2].pool // classes[2] = [2048, 4096)
	fmt.Printf("初始 calibratedSz = %d（max(2048, 1024)=2048）\n", p.calibratedSz.Load())

	for round := 0; round < 10; round++ {
		for i := 0; i < 1000; i++ {
			b := sp.Get(key, 2048)
			if uint64(cap(b)) < 3000 {
				b = make([]byte, 0, 3000)
			}
			b = b[:3000]
			sp.Put(key, b)
		}
		fmt.Printf("  第%2d 次校准后 calibratedSz = %d\n", round+1, p.calibratedSz.Load())
	}
	sz := p.calibratedSz.Load()
	fmt.Printf("收敛后 calibratedSz = %d（期望趋近 3000）\n", sz)
	if sz < 3000 || sz > 3100 {
		t.Errorf("写 3000 后 calibratedSz=%d，应收敛到 [3000, 3100]", sz)
	}
}

// ========== 步骤 4：缩容（EMA 慢跌 + minSize 边界） ==========
// 关键陷阱验证：缩容依赖「used > minSize」的采样。
//   - 写 2048（== MinSize）：used 不大于 minSize，不采样 → 不缩容（calibratedSz 停在 ~3000）
//   - 写 2049（>  MinSize）：采样 → 缩容，calibratedSz 缓慢回落到 ~2049
func TestShardedByteAutoClassPoolShrink(t *testing.T) {
	sp := NewShardedByteAutoClassPool(4)
	fmt.Printf("\n=== 步骤4：缩容（EMA 慢跌 + minSize 边界陷阱）===\n")
	key := uint32(0)
	p := sp.shards[0].classes[2].pool

	// 先涨到 ~3000
	for i := 0; i < 10000; i++ {
		b := sp.Get(key, 2048)
		if uint64(cap(b)) < 3000 {
			b = make([]byte, 0, 3000)
		}
		b = b[:3000]
		sp.Put(key, b)
	}
	fmt.Printf("写 3000 后 calibratedSz = %d\n", p.calibratedSz.Load())

	// 写 2048（== MinSize）：不应缩容
	for i := 0; i < 5000; i++ {
		b := sp.Get(key, 2048)
		b = b[:2048]
		sp.Put(key, b)
	}
	fmt.Printf("写 2048（==MinSize）后 calibratedSz = %d（不缩容：used 不大于 minSize 不采样）\n", p.calibratedSz.Load())

	// 写 2049（> MinSize）：触发缩容，缓慢回落
	for round := 0; round < 10; round++ {
		for i := 0; i < 1000; i++ {
			b := sp.Get(key, 2048)
			b = b[:2049]
			sp.Put(key, b)
		}
		fmt.Printf("  写 2049 第%2d 次校准后 calibratedSz = %d\n", round+1, p.calibratedSz.Load())
	}
	sz := p.calibratedSz.Load()
	fmt.Printf("写 2049 后 calibratedSz = %d（期望缓慢趋近 2049）\n", sz)
	if sz > 2200 {
		t.Errorf("写 2049 后 calibratedSz=%d，应回落到 [2049, 2200]", sz)
	}
}

// ========== 步骤 5：多峰流量不污染 ==========
// 同一 key 混合 2K 与 2M 流量，两档独立校准，小包不被大包污染。
func TestShardedByteAutoClassPoolMultiPeak(t *testing.T) {
	sp := NewShardedByteAutoClassPool(4)
	fmt.Printf("\n=== 步骤5：多峰流量不污染（同 key 混合 2K + 2M）===\n")
	key := uint32(0)
	for i := 0; i < 30000; i++ {
		if i%10 == 0 {
			b := sp.Get(key, 2<<20)
			if uint64(cap(b)) < 2<<20 {
				b = make([]byte, 0, 2<<20)
			}
			b = b[:2<<20]
			sp.Put(key, b)
		} else {
			b := sp.Get(key, 2<<10)
			if uint64(cap(b)) < 2<<10 {
				b = make([]byte, 0, 2<<10)
			}
			b = b[:2<<10]
			sp.Put(key, b)
		}
	}
	small := sp.Get(key, 2<<10)
	large := sp.Get(key, 2<<20)
	fmt.Printf("小包(2K) cap = %d\n", cap(small))
	fmt.Printf("大包(2M) cap = %d\n", cap(large))
	sp.Put(key, small)
	sp.Put(key, large)
	if cap(small) >= 64<<10 {
		t.Errorf("小包 cap=%d 被大包污染（应 < 64K）", cap(small))
	}
	if cap(large) < 2<<20 {
		t.Errorf("大包 cap=%d 未校准（应 >= 2M）", cap(large))
	}
}

// ========== 步骤 6：分片独立校准 ==========
// key=0 走小流量、key=1 走大流量，两个分片各自独立校准、互不污染。
func TestShardedByteAutoClassPoolShardIsolation(t *testing.T) {
	sp := NewShardedByteAutoClassPool(4)
	fmt.Printf("\n=== 步骤6：分片独立校准（key=0 小流量，key=1 大流量）===\n")
	for i := 0; i < 20000; i++ {
		b0 := sp.Get(0, 2<<10)
		if uint64(cap(b0)) < 2<<10 {
			b0 = make([]byte, 0, 2<<10)
		}
		b0 = b0[:2<<10]
		sp.Put(0, b0)

		b1 := sp.Get(1, 2<<20)
		if uint64(cap(b1)) < 2<<20 {
			b1 = make([]byte, 0, 2<<20)
		}
		b1 = b1[:2<<20]
		sp.Put(1, b1)
	}
	p0 := sp.Get(0, 2<<10)
	p1 := sp.Get(1, 2<<20)
	fmt.Printf("key=0(2K) cap = %d\n", cap(p0))
	fmt.Printf("key=1(2M) cap = %d\n", cap(p1))
	sp.Put(0, p0)
	sp.Put(1, p1)
	if cap(p0) >= 64<<10 {
		t.Errorf("key=0 cap=%d 被 key=1 污染", cap(p0))
	}
	if cap(p1) < 2<<20 {
		t.Errorf("key=1 cap=%d 未校准", cap(p1))
	}
}

// ========== 步骤 7：智能丢弃 ==========
// 放一个 cap 远超「水位×1.5」的 buffer，应被丢弃（不占池）。
func TestShardedByteAutoClassPoolDiscard(t *testing.T) {
	sp := NewShardedByteAutoClassPool(4)
	fmt.Printf("\n=== 步骤7：智能丢弃 ===\n")
	// 预热 [2048,4096) 档（classes[2]，MinSize=2048，初始水位 2048）：放 2048 的 buffer（2048 <= 2048*1.5 不丢弃）
	for i := 0; i < 10; i++ {
		b := make([]byte, 0, 2048)
		b = b[:2048]
		sp.Put(0, b)
	}
	c := sp.shards[0].classes[2].pool.pool
	before := c.count
	fmt.Printf("预热后 2048 档空闲数 = %d\n", before)
	// 放 3500 的 buffer：3500 > 2048*1.5=3072，应丢弃
	big := make([]byte, 0, 3500)
	big = big[:3500]
	sp.Put(0, big)
	after := c.count
	fmt.Printf("放 3500 后空闲数 = %d（应不变，因为被丢弃）\n", after)
	if after != before {
		t.Errorf("3500 buffer 应被丢弃（3500 > 2048×1.5），空闲数 %d -> %d", before, after)
	}
}

// ========== 步骤 8：Put(nil) 安全 ==========
func TestShardedByteAutoClassPoolNilPut(t *testing.T) {
	sp := NewShardedByteAutoClassPool(4)
	sp.Put(0, nil) // 不应 panic
	sp.Put(0, []byte(nil))
}

// ========== 步骤 9：并发安全（每 goroutine 固定 key + 混合 size） ==========
// 需配合 go test -race 验证无数据竞争。
func TestShardedByteAutoClassPoolConcurrent(t *testing.T) {
	sp := NewShardedByteAutoClassPool(16)
	var wg sync.WaitGroup
	for w := 0; w < 32; w++ {
		wg.Add(1)
		go func(id uint32) {
			defer wg.Done()
			for i := 0; i < 2000; i++ {
				size := uint64(1 << ((i + int(id)) % 16)) // 1B ~ 32KB 混合
				b := sp.Get(id, size)
				if uint64(cap(b)) < size {
					b = make([]byte, 0, size)
				}
				b = b[:size]
				sp.Put(id, b)
			}
		}(uint32(w))
	}
	wg.Wait()
}
