package buffer

import (
	"bytes"
	"fmt"
	"math/rand"
	"testing"
)

// TestEvolution 模拟流量演进
func TestEvolution(t *testing.T) {
	// 创建默认池子
	p := NewBufferPool()

	fmt.Println("Step | Traffic(Approx) | CalibratedSz | Status")
	fmt.Println("-----|-----------------|--------------|-------")

	// 阶段 1: 稳定小流量 (2KB)
	runPhase(t, p, "Stable Low", 2000, 2048, 200)

	// 阶段 2: 突增流量 (10KB)
	runPhase(t, p, "Surge High", 2000, 10240, 500)

	// 阶段 3: 流量回落 (2KB) - 观察慢跌
	runPhase(t, p, "Cooldown", 10000, 2048, 200)
}

func runPhase(t *testing.T, p *Pool[*bytes.Buffer], phaseName string, cycles int, baseSize int, jitter int) {
	for i := 0; i < cycles; i++ {
		// 1. 获取
		buf := p.Get()

		// 2. 模拟真实使用 (核心修正点！！！)
		// ---------------------------------------------------------
		usage := baseSize + rand.Intn(jitter*2) - jitter
		if usage < 0 {
			usage = 100
		}

		// [关键错误修正]
		// 原来的测试可能只 Grow 了，Cap 变了但 Len 还是 0。
		// 库的 statFunc 统计的是 Len。必须 Write 数据进去！

		// A. 确保容量足够
		if buf.Cap() < usage {
			buf.Grow(usage)
		}

		// B. 【重要】写入假数据，撑大 Len
		// 如果不写这一步，Len 是 0，Pool 会认为你在还空包，从而忽略统计
		needed := usage - buf.Len()
		if needed > 0 {
			// 快速模拟写入，填充 buffer 到 usage 长度
			buf.Write(make([]byte, needed))
		}
		// ---------------------------------------------------------

		// 3. 归还
		p.Put(buf)

		// 4. 打印监测 (每 1000 次)
		if (i+1)%1000 == 0 {
			// 探测当前水位
			probe := p.Get()
			currentSz := probe.Cap()
			p.Put(probe)

			fmt.Printf("%4d | ~%5dB        | %5dB        | %s\n",
				i+1, baseSize, currentSz, phaseName)
		}
	}
}

func TestCooldownDebug(t *testing.T) {
	p := NewBufferPool()

	// 1. 先把水位撑大 (模拟 Surge)
	// 直接修改内部状态（如果为了快速测试）或者通过 Put 大包
	// 这里我们用正规方法 Put 一个大包
	largeBuf := p.Get()
	largeBuf.Grow(10240)
	largeBuf.Write(make([]byte, 10240)) // 必须 Write
	p.Put(largeBuf)

	// 此时调用 calibrate 应该会让水位上涨
	// 我们手动多跑几次确保它涨上去，或者简单点，假设它已经涨上去了
	// (根据你的结果，它已经涨到了 9990)

	fmt.Println("Start Cooldown Phase...")

	// 2. Cooldown 阶段
	for i := 0; i < 2000; i++ {
		b := p.Get()

		// --- 核心修正 ---
		b.Reset() // 保险起见
		b.Grow(2048)
		b.Write(make([]byte, 2048))

		// 强行检查！如果这里是 0，那就是 buffer 用法问题
		if b.Len() != 2048 {
			t.Fatalf("Buffer Len is %d, expected 2048", b.Len())
		}
		// ----------------

		p.Put(b)

		// 触发校准
		if (i+1)%1000 == 0 {
			// 探测水位
			probe := p.Get()
			sz := probe.Cap()
			p.Put(probe)
			fmt.Printf("Cycle %d: CalibratedSz = %d\n", i+1, sz)
		}
	}
}
