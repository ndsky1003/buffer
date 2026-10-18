package buffer

import (
	"bytes"
	"sync"
	"testing"
)

// 基本 Get/Put
func TestClassPoolBasic(t *testing.T) {
	cp := NewBufferClassPool([]uint64{64 << 10})
	b := cp.Get(2 << 10)
	if b == nil {
		t.Fatal("Get 返回 nil")
	}
	b.WriteString("hello")
	cp.Put(b)
}

// 双峰流量（2K + 2M）：各自归拢到不同档，互不污染
func TestClassPoolDualTraffic(t *testing.T) {
	cp := NewBufferClassPool([]uint64{64 << 10}) // 两档：<64K、>=64K
	for i := 0; i < 50000; i++ {
		var size uint64
		if i%10 == 0 {
			size = 2 << 20 // 2M
		} else {
			size = 2 << 10 // 2K
		}
		b := cp.Get(size)
		if uint64(b.Cap()) < size {
			b.Grow(int(size))
		}
		b.Write(make([]byte, size))
		cp.Put(b)
	}
	sb := cp.Get(2 << 10)
	lb := cp.Get(2 << 20)
	sc, lc := sb.Cap(), lb.Cap()
	cp.Put(sb)
	cp.Put(lb)
	if sc >= 64<<10 {
		t.Errorf("小档 buffer cap=%d，应 < 64K（被大流量污染了）", sc)
	}
	if lc < 64<<10 {
		t.Errorf("大档 buffer cap=%d，应 >= 64K", lc)
	}
}

// 三档归拢：各档独立校准
func TestClassPoolThreeClasses(t *testing.T) {
	cp := NewBufferClassPool([]uint64{8 << 10, 1 << 20}) // <8K、8K~1M、>=1M
	sizes := []uint64{2 << 10, 64 << 10, 2 << 20}
	for i := 0; i < 30000; i++ {
		size := sizes[i%3]
		b := cp.Get(size)
		if uint64(b.Cap()) < size {
			b.Grow(int(size))
		}
		b.Write(make([]byte, size))
		cp.Put(b)
	}
	// 三档各自应落在自己的量级
	c1 := cp.Get(2 << 10)
	c2 := cp.Get(64 << 10)
	c3 := cp.Get(2 << 20)
	if c1.Cap() >= 8<<10 {
		t.Errorf("档1 cap=%d，应 < 8K", c1.Cap())
	}
	if c2.Cap() < 8<<10 || c2.Cap() >= 1<<20 {
		t.Errorf("档2 cap=%d，应在 [8K, 1M)", c2.Cap())
	}
	if c3.Cap() < 1<<20 {
		t.Errorf("档3 cap=%d，应 >= 1M", c3.Cap())
	}
	cp.Put(c1)
	cp.Put(c2)
	cp.Put(c3)
}

// 自动分桶：非 2 的幂请求向上取整到 2 的幂桶，误差 ≤2×
func TestClassPoolAutoRounding(t *testing.T) {
	cp := NewBufferAutoClassPool()
	sizes := []uint64{3 << 10, 5 << 10, 100 << 10, 3 << 20} // 3K, 5K, 100K, 3M
	for _, sz := range sizes {
		for i := 0; i < 8000; i++ {
			b := cp.Get(sz)
			if uint64(b.Cap()) < sz {
				b.Grow(int(sz))
			}
			b.Write(make([]byte, sz))
			cp.Put(b)
		}
		probe := cp.Get(sz)
		c := uint64(probe.Cap())
		cp.Put(probe)
		if c < sz || c > sz*2 {
			t.Errorf("请求 %d 归拢到桶 cap=%d，超出 2 倍误差", sz, c)
		}
	}
}

// 空 boundaries：退化为单档（等价于单 Pool）
func TestClassPoolEmptyBoundaries(t *testing.T) {
	cp := NewBufferClassPool(nil)
	if len(cp.classes) != 1 {
		t.Fatalf("空 boundaries 应只有 1 档，实际 %d 档", len(cp.classes))
	}
	b := cp.Get(1024)
	if b == nil {
		t.Fatal("Get 返回 nil")
	}
	cp.Put(b)
}

// 无效 boundaries（不递增、越界）被忽略
func TestClassPoolInvalidBoundaries(t *testing.T) {
	cp := NewBufferClassPool([]uint64{64 << 10, 32 << 10, 1000 << 20}) // 32K 不递增、1000M 越界
	// 有效分界只有 64K，所以应是 2 档
	if len(cp.classes) != 2 {
		t.Fatalf("期望 2 档（只保留有效的 64K 分界），实际 %d 档", len(cp.classes))
	}
}

// Put(nil) 安全
func TestClassPoolNilPut(t *testing.T) {
	cp := NewBufferClassPool([]uint64{64 << 10})
	cp.Put(nil) // 不应 panic
	cp.Put((*bytes.Buffer)(nil))
}

// 并发安全
func TestClassPoolConcurrent(t *testing.T) {
	cp := NewBufferAutoClassPool()
	var wg sync.WaitGroup
	for w := 0; w < 32; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < 2000; i++ {
				size := uint64(1 << ((i + id) % 16)) // 1B ~ 32KB 混合大小
				b := cp.Get(size)
				if uint64(b.Cap()) < size {
					b.Grow(int(size))
				}
				b.Write(make([]byte, size))
				cp.Put(b)
			}
		}(w)
	}
	wg.Wait()
}
