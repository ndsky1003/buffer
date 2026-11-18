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

// SetEvict 必须传播到所有分片：每片各自在满时淘汰，回调都能收到
func TestShardedFixedRingPoolSetEvict(t *testing.T) {
	sp := newTestBytePool(8, 2) // 8 片，每片 cap=2
	var mu sync.Mutex
	n := 0
	sp.SetEvict(func(b []byte) {
		if b == nil {
			t.Error("evict 收到 nil 对象")
		}
		mu.Lock()
		n++
		mu.Unlock()
	})

	for i := 0; i < 10; i++ {
		sp.Put(1, []byte("a")) // 同一分片，cap=2 → 淘汰 8 个
	}
	mu.Lock()
	got := n
	mu.Unlock()
	if got != 8 {
		t.Fatalf("淘汰 %d，期望 8（单片 cap=2、Put 10 个）", got)
	}

	for i := 0; i < 10; i++ {
		sp.Put(2, []byte("b")) // 换 key → 另一分片，回调同样生效
	}
	mu.Lock()
	got = n
	mu.Unlock()
	if got != 16 {
		t.Fatalf("累计淘汰 %d，期望 16（SetEvict 须作用于所有分片）", got)
	}
}

// SetEvict 返回自身：支持「构造即登记」的链式写法，且对所有分片生效
func TestShardedFixedRingPoolSetEvictChaining(t *testing.T) {
	n := 0
	sp := NewShardedFixedRingPool(8, 2, func() *bytes.Buffer { return &bytes.Buffer{} }).
		SetEvict(func(b *bytes.Buffer) { n++ })

	for i := 0; i < 10; i++ {
		sp.Put(1, &bytes.Buffer{}) // 分片 cap=2 → 淘汰 8 个
	}
	if n != 8 {
		t.Fatalf("链式登记的回调未生效：淘汰 %d，期望 8", n)
	}
	if got := sp.SetEvict(nil); got != sp {
		t.Fatal("SetEvict(nil) 应返回接收者自身")
	}
}

// 分片池：已取出的对象不会被 evict（各片独立计数）
func TestShardedFixedRingPoolEvictSkipsInUseObject(t *testing.T) {
	sp := newTestBufPool(8, 2)
	var evicted []*bytes.Buffer
	sp.SetEvict(func(b *bytes.Buffer) { evicted = append(evicted, b) })

	h1, h2, h3 := &bytes.Buffer{}, &bytes.Buffer{}, &bytes.Buffer{}
	sp.Put(1, h1)
	sp.Put(1, h2) // 该片满
	inUse := sp.Get(1)
	sp.Put(1, h3) // 未满：覆盖 h1 的残留槽位引用，不应淘汰
	if len(evicted) != 0 {
		t.Fatalf("未满时不应淘汰，实际 %d 个", len(evicted))
	}
	if inUse != h1 {
		t.Fatal("Get 应取回 h1")
	}
	h4 := &bytes.Buffer{}
	sp.Put(1, h4) // 满 → 淘汰空闲的 h2
	if len(evicted) != 1 || evicted[0] != h2 {
		t.Fatalf("淘汰 %v，期望仅 h2（使用中的对象不得被淘汰）", evicted)
	}
}
