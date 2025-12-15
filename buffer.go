package buffer

import (
	"sync"
	"sync/atomic"
)

const (
	// emaUpFactor: 上涨时的平滑因子 (0~1)。
	// 值越小，对新值越敏感（涨得越快）。0.4 代表保留 40% 历史，接纳 60% 新值。
	// 选 0.4 而不是 0 是为了过滤掉偶发的“超级尖峰”。
	emaUpFactor = 0.4

	// emaDownFactor: 下跌时的平滑因子 (0~1)。
	// 值越大，对历史越执着（跌得越慢）。0.9 代表保留 90% 历史，只接纳 10% 下跌。
	// 这有助于在流量波动时保持水位，避免频繁扩容。
	emaDownFactor = 0.9

	// premiumFactor: 溢价系数。
	// 为了防止 EMA 算法永远逼近但达不到最大值，我们给结果增加 5% 的余量。
	premiumFactor = 1.05
)

// -----------------------------------------------------------------------------
// 优化技巧：Cache Padding
// -----------------------------------------------------------------------------
// cacheLineSize 通常是 64 字节。我们用 padding 防止伪共享。
type padding [64]byte

// Pool 是一个自动伸缩的 bytes.Buffer 池
type Pool[T any] struct {
	pool sync.Pool
	// --- 适配器函数 (核心变化) ---
	// 这些函数消除了 *bytes.Buffer 和 []byte 的差异
	// 虽然是函数指针调用，但在现代 CPU 上开销极低
	makeFunc  func(size uint64) T
	resetFunc func(T) T // 返回 T 是为了兼容 slice 的 reslice 操作
	statFunc  func(T) (used, cap uint64)

	// 1. 配置参数 (只读，无需原子操作)
	minSize         uint64
	maxSize         uint64
	calibratePeriod uint64
	maxPercent      float64

	_ padding // 隔离只读区和读写区

	// 2. 运行时状态 (高频读写，原子操作)
	calls        uint64
	_            padding // 隔离 calls 和 maxUsage
	maxUsage     uint64  //校准区间的最大使用者,是多少
	_            padding // 隔离 maxUsage 和 calibratedSz
	calibratedSz uint64  //校准值，最新分配的大小
}

// New 创建一个新的智能池
func New[T any](makeFunc func(uint64) T, resetFunc func(T) T, statFunc func(T) (uint64, uint64), opts ...*Option) *Pool[T] {
	opt := Options().
		SetMinSize(512).          // 最小不小于 512B
		SetMaxSize(64 << 20).     // 最大不超过 64MB (防止 OOM) 64<< 10 是64k
		SetCalibratePeriod(1000). //多久校准一次
		SetMaxPercent(2.0).
		SetCalibratedSz(1024). //校准就是修改这个size,最新适合的size
		Merge(opts...)
	p := &Pool[T]{
		minSize:         *opt.MinSize,
		maxSize:         *opt.MaxSize,
		calibratePeriod: *opt.CalibratePeriod,
		maxPercent:      *opt.MaxPercent,
		calibratedSz:    *opt.CalibratedSz, // 初始猜测值
		makeFunc:        makeFunc,
		resetFunc:       resetFunc,
		statFunc:        statFunc,
	}

	// 确保初始值合法
	p.calibratedSz = max(p.minSize, p.calibratedSz)

	p.pool.New = func() any {
		// 原子读取当前的校准大小
		size := atomic.LoadUint64(&p.calibratedSz)
		return p.makeFunc(size)
	}

	return p
}

// Get 获取原生 *bytes.Buffer (零分配)
func (p *Pool[T]) Get() T {
	// 类型断言在 Go 中非常快
	return p.pool.Get().(T)
}

// Put 归还并智能处理
func (p *Pool[T]) Put(b T) {
	// if b == nil {
	// 	return
	// }

	// 此时 buffer 已包含数据，Len 是实际使用量，Cap 是底层数组容量
	// used := uint64(b.Len())
	// capVal := uint64(b.Cap())
	used, capVal := p.statFunc(b) // ==nil cap 返回0
	if capVal == 0 {
		return
	}

	// 1. 智能采样更新 maxUsage (性能优化核心)
	// 不要每次 Put 都去 CAS 抢锁。
	// 策略：如果流量突增(used > current)，必须记录；否则低概率采样记录。
	currentSz := atomic.LoadUint64(&p.calibratedSz)
	shouldRecord := false

	if used > currentSz {
		// 流量突增，必须记录，防止下一轮分配过小
		shouldRecord = true
	} else if used > p.minSize {
		// 只有大于最小值的包才有记录意义。
		// 这里使用简单的位运算做低成本采样 (每 16 次记录一次)
		// 注意：这里用 b.Cap() 的地址或者其他随机数做判断源均可，
		// 简单起见，利用 calls 的低位在下面做判断也可以，这里为了逻辑分离，
		// 仅在确实较大时尝试更新。
		// 简化策略：为了极致性能，非突增情况，我们甚至可以不更新 maxUsage，
		// 因为 calibrate 会自动处理收缩。我们只关注“撑大”的情况。
		if atomic.LoadUint64(&p.calls)&0x0F == 0 {
			shouldRecord = true
		}
	}

	if shouldRecord {
		for {
			oldMax := atomic.LoadUint64(&p.maxUsage)
			if used <= oldMax {
				break
			}
			// CAS 乐观锁更新
			if atomic.CompareAndSwapUint64(&p.maxUsage, oldMax, used) {
				break
			}
		}
	}

	// 2. 触发校准 (原子计数器)
	newCalls := atomic.AddUint64(&p.calls, 1)
	if newCalls >= p.calibratePeriod {
		// 只有获得重置权的那个 goroutine 去执行 calibrate
		if atomic.CompareAndSwapUint64(&p.calls, newCalls, 0) {
			p.calibrate()
		}
	}

	// 3. 智能丢弃判决
	// 如果当前 buffer 容量远超当前需要的尺寸，归还给 pool 会导致内存泄漏（虚高）。
	// 直接丢弃，让 GC 回收。
	if capVal > uint64(float64(currentSz)*p.maxPercent) {
		return
	}

	// 必须 Reset 才能复用
	// b.Reset()
	p.resetFunc(b)
	p.pool.Put(b)
}

// calibrate 计算周期内新的基准大小 (核心算法)
// 此方法在单独的 goroutine 或低频路径执行，不需要极度优化，重在算法逻辑
func (p *Pool[T]) calibrate() {
	// 1. 获取并重置本周期的最大使用量
	newMax := atomic.LoadUint64(&p.maxUsage)
	atomic.StoreUint64(&p.maxUsage, 0)

	// 2. 只有当本周期有有效数据时才调整
	if newMax == 0 {
		// 可能是完全闲置，不做调整，避免将 size 拖到 0
		return
	}

	// 3. 限制范围 (Bounds Check)
	newMax = max(p.minSize, newMax)
	newMax = min(newMax, p.maxSize)

	// 4. 读取旧的校准值
	oldSz := atomic.LoadUint64(&p.calibratedSz)

	// 5. EMA (指数加权移动平均) 算法 - 快涨慢跌
	var nextSz uint64

	if newMax > oldSz {
		// 【上涨】：使用较小的因子 (emaUpFactor)，让权重更多向 newMax 倾斜
		// 目的：快速响应流量增长，减少 Get 后的 Grow() 开销
		nextSz = uint64(float64(oldSz)*emaUpFactor + float64(newMax)*(1-emaUpFactor))
	} else {
		// 【下跌】：使用较大的因子 (emaDownFactor)，让权重主要保留在 oldSz
		// 目的：抵抗抖动，只有流量长期低迷时才缓慢缩容
		nextSz = uint64(float64(oldSz)*emaDownFactor + float64(newMax)*(1-emaDownFactor))
	}

	// 6. 微量溢价 (Buffer Premium)
	// 解决“EMA 永远追不上最大值”的问题，给结果加一点余量 (比如 5%)
	// 这保证了分配的 Buffer 大概率略大于实际需求，从而彻底消除 Grow()
	if newMax > oldSz {
		nextSz = uint64(float64(nextSz) * premiumFactor)
	}

	// 再次限制最大值（防止溢价后越界）
	nextSz = max(p.minSize, nextSz)
	nextSz = min(nextSz, p.maxSize)

	// 7. 原子更新最终值
	atomic.StoreUint64(&p.calibratedSz, nextSz)
}
