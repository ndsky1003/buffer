package buffer

import (
	"sync"
	"testing"
)

// 基本 Get/Put（同一 key）
func TestShardedPoolBasic(t *testing.T) {
	sp := NewShardedBufferPool(4)
	b := sp.Get(1)
	if b == nil {
		t.Fatal("Get 返回 nil")
	}
	b.WriteString("hello")
	sp.Put(1, b)
}

// 不同 key 路由到不同分片，对象不漂移（同一 key 的 Get/Put 落在同一分片）
func TestShardedPoolShardIsolation(t *testing.T) {
	sp := NewShardedBufferPool(8)
	// 预热：key 0 和 key 1 各自独立复用
	for i := 0; i < 1000; i++ {
		b0 := sp.Get(0)
		sp.Put(0, b0)
		b1 := sp.Get(1)
		sp.Put(1, b1)
	}
	// 两个分片的 ring 都应该有对象（各自独立，不互相污染）
	s0 := sp.shards[0].pool
	s1 := sp.shards[1].pool
	if s0.count == 0 || s1.count == 0 {
		t.Fatalf("分片隔离失败：shard0.count=%d shard1.count=%d", s0.count, s1.count)
	}
}

// 并发安全：每个 goroutine 固定 key（模拟每连接一个分片）
func TestShardedPoolConcurrent(t *testing.T) {
	sp := NewShardedBufferPool(8)
	var wg sync.WaitGroup
	for w := 0; w < 32; w++ {
		wg.Add(1)
		go func(id uint32) {
			defer wg.Done()
			for i := 0; i < 5000; i++ {
				b := sp.Get(id)
				b.WriteString("x")
				sp.Put(id, b)
			}
		}(uint32(w))
	}
	wg.Wait()
}
