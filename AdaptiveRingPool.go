package buffer

import (
	"sync/atomic"
)

// -------------------------- 核心配置（个人项目无脑用默认值，不用改） --------------------------
const (
	// 默认最小容量：保底的空闲buffer数，缩容不会低于这个值，内存占用极低
	DefaultMinCapacity = 32
	// 默认最大容量：扩容不会超过这个值，彻底杜绝内存泄漏，个人项目512足够用
	DefaultMaxCapacity = 512
	// 扩容因子：忙时扩容1.2倍，平缓扩容，无性能波动
	ScaleUpFactor = 1.2
	// 缩容因子：闲时缩容0.8倍，平缓缩容，保留足够的空闲buffer
	ScaleDownFactor = 0.8
	// 命中率阈值：<0.2 扩容（miss 多、池子不够用），>0.8 缩容（hit 多、池子过剩）
	HitRateHigh = 0.8
	HitRateLow  = 0.2
	// 采样周期：每 calibrateSamples 次 Get 判断一次命中率，命中率反映「最近一个周期」而非累计，
	// 对近期流量变化更敏感；周期结束（无论是否伸缩）都重置统计。
	calibrateSamples = 1000
)

// 注意：本结构体字段由自旋锁串行化访问（关联访问）。
// 实测缓存行填充（padding）反而破坏局部性、降低吞吐，故采用紧凑排列。
type AdaptiveRingPool[T any] struct {
	mu       SpinLock     // 锁（高频）
	head     int          // 取数位置（高频读写）
	tail     int          // 放数位置（高频读写）
	count    int          // 空闲数（高频读写）
	hitCount atomic.Int64 // 命中数（高频读，低频写）
	getCount atomic.Int64 // 总获取数（高频写）
	minCap   int          // 最小容量（几乎不修改）
	maxCap   int          // 最大容量（初始化后不变）
	curCap   int          // 当前容量（低频修改）
	New      func() T     // 创建函数（初始化后不变）
	buffer   []T          // 环形队列（低频大尺寸访问）
}

// NewAdaptiveRingPool 创建自适应环形池，个人项目无脑用这个，默认配置足够
func NewAdaptiveRingPool[T any](newFunc func() T) *AdaptiveRingPool[T] {
	return NewAdaptiveRingPoolWithLimit(DefaultMinCapacity, DefaultMaxCapacity, newFunc)
}

// NewAdaptiveRingPoolWithLimit 自定义最小/最大容量，按需使用
func NewAdaptiveRingPoolWithLimit[T any](minCap, maxCap int, newFunc func() T) *AdaptiveRingPool[T] {
	if minCap < 1 {
		minCap = 1
	}
	if maxCap < minCap {
		maxCap = minCap
	}
	return &AdaptiveRingPool[T]{
		buffer: make([]T, minCap),
		minCap: minCap,
		maxCap: maxCap,
		curCap: minCap,
		New:    newFunc,
	}
}

// Get 核心：获取对象 + 无锁统计 + 自动学习+伸缩，性能和原生RingBuffer几乎无差别
func (p *AdaptiveRingPool[T]) Get() T {
	// 1. 原子统计：总获取数+1，无锁，零损耗
	p.getCount.Add(1)

	p.mu.Lock()
	// 2. 有空闲对象，复用，命中数+1
	if p.count > 0 {
		obj := p.buffer[p.head]
		p.head = (p.head + 1) % p.curCap
		p.count--
		p.hitCount.Add(1)
		p.mu.Unlock()
		return obj
	}
	p.mu.Unlock()

	// 3. 无空闲对象，锁外新建，避免在自旋锁内做可能触发 GC 的分配
	return p.New()
}

// Put 核心：放回对象 + 触发自动学习+伸缩逻辑，核心逻辑都在这里
func (p *AdaptiveRingPool[T]) Put(obj T) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// 1. 队列未满，放回对象
	if p.count < p.curCap {
		p.buffer[p.tail] = obj
		p.tail = (p.tail + 1) % p.curCap
		p.count++
	}
	// 队列已满，直接丢弃，避免内存溢出

	// 2. 核心：自动学习+自适应伸缩，只在Put时触发，频率极低，无性能损耗
	p.autoScale()
}

// autoScale 自动学习+扩容缩容核心逻辑，必须在持有 p.mu 时调用。
func (p *AdaptiveRingPool[T]) autoScale() {
	// 周期采样：样本不足一个周期时不做判断（也避免了冷启动首次 miss 的误判）
	total := p.getCount.Load()
	if total < calibrateSamples {
		return
	}

	// 计算本周期命中率
	hitRate := float64(p.hitCount.Load()) / float64(total)

	// 情况1：命中率过低（miss 多，池子不够用）→ 扩容
	if hitRate < HitRateLow && p.curCap < p.maxCap {
		newCap := int(float64(p.curCap) * ScaleUpFactor)
		newCap = min(newCap, p.maxCap)
		p.resize(newCap) // resize 会重置统计
		return
	}

	// 情况2：命中率过高（hit 多，池子过剩）→ 缩容
	if hitRate > HitRateHigh && p.curCap > p.minCap {
		newCap := int(float64(p.curCap) * ScaleDownFactor)
		newCap = max(newCap, p.minCap)
		p.resize(newCap) // resize 会重置统计
		return
	}

	// 情况3：命中率适中，不伸缩，但强制重置统计，开始新周期
	p.hitCount.Store(0)
	p.getCount.Store(0)
}

// resize 环形队列的扩容/缩容实现，必须在持有 p.mu 时调用。
func (p *AdaptiveRingPool[T]) resize(newCap int) {
	if newCap == p.curCap {
		return
	}

	// 新建新容量的数组
	newBuf := make([]T, newCap)
	// 把原队列中的空闲对象按顺序拷贝到新数组。
	// 缩容时 newCap 可能小于当前空闲对象数：只拷贝放得下的，多余对象直接丢弃。
	copyCount := min(p.count, newCap)
	for i := 0; i < copyCount; i++ {
		srcIdx := (p.head + i) % p.curCap
		newBuf[i] = p.buffer[srcIdx]
	}

	// 更新队列状态，完成伸缩（count 必须同步为实际拷贝数，避免缩容后越界）
	p.buffer = newBuf
	p.head = 0
	p.tail = copyCount % newCap
	p.count = copyCount
	p.curCap = newCap

	// 重置统计，开始新一轮的自动学习
	p.hitCount.Store(0)
	p.getCount.Store(0)
}
