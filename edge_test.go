package buffer

import (
	"bytes"
	"testing"
)

// AdaptiveRingPool 容量边界：minCap < 1、maxCap < minCap 的处理
func TestAdaptiveRingCapBoundary(t *testing.T) {
	// minCap < 1 → 1
	p := NewAdaptiveRingPoolWithLimit(0, 512, func() *bytes.Buffer { return &bytes.Buffer{} })
	if p.minCap != 1 || p.maxCap != 512 || p.curCap != 1 {
		t.Fatalf("minCap<1 应 minCap=1，实际 minCap=%d maxCap=%d curCap=%d", p.minCap, p.maxCap, p.curCap)
	}
	// maxCap < minCap → maxCap = minCap
	p2 := NewAdaptiveRingPoolWithLimit(100, 50, func() *bytes.Buffer { return &bytes.Buffer{} })
	if p2.minCap != 100 || p2.maxCap != 100 {
		t.Fatalf("maxCap<minCap 应 maxCap=minCap，实际 minCap=%d maxCap=%d", p2.minCap, p2.maxCap)
	}
	// 正常配置
	p3 := NewAdaptiveRingPoolWithLimit(32, 512, func() *bytes.Buffer { return &bytes.Buffer{} })
	if p3.minCap != 32 || p3.maxCap != 512 || p3.curCap != 32 {
		t.Fatalf("正常配置应 minCap=32 maxCap=512 curCap=32，实际 %d/%d/%d", p3.minCap, p3.maxCap, p3.curCap)
	}
}

// ShardedPool 分片数：向上取整到 2 的幂、clamp 到 1024、默认 GOMAXPROCS
func TestShardedPoolShardCount(t *testing.T) {
	// 5 → 向上取整到 8
	sp := NewShardedBufferPool(5)
	if len(sp.shards) != 8 {
		t.Fatalf("shardCount=5 应向上取整到 8，实际 %d", len(sp.shards))
	}
	// 1 → 1
	sp1 := NewShardedBufferPool(1)
	if len(sp1.shards) != 1 {
		t.Fatalf("shardCount=1 应 1 分片，实际 %d", len(sp1.shards))
	}
	// 0 → GOMAXPROCS（至少 1）
	sp0 := NewShardedBufferPool(0)
	if len(sp0.shards) < 1 {
		t.Fatal("shardCount=0 应至少 1 分片")
	}
	// 超大 → clamp 到 1024
	spBig := NewShardedBufferPool(100000)
	if len(spBig.shards) != 1024 {
		t.Fatalf("超大 shardCount 应 clamp 到 1024，实际 %d", len(spBig.shards))
	}
	// 所有分片数都是 2 的幂
	for _, s := range []*ShardedPool[*bytes.Buffer]{sp, sp1, sp0, spBig} {
		n := len(s.shards)
		if n&(n-1) != 0 {
			t.Fatalf("分片数 %d 不是 2 的幂", n)
		}
	}
}

// roundUpPow2 正确性
func TestRoundUpPow2(t *testing.T) {
	cases := []struct{ in, want uint64 }{
		{0, 1}, {1, 1}, {2, 2}, {3, 4}, {31, 32}, {32, 32}, {33, 64},
		{100, 128}, {500, 512}, {512, 512}, {1000, 1024},
	}
	for _, c := range cases {
		if got := roundUpPow2(c.in); got != c.want {
			t.Errorf("roundUpPow2(%d)=%d, want %d", c.in, got, c.want)
		}
	}
}

// FixedRingPool 容量边界：capacity < 1
func TestFixedRingPoolCapBoundary(t *testing.T) {
	p := NewFixedRingPool(0, func() *bytes.Buffer { return &bytes.Buffer{} })
	if p.capacity != 1 {
		t.Fatalf("capacity<1 应为 1，实际 %d", p.capacity)
	}
	b := p.Get()
	if b == nil {
		t.Fatal("capacity=1 时 Get 返回 nil")
	}
	p.Put(b)
}
