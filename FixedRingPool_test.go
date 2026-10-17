package buffer

import (
	"bytes"
	"sync"
	"testing"
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
