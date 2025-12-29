package buffer

// 复用之前的runtime_procPin/runtime_procUnpin函数
import (
	"runtime"
	"sync"
	"time"
	_ "unsafe"
)

// AdaptivePool 自适应伸缩的分片对象池
type AdaptivePool[T any] struct {
	shards        []*adaptiveShard[T] // 每个CPU核心一个分片
	minIdle       int                 // 每个分片最小空闲数（保底）
	maxIdle       int                 // 每个分片初始最大空闲数
	maxIdleLimit  int                 // 每个分片最大空闲数上限（防止无限扩容）
	scaleFactor   float64             // 扩容因子（默认1.2）
	shrinkFactor  float64             // 缩容因子（默认0.8）
	scaleInterval time.Duration       // 伸缩检查间隔（默认10秒）
}

// adaptiveShard 单个分片的自适应池
type adaptiveShard[T any] struct {
	mu          sync.Mutex
	idle        []idleObj[T] // 空闲对象（带最后使用时间）
	activeCount int64        // 活跃对象数（正在使用的）
	hitCount    int64        // 缓存命中数
	getCount    int64        // 总获取数
	currentMax  int          // 当前分片的最大空闲数
	newFunc     func() T     // 创建新对象的函数
	resetFunc   func(*T)     // 重置对象的函数
	lastScale   time.Time    // 上次伸缩调整时间
}

// idleObj 带时间戳的空闲对象
type idleObj[T any] struct {
	obj     T
	lastUse time.Time // 最后使用时间
}

// NewAdaptivePool 创建自适应对象池
// minIdle: 每个分片保底空闲数；maxIdle: 初始最大空闲数；maxIdleLimit: 最大空闲上限
func NewAdaptivePool[T any](minIdle, maxIdle, maxIdleLimit int, newFunc func() T, resetFunc func(*T)) *AdaptivePool[T] {
	numCPU := runtime.NumCPU()
	shards := make([]*adaptiveShard[T], numCPU)

	// 初始化每个分片
	for i := 0; i < numCPU; i++ {
		shards[i] = &adaptiveShard[T]{
			idle:       make([]idleObj[T], 0, maxIdle),
			currentMax: maxIdle,
			newFunc:    newFunc,
			resetFunc:  resetFunc,
			lastScale:  time.Now(),
		}
	}

	return &AdaptivePool[T]{
		shards:        shards,
		minIdle:       minIdle,
		maxIdle:       maxIdle,
		maxIdleLimit:  maxIdleLimit,
		scaleFactor:   1.2,              // 忙时扩容20%
		shrinkFactor:  0.8,              // 闲时缩容20%
		scaleInterval: 10 * time.Second, // 每10秒检查一次
	}
}

// Get 从池中获取对象
func (p *AdaptivePool[T]) Get() T {
	// 获取当前Goroutine绑定的分片
	shardIdx := runtime_procPin() % len(p.shards)
	runtime_procUnpin()
	shard := p.shards[shardIdx]

	shard.mu.Lock()
	defer shard.mu.Unlock()

	// 统计总获取数
	shard.getCount++

	// 优先从空闲列表取对象（剔除超期的）
	now := time.Now()
	for len(shard.idle) > 0 {
		obj := shard.idle[len(shard.idle)-1]
		shard.idle = shard.idle[:len(shard.idle)-1]

		// 检查对象是否超期（超过scaleInterval未使用则丢弃）
		if now.Sub(obj.lastUse) < p.scaleInterval {
			// 缓存命中
			shard.hitCount++
			if shard.resetFunc != nil {
				shard.resetFunc(&obj.obj)
			}
			shard.activeCount++
			return obj.obj
		}
	}

	// 无空闲对象，创建新的
	shard.activeCount++
	return shard.newFunc()
}

// Put 将对象放回池中
func (p *AdaptivePool[T]) Put(obj T) {
	shardIdx := runtime_procPin() % len(p.shards)
	runtime_procUnpin()
	shard := p.shards[shardIdx]

	shard.mu.Lock()
	defer shard.mu.Unlock()

	// 活跃数减1
	shard.activeCount--

	// 检查是否需要动态调整最大空闲数
	now := time.Now()
	p.scaleShard(shard, now)

	// 如果空闲数未超当前最大值，放回池
	if len(shard.idle) < shard.currentMax {
		shard.idle = append(shard.idle, idleObj[T]{
			obj:     obj,
			lastUse: time.Now(),
		})
	}
	// 超过则直接丢弃，避免内存超限
}

// scaleShard 动态调整分片的最大空闲数
func (p *AdaptivePool[T]) scaleShard(shard *adaptiveShard[T], now time.Time) {
	// 检查是否到了伸缩时间
	if now.Sub(shard.lastScale) < p.scaleInterval {
		return
	}
	shard.lastScale = now

	// 计算缓存命中率
	var hitRate float64
	if shard.getCount > 0 {
		hitRate = float64(shard.hitCount) / float64(shard.getCount)
	}

	// 重置统计数
	shard.hitCount = 0
	shard.getCount = 0

	// 1. 忙时扩容：命中率>0.8（说明空闲对象不够），且未到上限
	if hitRate > 0.8 && shard.currentMax < p.maxIdleLimit {
		newMax := int(float64(shard.currentMax) * p.scaleFactor)
		if newMax > p.maxIdleLimit {
			newMax = p.maxIdleLimit
		}
		shard.currentMax = newMax
		return
	}

	// 2. 闲时缩容：命中率<0.2（说明空闲对象太多），且不低于保底
	if hitRate < 0.2 && shard.currentMax > p.minIdle {
		newMax := int(float64(shard.currentMax) * p.shrinkFactor)
		if newMax < p.minIdle {
			newMax = p.minIdle
		}
		shard.currentMax = newMax

		// 缩容时清理多余的空闲对象
		if len(shard.idle) > newMax {
			shard.idle = shard.idle[:newMax]
		}
	}
}
