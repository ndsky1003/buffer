package buffer

import (
	"bytes"
	"sync"
	"testing"
)

// newTestBufPool 测试辅助：创建 *bytes.Buffer 分片固定容量池（初始 1KB）。
func newTestBufPool(shardCount, capacity int) *ShardedFixedRingPool[*bytes.Buffer] {
	return NewShardedFixedRingPool(shardCount, capacity, func() *bytes.Buffer {
		return bytes.NewBuffer(make([]byte, 0, 1024))
	})
}

// newTestBytePool 测试辅助：创建 []byte 分片固定容量池（初始 1KB）。
func newTestBytePool(shardCount, capacity int) *ShardedFixedRingPool[[]byte] {
	return NewShardedFixedRingPool(shardCount, capacity, func() []byte {
		return make([]byte, 0, 1024)
	})
}

// 基本 Get/Put（同一 key）
func TestShardedFixedRingPoolBasic(t *testing.T) {
	sp := newTestBufPool(4, 8)
	b := sp.Get(1)
	if b == nil {
		t.Fatal("Get 返回 nil")
	}
	b.Reset()
	b.WriteString("hello")
	sp.Put(1, b)
}

// 分片隔离：不同 key 路由到不同分片，各自独立
func TestShardedFixedRingPoolShardIsolation(t *testing.T) {
	sp := newTestBufPool(8, 8)
	for i := 0; i < 100; i++ {
		b0 := sp.Get(0)
		b0.Reset()
		sp.Put(0, b0)
		b1 := sp.Get(1)
		b1.Reset()
		sp.Put(1, b1)
	}
	if sp.shards[0].count == 0 || sp.shards[1].count == 0 {
		t.Fatalf("分片隔离失败：shard0.count=%d shard1.count=%d", sp.shards[0].count, sp.shards[1].count)
	}
}

// 并发安全：每个 goroutine 固定 key（模拟每连接一个分片）
func TestShardedFixedRingPoolConcurrent(t *testing.T) {
	sp := newTestBufPool(8, 16)
	var wg sync.WaitGroup
	for w := 0; w < 32; w++ {
		wg.Add(1)
		go func(id uint32) {
			defer wg.Done()
			for i := 0; i < 5000; i++ {
				b := sp.Get(id)
				b.Reset()
				b.WriteString("x")
				sp.Put(id, b)
			}
		}(uint32(w))
	}
	wg.Wait()
}

// 无自动 reset：复用同一对象，内容保留（调用方须自行 Reset）
func TestShardedFixedRingPoolNoAutoReset(t *testing.T) {
	sp := newTestBufPool(4, 8)
	b := sp.Get(0)
	b.Reset()
	b.WriteString("hello")
	sp.Put(0, b)

	b2 := sp.Get(0)
	if b2 != b {
		t.Fatal("应复用同一对象")
	}
	if b2.String() != "hello" {
		t.Fatalf("Get 未自动 reset，内容=%q 期望 hello", b2.String())
	}
	b2.Reset()
	sp.Put(0, b2)
}

// []byte 归还须 b[:0]，否则下次复用 len 非 0（zero-copy 大忌）
func TestShardedFixedBytePoolReslice(t *testing.T) {
	sp := newTestBytePool(4, 8)
	b := sp.Get(0)
	b = append(b, 'a', 'b', 'c')
	sp.Put(0, b[:0])

	b2 := sp.Get(0)
	if len(b2) != 0 {
		t.Fatalf("Get 复用 slice len=%d，期望 0（归还时应传 b[:0]）", len(b2))
	}
	sp.Put(0, b2[:0])
}

// 满时覆盖最旧：每片容量固定，不无限增长
func TestShardedFixedRingPoolCapacityPerShard(t *testing.T) {
	const cap = 8
	sp := newTestBufPool(4, cap)
	for i := 0; i < cap*2; i++ {
		sp.Put(0, &bytes.Buffer{})
	}
	if sp.shards[0].count != cap {
		t.Fatalf("shard0.count=%d 期望 %d（固定容量）", sp.shards[0].count, cap)
	}
}
