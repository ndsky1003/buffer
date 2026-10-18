package buffer

import (
	"bytes"
	"sync"
	"testing"
)

// 基本 Get/Put（同一 key）
func TestShardedClassPoolBasic(t *testing.T) {
	sp := NewShardedByteClassPool(4, []uint64{64 << 10})
	b := sp.Get(1, 2<<10)
	if b == nil {
		t.Fatal("Get 返回 nil")
	}
	sp.Put(1, b)
}

// 分片隔离：不同 key 落到不同分片，互不污染
func TestShardedClassPoolShardIsolation(t *testing.T) {
	sp := NewShardedByteClassPool(8, []uint64{64 << 10})
	for i := 0; i < 1000; i++ {
		b0 := sp.Get(0, 1024)
		sp.Put(0, b0)
		b1 := sp.Get(1, 1024)
		sp.Put(1, b1)
	}
	// 两个分片的第一档（<64K，1024 归拢到此档）ring 都应有对象（各自独立）
	s0 := sp.shards[0].classes[0].pool.pool
	s1 := sp.shards[1].classes[0].pool.pool
	if s0.count == 0 || s1.count == 0 {
		t.Fatalf("分片隔离失败：shard0.count=%d shard1.count=%d", s0.count, s1.count)
	}
}

// 分级归拢：key 分离大小流量，各自归到不同档位、互不污染
func TestShardedClassPoolClassify(t *testing.T) {
	sp := NewShardedByteClassPool(4, []uint64{64 << 10}) // 两档：<64K、>=64K
	for i := 0; i < 20000; i++ {
		// key=0 走小流量 2K
		sb := sp.Get(0, 2<<10)
		if uint64(cap(sb)) < 2<<10 {
			sb = make([]byte, 0, 2<<10)
		}
		sb = sb[:2<<10]
		sp.Put(0, sb)

		// key=1 走大流量 2M
		lb := sp.Get(1, 2<<20)
		if uint64(cap(lb)) < 2<<20 {
			lb = make([]byte, 0, 2<<20)
		}
		lb = lb[:2<<20]
		sp.Put(1, lb)
	}
	sb := sp.Get(0, 2<<10)
	lb := sp.Get(1, 2<<20)
	sc, lc := cap(sb), cap(lb)
	sp.Put(0, sb)
	sp.Put(1, lb)
	if sc >= 64<<10 {
		t.Errorf("key=0 小流量 buffer cap=%d，应 < 64K（被大流量污染）", sc)
	}
	if lc < 64<<10 {
		t.Errorf("key=1 大流量 buffer cap=%d，应 >= 64K", lc)
	}
}

// 自动分桶：非 2 的幂请求向上取整，误差 ≤2×
func TestShardedClassPoolAuto(t *testing.T) {
	sp := NewShardedByteAutoClassPool(4)
	for i := 0; i < 8000; i++ {
		b := sp.Get(0, 3<<10) // 3K → 向上取整到 4K 桶
		if uint64(cap(b)) < 3<<10 {
			b = make([]byte, 0, 3<<10)
		}
		b = b[:3<<10]
		sp.Put(0, b)
	}
	probe := sp.Get(0, 3<<10)
	c := uint64(cap(probe))
	sp.Put(0, probe)
	if c < 3<<10 || c > 3<<10*2 {
		t.Errorf("请求 3K 归拢到桶 cap=%d，应在 [3K, 6K] 内", c)
	}
}

// 分片数：向上取整到 2 的幂、clamp 到 1024
func TestShardedClassPoolShardCount(t *testing.T) {
	sp := NewShardedBufferClassPool(5, nil) // 5 → 8
	if len(sp.shards) != 8 {
		t.Fatalf("shardCount=5 应向上取整到 8，实际 %d", len(sp.shards))
	}
	spBig := NewShardedBufferClassPool(100000, nil) // clamp 到 1024
	if len(spBig.shards) != 1024 {
		t.Fatalf("超大 shardCount 应 clamp 到 1024，实际 %d", len(spBig.shards))
	}
	// 自动分桶同样处理
	spAuto := NewShardedByteAutoClassPool(3) // 3 → 4
	if len(spAuto.shards) != 4 {
		t.Fatalf("shardCount=3 应向上取整到 4，实际 %d", len(spAuto.shards))
	}
}

// Put(nil) 安全
func TestShardedClassPoolNilPut(t *testing.T) {
	sp := NewShardedByteClassPool(4, []uint64{64 << 10})
	sp.Put(0, nil)
	spBuf := NewShardedBufferClassPool(4, []uint64{64 << 10})
	spBuf.Put(0, nil)
	spBuf.Put(0, (*bytes.Buffer)(nil))
}

// 并发安全：每 goroutine 固定 key（模拟每连接一分片）+ 混合大小（多峰流量）
func TestShardedClassPoolConcurrent(t *testing.T) {
	sp := NewShardedByteAutoClassPool(16)
	var wg sync.WaitGroup
	for w := 0; w < 32; w++ {
		wg.Add(1)
		go func(id uint32) {
			defer wg.Done()
			for i := 0; i < 2000; i++ {
				size := uint64(1 << ((i + int(id)) % 16)) // 1B ~ 32KB 混合
				if size < 1 {
					size = 1
				}
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
