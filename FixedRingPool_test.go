package buffer

import (
	"bytes"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 基本 Get/Put
func TestFixedRingPoolBasic(t *testing.T) {
	p := NewFixedRingPool(8, func() *bytes.Buffer { return &bytes.Buffer{} })
	b := p.Get()
	if b == nil {
		t.Fatal("Get 返回 nil")
	}
	p.Put(b)
}

// 容量固定：放入超过容量的对象，多余的覆盖最旧，count 始终不超过 capacity
func TestFixedRingPoolCapacity(t *testing.T) {
	const cap = 8
	p := NewFixedRingPool(cap, func() *bytes.Buffer { return &bytes.Buffer{} })
	for i := 0; i < cap*2; i++ {
		p.Put(&bytes.Buffer{})
	}
	if p.count != cap {
		t.Fatalf("count=%d want %d", p.count, cap)
	}
}

// 满时覆盖最旧：淘汰 head 指向的对象，保持最新
func TestFixedRingPoolOverwrite(t *testing.T) {
	p := NewFixedRingPool(4, func() *bytes.Buffer { return &bytes.Buffer{} })
	bufs := make([]*bytes.Buffer, 4)
	for i := 0; i < 4; i++ {
		bufs[i] = &bytes.Buffer{}
		p.Put(bufs[i])
	}
	// 满时 Put 第 5 个，覆盖最旧的 bufs[0]
	p.Put(&bytes.Buffer{})
	// Get 应拿到次旧的 bufs[1]（bufs[0] 已被覆盖）
	if b := p.Get(); b != bufs[1] {
		t.Fatalf("覆盖后 Get 拿到 %v，期望 bufs[1]（bufs[0] 应已被覆盖）", b)
	}
}

// 覆盖保持最新：流量突变时，池子能滚动更新到大容量 buffer
func TestFixedRingPoolKeepLatest(t *testing.T) {
	const cap = 64
	const big = 1 << 20 // 1MB
	p := NewFixedRingPool(cap, func() *bytes.Buffer { return bytes.NewBuffer(make([]byte, 0, 1024)) })
	// 预热满 64 个 1KB buffer
	for i := 0; i < cap; i++ {
		p.Put(bytes.NewBuffer(make([]byte, 0, 1024)))
	}
	// 突变：连续 Put 100 个 1MB buffer（归还 > 取走）
	for i := 0; i < 100; i++ {
		p.Put(bytes.NewBuffer(make([]byte, 0, big)))
	}
	// 池子里应已全部更新为大 buffer
	n := 0
	for _, b := range p.buffer {
		if b.Cap() >= big {
			n++
		}
	}
	if n != cap {
		t.Fatalf("覆盖后大 buffer 数量=%d，期望 %d（覆盖应让池子保持最新）", n, cap)
	}
}

// 池空时走 New，不会返回 nil
func TestFixedRingPoolEmptyNew(t *testing.T) {
	p := NewFixedRingPool(2, func() *bytes.Buffer { return &bytes.Buffer{} })
	for i := 0; i < 5; i++ {
		if b := p.Get(); b == nil {
			t.Fatal("池空时 Get 返回 nil")
		}
	}
}

// 并发安全
func TestFixedRingPoolConcurrent(t *testing.T) {
	p := NewFixedRingPool(64, func() *bytes.Buffer { return &bytes.Buffer{} })
	var wg sync.WaitGroup
	for w := 0; w < 32; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 5000; i++ {
				b := p.Get()
				p.Put(b)
			}
		}()
	}
	wg.Wait()
}

// =============================================================================
// SetEvict：淘汰回调（池化需要显式释放的资源时用）
// =============================================================================

// 模拟需要显式释放的资源（fd / 连接 / 外部句柄）
type testHandle struct{ id int }

// 池满覆盖最旧时，被淘汰的对象按「最旧优先」顺序交给回调
func TestFixedRingPoolEvictOnOverwrite(t *testing.T) {
	p := NewFixedRingPool(3, func() int { return -1 })
	var evicted []int
	p.SetEvict(func(v int) { evicted = append(evicted, v) })

	for i := 1; i <= 6; i++ { // capacity=3，Put 6 个 → 最旧的 1,2,3 被淘汰
		p.Put(i)
	}
	want := []int{1, 2, 3}
	if len(evicted) != len(want) {
		t.Fatalf("淘汰 %v，期望 %v", evicted, want)
	}
	for i := range want {
		if evicted[i] != want[i] {
			t.Fatalf("淘汰顺序 %v，期望 %v（应最旧优先）", evicted, want)
		}
	}
	// 池内应保留最新的 4,5,6
	for i := 0; i < 3; i++ {
		if got := p.Get(); got != 4+i {
			t.Fatalf("Get=%d，期望 %d（覆盖后池内应保留最新对象）", got, 4+i)
		}
	}
}

// 未满时不触发淘汰；正在使用中（已 Get 未 Put）的对象不会被误淘汰
func TestFixedRingPoolEvictSkipsInUseObject(t *testing.T) {
	p := NewFixedRingPool(2, func() *testHandle { return &testHandle{id: -1} })
	var evicted []*testHandle
	p.SetEvict(func(h *testHandle) { evicted = append(evicted, h) })

	h1, h2, h3, h4 := &testHandle{id: 1}, &testHandle{id: 2}, &testHandle{id: 3}, &testHandle{id: 4}
	p.Put(h1)        // slot0
	p.Put(h2)        // slot1，满
	inUse := p.Get() // 取走 h1（槽位仍残留它的引用）
	p.Put(h3)        // 未满：覆盖 h1 的残留引用，不应触发 evict
	if len(evicted) != 0 {
		t.Fatalf("未满时不应淘汰对象，实际淘汰了 %d 个", len(evicted))
	}
	p.Put(h4) // 满 → 淘汰空闲的最旧对象 h2
	if inUse != h1 {
		t.Fatalf("Get 拿到 %v，期望 h1", inUse)
	}
	if len(evicted) != 1 || evicted[0] != h2 {
		t.Fatalf("淘汰 %v，期望仅 h2（使用中的对象不得被淘汰）", evicted)
	}
}

// 回调内回用本池的 Get/Put 不应死锁（evict 必须在锁外调用）
func TestFixedRingPoolEvictMayReenterPool(t *testing.T) {
	p := NewFixedRingPool(2, func() int { return 0 })
	done := make(chan struct{}, 8)
	p.SetEvict(func(v int) {
		p.Put(p.Get()) // 再入本池：若 evict 在锁内调用则死锁
		done <- struct{}{}
	})
	for i := 1; i <= 5; i++ {
		p.Put(i)
	}
	for i := 0; i < 3; i++ { // capacity=2，Put 5 个 → 淘汰 3 次
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("evict 内调用 Get/Put 死锁（evict 应在锁外调用）")
		}
	}
}

// SetEvict(nil) 清除回调：之后覆盖不再调用，也不 panic
func TestFixedRingPoolEvictUnset(t *testing.T) {
	p := NewFixedRingPool(2, func() int { return 0 })
	n := 0
	p.SetEvict(func(int) { n++ })
	p.SetEvict(nil)
	for i := 1; i <= 6; i++ {
		p.Put(i) // 覆盖最旧，但回调已清除 → 直接交给 GC
	}
	if n != 0 {
		t.Fatalf("清除回调后仍被调用 %d 次", n)
	}
}

// SetEvict 返回自身：支持「构造即登记」的链式写法
func TestFixedRingPoolSetEvictChaining(t *testing.T) {
	p := NewFixedRingPool(2, func() int { return 0 })
	if got := p.SetEvict(func(int) {}); got != p {
		t.Fatal("SetEvict 应返回接收者自身")
	}
	if got := p.SetEvict(nil); got != p {
		t.Fatal("SetEvict(nil) 也应返回接收者自身")
	}

	n := 0
	q := NewFixedRingPool(2, func() int { return 0 }).SetEvict(func(int) { n++ })
	for i := 1; i <= 5; i++ { // capacity=2 → 淘汰 3 次
		q.Put(i)
	}
	if n != 3 {
		t.Fatalf("链式登记的回调未生效：淘汰 %d 次，期望 3", n)
	}
}

// 并发 Get/Put + 淘汰回调（配合 -race 验证 evict 快照读取无争用）
func TestFixedRingPoolEvictConcurrent(t *testing.T) {
	const cap = 32
	p := NewFixedRingPool(cap, func() *testHandle { return &testHandle{} })
	var evictions, puts uint64
	p.SetEvict(func(h *testHandle) {
		if h == nil {
			t.Error("evict 收到 nil 对象")
		}
		atomic.AddUint64(&evictions, 1)
	})
	var wg sync.WaitGroup
	for w := 0; w < 16; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 3000; i++ {
				h := p.Get()
				p.Put(h)
				atomic.AddUint64(&puts, 1)
			}
		}()
	}
	wg.Wait()
	// 16 个 goroutine 交替 Get/Put（同一对象立即归还），通常不会填满池；
	// 只要求淘汰次数不超过归还次数，且不出现负值/溢出
	if e := atomic.LoadUint64(&evictions); e > atomic.LoadUint64(&puts) {
		t.Fatalf("淘汰 %d 次 > 归还 %d 次", e, atomic.LoadUint64(&puts))
	}
	if p.count > cap {
		t.Fatalf("count=%d 超过 capacity=%d", p.count, cap)
	}
}
