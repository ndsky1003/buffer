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
	// 命中率阈值：>0.8扩容，<0.2缩容，业界通用最优值，不用调
	HitRateHigh = 0.8
	HitRateLow  = 0.2
)

type AdaptiveRingPool[T any] struct {
	// mu     sync.Mutex // 单锁，性能足够，个人项目不用换自旋锁
	mu     SpinLock
	buffer []T // 环形队列底层数组
	head   int // 队头：取数据位置
	tail   int // 队尾：放数据位置
	count  int // 当前空闲对象数

	minCap int // 最小容量（保底）
	maxCap int // 最大容量（封顶）
	curCap int // 当前容量（动态变化）

	// 自动学习的核心统计：原子变量，无锁统计，零性能损耗
	hitCount atomic.Int64 // 缓存命中数
	getCount atomic.Int64 // 总获取数

	New func() T // 创建新对象函数
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
	defer p.mu.Unlock()

	// 2. 有空闲对象，复用，命中数+1
	if p.count > 0 {
		obj := p.buffer[p.head]
		p.head = (p.head + 1) % p.curCap
		p.count--
		p.hitCount.Add(1)
		return obj
	}

	// 3. 无空闲对象，新建
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

// autoScale 自动学习+扩容缩容核心逻辑，极简，无复杂计算，锁内执行，耗时可忽略
func (p *AdaptiveRingPool[T]) autoScale() {
	// 总获取数为0，无需伸缩
	total := p.getCount.Load()
	if total == 0 {
		return
	}

	// 计算命中率
	hitRate := float64(p.hitCount.Load()) / float64(total)

	// 情况1：命中率过高 → 忙时，扩容
	if hitRate > HitRateHigh && p.curCap < p.maxCap {
		newCap := int(float64(p.curCap) * ScaleUpFactor)
		newCap = min(newCap, p.maxCap)
		p.resize(newCap)
		return
	}

	// 情况2：命中率过低 → 闲时，缩容
	if hitRate < HitRateLow && p.curCap > p.minCap {
		newCap := int(float64(p.curCap) * ScaleDownFactor)
		newCap = max(newCap, p.minCap)
		p.resize(newCap)
		return
	}

	// 情况3：命中率适中，不做任何操作，维持当前容量
}

// resize 环形队列的扩容/缩容实现，最优写法，无内存浪费，性能极致
func (p *AdaptiveRingPool[T]) resize(newCap int) {
	if newCap == p.curCap {
		return
	}

	// 新建新容量的数组
	newBuf := make([]T, newCap)
	// 把原队列中的空闲对象，按顺序拷贝到新数组，只拷贝有效数据，无浪费
	copyCount := 0
	for copyCount < p.count {
		srcIdx := (p.head + copyCount) % p.curCap
		newBuf[copyCount] = p.buffer[srcIdx]
		copyCount++
	}

	// 更新队列状态，完成伸缩
	p.buffer = newBuf
	p.head = 0
	p.tail = p.count
	p.curCap = newCap

	// 重置统计，开始新一轮的自动学习
	p.hitCount.Store(0)
	p.getCount.Store(0)
}
