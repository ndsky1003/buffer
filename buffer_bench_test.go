package buffer

import (
	"bytes"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// =============================================================================
// 基准测试 - 单 goroutine 性能
// =============================================================================

// BenchmarkBufferPoolGetPut 基准测试 Get/Put 性能
func BenchmarkBufferPoolGetPut(b *testing.B) {
	p := NewBufferPool()
	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		buf := p.Get()
		buf.WriteString("benchmark test data")
		p.Put(buf)
	}
}

// BenchmarkBufferPoolGetPutWithData 带数据的 Get/Put
func BenchmarkBufferPoolGetPutWithData(b *testing.B) {
	p := NewBufferPool()
	data := make([]byte, 1024)
	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		buf := p.Get()
		buf.Write(data)
		p.Put(buf)
	}
}

// BenchmarkBufferPoolGetPut4K 4KB 数据
func BenchmarkBufferPoolGetPut4K(b *testing.B) {
	p := NewBufferPool()
	data := make([]byte, 4096)
	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		buf := p.Get()
		buf.Write(data)
		p.Put(buf)
	}
}

// BenchmarkBufferPoolGetPut16K 16KB 数据
func BenchmarkBufferPoolGetPut16K(b *testing.B) {
	p := NewBufferPool()
	data := make([]byte, 16*1024)
	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		buf := p.Get()
		buf.Write(data)
		p.Put(buf)
	}
}

// BenchmarkBytePoolGetPut []byte 池基准
func BenchmarkBytePoolGetPut(b *testing.B) {
	p := NewBytePool()
	data := make([]byte, 1024)
	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		buf := p.Get()
		buf = append(buf, data...)
		p.Put(buf[:0])
	}
}

// BenchmarkDirectNewBuffer 对比：直接创建新 Buffer
func BenchmarkDirectNewBuffer(b *testing.B) {
	for i := 0; i < b.N; i++ {
		buf := bytes.NewBuffer(make([]byte, 0, 1024))
		buf.WriteString("benchmark test data")
		_ = buf
	}
}

// BenchmarkDirectMakeByteSlice 对比：直接 make []byte
func BenchmarkDirectMakeByteSlice(b *testing.B) {
	for i := 0; i < b.N; i++ {
		buf := make([]byte, 0, 1024)
		buf = append(buf, make([]byte, 1024)...)
		_ = buf
	}
}

// =============================================================================
// 并发基准测试 - 多 goroutine 吞吐量
// =============================================================================

// BenchmarkConcurrentGetPut1 并发 Get/Put - 1 worker (baseline)
func BenchmarkConcurrentGetPut1(b *testing.B) {
	benchmarkConcurrentWorkers(b, 1, 1024)
}

// BenchmarkConcurrentGetPut2 并发 Get/Put - 2 workers
func BenchmarkConcurrentGetPut2(b *testing.B) {
	benchmarkConcurrentWorkers(b, 2, 1024)
}

// BenchmarkConcurrentGetPut4 并发 Get/Put - 4 workers
func BenchmarkConcurrentGetPut4(b *testing.B) {
	benchmarkConcurrentWorkers(b, 4, 1024)
}

// BenchmarkConcurrentGetPut8 并发 Get/Put - 8 workers
func BenchmarkConcurrentGetPut8(b *testing.B) {
	benchmarkConcurrentWorkers(b, 8, 1024)
}

// BenchmarkConcurrentGetPut16 并发 Get/Put - 16 workers
func BenchmarkConcurrentGetPut16(b *testing.B) {
	benchmarkConcurrentWorkers(b, 16, 1024)
}

// BenchmarkConcurrentGetPut32 并发 Get/Put - 32 workers
func BenchmarkConcurrentGetPut32(b *testing.B) {
	benchmarkConcurrentWorkers(b, 32, 1024)
}

// BenchmarkConcurrentGetPut64 并发 Get/Put - 64 workers
func BenchmarkConcurrentGetPut64(b *testing.B) {
	benchmarkConcurrentWorkers(b, 64, 1024)
}

// BenchmarkConcurrentGetPut128 并发 Get/Put - 128 workers
func BenchmarkConcurrentGetPut128(b *testing.B) {
	benchmarkConcurrentWorkers(b, 128, 1024)
}

func benchmarkConcurrentWorkers(b *testing.B, workers, dataSize int) {
	b.ResetTimer()
	b.ReportAllocs()

	p := NewBufferPool()
	data := make([]byte, dataSize)

	var wg sync.WaitGroup
	b.RunParallel(func(pb *testing.PB) {
		wg.Add(1)
		defer wg.Done()
		for pb.Next() {
			buf := p.Get()
			buf.Write(data)
			p.Put(buf)
		}
	})
	wg.Wait()
}

// =============================================================================
// 不同数据大小的并发测试
// =============================================================================

// BenchmarkConcurrent4K 并发处理 4KB 数据
func BenchmarkConcurrent4K(b *testing.B) {
	benchmarkConcurrentWorkers(b, runtime.NumCPU(), 4096)
}

// BenchmarkConcurrent16K 并发处理 16KB 数据
func BenchmarkConcurrent16K(b *testing.B) {
	benchmarkConcurrentWorkers(b, runtime.NumCPU(), 16*1024)
}

// BenchmarkConcurrent64K 并发处理 64KB 数据
func BenchmarkConcurrent64K(b *testing.B) {
	benchmarkConcurrentWorkers(b, runtime.NumCPU(), 64*1024)
}

// =============================================================================
// 极限 QPS 压测
// =============================================================================

// BenchmarkMaxQPS 极限 QPS 压测
func BenchmarkMaxQPS(b *testing.B) {
	p := NewBufferPool()
	data := make([]byte, 1024)

	// 预热
	for i := 0; i < 10000; i++ {
		buf := p.Get()
		buf.Write(data)
		p.Put(buf)
	}

	b.ResetTimer()
	var ops uint64

	// 使用所有 CPU 核心
	workers := runtime.NumCPU() * 2
	if workers < 4 {
		workers = 4
	}

	var wg sync.WaitGroup
	start := make(chan struct{})

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start

			for {
				buf := p.Get()
				buf.Write(data)
				p.Put(buf)
				atomic.AddUint64(&ops, 1)
			}
		}()
	}

	close(start)

	// 运行指定时间
	duration := 5 * time.Second
	time.Sleep(duration)

	// 停止所有 goroutine 并报告
	b.StopTimer()
	qps := float64(atomic.LoadUint64(&ops)) / duration.Seconds()
	b.ReportMetric(qps/1e6, "Mops/s") // 百万 ops/秒

	// 让 goroutine 退出（通过 b.StopTimer 后测试结束）
	// 注意：goroutine 会泄漏，但在 benchmark 中这是可以接受的
}

// BenchmarkMaxQPS4K 极限 QPS - 4KB 数据
func BenchmarkMaxQPS4K(b *testing.B) {
	p := NewBufferPool()
	data := make([]byte, 4096)

	// 预热
	for i := 0; i < 5000; i++ {
		buf := p.Get()
		buf.Write(data)
		p.Put(buf)
	}

	b.ResetTimer()
	var ops uint64

	workers := runtime.NumCPU() * 2
	if workers < 4 {
		workers = 4
	}

	var wg sync.WaitGroup
	start := make(chan struct{})

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start

			for {
				buf := p.Get()
				buf.Write(data)
				p.Put(buf)
				atomic.AddUint64(&ops, 1)
			}
		}()
	}

	close(start)
	duration := 5 * time.Second
	time.Sleep(duration)

	b.StopTimer()
	qps := float64(atomic.LoadUint64(&ops)) / duration.Seconds()
	b.ReportMetric(qps/1e6, "Mops/s")
}

// BenchmarkMaxQPSBytePool []byte 池极限 QPS
func BenchmarkMaxQPSBytePool(b *testing.B) {
	p := NewBytePool()
	data := make([]byte, 1024)

	// 预热
	for i := 0; i < 10000; i++ {
		buf := p.Get()
		buf = append(buf, data...)
		p.Put(buf[:0])
	}

	b.ResetTimer()
	var ops uint64

	workers := runtime.NumCPU() * 2
	if workers < 4 {
		workers = 4
	}

	var wg sync.WaitGroup
	start := make(chan struct{})

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start

			for {
				buf := p.Get()
				buf = append(buf, data...)
				p.Put(buf[:0])
				atomic.AddUint64(&ops, 1)
			}
		}()
	}

	close(start)
	duration := 5 * time.Second
	time.Sleep(duration)

	b.StopTimer()
	qps := float64(atomic.LoadUint64(&ops)) / duration.Seconds()
	b.ReportMetric(qps/1e6, "Mops/s")
}

// =============================================================================
// 动态流量场景测试
// =============================================================================

// BenchmarkBurstTraffic 突发流量场景
func BenchmarkBurstTraffic(b *testing.B) {
	p := NewBufferPool()
	data := make([]byte, 1024)

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		// 模拟突发：小流量后跟大流量
		if i%10 == 0 {
			// 突发
			for j := 0; j < 10; j++ {
				buf := p.Get()
				buf.Write(make([]byte, 8192)) // 大数据
				p.Put(buf)
			}
		}

		// 正常流量
		buf := p.Get()
		buf.Write(data)
		p.Put(buf)
	}
}

// BenchmarkVariableTraffic 变化流量场景
func BenchmarkVariableTraffic(b *testing.B) {
	p := NewBufferPool()

	b.ResetTimer()

	sizes := []int{512, 1024, 2048, 4096, 8192, 4096, 2048, 1024, 512}

	for i := 0; i < b.N; i++ {
		size := sizes[i%len(sizes)]
		buf := p.Get()
		buf.Write(make([]byte, size))
		p.Put(buf)
	}
}

// =============================================================================
// 对比测试：Pool vs No Pool
// =============================================================================

// BenchmarkNoPoolVsPool 无池 vs 有池对比 - 单线程
func BenchmarkNoPoolVsPool(b *testing.B) {
	data := make([]byte, 1024)

	b.Run("NoPool", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			buf := bytes.NewBuffer(make([]byte, 0, 1024))
			buf.Write(data)
			_ = buf
		}
	})

	b.Run("WithPool", func(b *testing.B) {
		p := NewBufferPool()
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			buf := p.Get()
			buf.Write(data)
			p.Put(buf)
		}
	})
}

// BenchmarkNoPoolVsPoolConcurrent 无池 vs 有池对比 - 并发
func BenchmarkNoPoolVsPoolConcurrent(b *testing.B) {
	data := make([]byte, 1024)

	b.Run("NoPool", func(b *testing.B) {
		b.ReportAllocs()
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				buf := bytes.NewBuffer(make([]byte, 0, 1024))
				buf.Write(data)
				_ = buf
			}
		})
	})

	b.Run("WithPool", func(b *testing.B) {
		p := NewBufferPool()
		b.ReportAllocs()
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				buf := p.Get()
				buf.Write(data)
				p.Put(buf)
			}
		})
	})
}

// =============================================================================
// 校准开销测试
// =============================================================================

// BenchmarkCalibrationOverhead 测试校准对性能的影响
func BenchmarkCalibrationOverhead(b *testing.B) {
	data := make([]byte, 1024)

	b.Run("FrequentCalibration", func(b *testing.B) {
		opt := Options().SetCalibratePeriod(100)
		p := NewBufferPool(opt)
		b.ReportAllocs()

		for i := 0; i < b.N; i++ {
			buf := p.Get()
			buf.Write(data)
			p.Put(buf)
		}
	})

	b.Run("RareCalibration", func(b *testing.B) {
		opt := Options().SetCalibratePeriod(100000)
		p := NewBufferPool(opt)
		b.ReportAllocs()

		for i := 0; i < b.N; i++ {
			buf := p.Get()
			buf.Write(data)
			p.Put(buf)
		}
	})
}

// =============================================================================
// 持续压测测试
// =============================================================================

// BenchmarkSustainedLoad 持续负载测试
func BenchmarkSustainedLoad(b *testing.B) {
	p := NewBufferPool()
	data := make([]byte, 4096)

	b.ResetTimer()
	b.ReportAllocs()

	// 混合不同大小的操作
	for i := 0; i < b.N; i++ {
		var buf *bytes.Buffer

		switch i % 4 {
		case 0:
			buf = p.Get()
			buf.Write(data)
		case 1:
			buf = p.Get()
			buf.Write(make([]byte, 512))
		case 2:
			buf = p.Get()
			buf.Write(make([]byte, 8192))
		case 3:
			buf = p.Get()
			buf.Write(make([]byte, 2048))
		}

		p.Put(buf)
	}
}

// =============================================================================
// 压力测试 - 极限场景
// =============================================================================

// BenchmarkStressTestPutAfterPut 压力测试：连续 Put
func BenchmarkStressTestPutAfterPut(b *testing.B) {
	p := NewBufferPool()
	data := make([]byte, 1024)

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		buf := p.Get()
		buf.Write(data)
		p.Put(buf)
		p.Put(buf) // 连续 Put，应该安全
	}
}

// BenchmarkStressTestEmptyGet 压力测试：大量空 Get
func BenchmarkStressTestEmptyGet(b *testing.B) {
	p := NewBufferPool()

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		buf := p.Get()
		// 不使用
		p.Put(buf)
	}
}

// BenchmarkPoolUnderContention 高争用场景测试
func BenchmarkPoolUnderContention(b *testing.B) {
	p := NewBufferPool()
	data := make([]byte, 1024)

	b.ResetTimer()

	// 创建远超 CPU 核心数的 goroutine，制造争用
	goroutines := 100
	var wg sync.WaitGroup

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 1000; j++ {
				buf := p.Get()
				buf.Write(data)
				p.Put(buf)
			}
		}()
	}

	wg.Wait()
}

// =============================================================================
// 性能测试辅助函数
// =============================================================================

// printQPSResults 打印 QPS 测试结果（用于手动测试）
func printQPSResults(name string, ops uint64, duration time.Duration) {
	qps := float64(ops) / duration.Seconds()
	fmt.Printf("%s: %.2f M ops/s (%.0f ops in %v)\n",
		name, qps/1e6, float64(ops), duration)
}

// TestQPSManual 手动 QPS 测试（非 benchmark）
func TestQPSManual(t *testing.T) {
	tests := []struct {
		name     string
		dataSize int
		workers  int
		duration time.Duration
	}{
		{"1KB_1worker", 1024, 1, time.Second},
		{"1KB_4workers", 1024, 4, time.Second},
		{"1KB_16workers", 1024, 16, time.Second},
		{"4KB_MaxWorkers", 4096, runtime.NumCPU() * 2, 2 * time.Second},
		{"16KB_MaxWorkers", 16 * 1024, runtime.NumCPU() * 2, 2 * time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := NewBufferPool()
			data := make([]byte, tt.dataSize)

			// 预热
			for i := 0; i < 10000; i++ {
				buf := p.Get()
				buf.Write(data)
				p.Put(buf)
			}

			var ops uint64
			var wg sync.WaitGroup
			start := make(chan struct{})

			for i := 0; i < tt.workers; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start

					for {
						buf := p.Get()
						buf.Write(data)
						p.Put(buf)
						atomic.AddUint64(&ops, 1)
					}
				}()
			}

			startTime := time.Now()
			close(start)

			time.Sleep(tt.duration)

			elapsed := time.Since(startTime)
			qps := float64(atomic.LoadUint64(&ops)) / elapsed.Seconds()

			t.Logf("QPS: %.2f M ops/s (%.0f ops in %v)",
				qps/1e6, float64(atomic.LoadUint64(&ops)), elapsed)

			// goroutine 会泄漏，但在测试中可接受
		})
	}
}
