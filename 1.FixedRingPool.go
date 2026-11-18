package buffer

import "sync"

// FixedRingPool 固定容量的环形对象池。
//
// 与 AdaptiveRingPool 的区别：容量固定、不伸缩、无命中率统计，
// 因此热路径更短（省去 getCount/hitCount 的原子操作和 autoScale 的浮点除法）。
// 实测相比 AdaptiveRingPool 快约 16%~21%。
// 队列满时采用「覆盖最旧」（丢弃最旧的、保留新归还的），使池子始终保持为最新归还的对象，
// 有助于复用大容量 buffer、减少 grow。
// 适用于容量可预先确定、追求极致吞吐的场景。
//
// 注意：覆盖最旧意味着被淘汰的对象被直接丢弃。池化需要显式释放的资源
// （fd、CGO 内存、连接、外部句柄）时，须用 SetEvict 登记释放回调，否则资源泄漏。
type FixedRingPool[T any] struct {
	mu       sync.Mutex // 锁
	head     int        // 取数位置
	tail     int        // 放数位置
	count    int        // 空闲数
	capacity int        // 固定容量
	new      func() T   // 创建函数（池空时调用）
	evict    func(T)    // 淘汰回调（池满覆盖最旧时调用），nil 表示交给 GC
	buffer   []T        // 环形队列
}

// NewFixedRingPool 创建固定容量的环形对象池。
func NewFixedRingPool[T any](capacity int, newFunc func() T) *FixedRingPool[T] {
	if capacity < 1 {
		capacity = 1
	}
	return &FixedRingPool[T]{
		capacity: capacity,
		buffer:   make([]T, capacity),
		new:      newFunc,
	}
}

// Get 取对象：有空闲对象则复用，否则锁外调用 New 新建。
func (p *FixedRingPool[T]) Get() T {
	p.mu.Lock()
	if p.count > 0 {
		obj := p.buffer[p.head]
		p.head = (p.head + 1) % p.capacity
		p.count--
		p.mu.Unlock()
		return obj
	}
	p.mu.Unlock()
	return p.new()
}

// Put 归还对象：队列未满则放回；已满则覆盖最旧的对象（淘汰 head，保持池子始终为最新状态）。
// 已满且设置了淘汰回调时，被淘汰的对象在锁外交给 SetEvict 登记的回调；未设置则直接交给 GC。
func (p *FixedRingPool[T]) Put(obj T) {
	p.mu.Lock()
	if p.count < p.capacity {
		p.buffer[p.tail] = obj
		p.tail = (p.tail + 1) % p.capacity
		p.count++
		p.mu.Unlock()
		return
	}
	// 队列已满：覆盖最旧（head），head/tail 同步前进，count 保持满
	evicted := p.buffer[p.head]
	p.buffer[p.head] = obj
	p.head = (p.head + 1) % p.capacity
	p.tail = p.head
	evict := p.evict // 锁内取快照，与 SetEvict 并发安全
	p.mu.Unlock()
	if evict != nil {
		evict(evicted) // 锁外调用：回调内可安全回用本池的 Get/Put
	}
}

// SetEvict 登记淘汰回调：池满「覆盖最旧」时被淘汰的对象交给 fn 处理，
// 而不是静默交给 GC。传 nil 清除回调。返回自身以便链式调用。
//
// 池化 *bytes.Buffer / []byte 这类纯 GC 对象时不必设置；
// 池化需要显式释放的资源（fd、CGO 内存、mmap、下游连接、外部句柄）时**必须设置**，
// 否则被淘汰的对象既没人 Close、也不再被任何地方引用 → 资源泄漏。
//
// 语义边界：
//   - 只在「池满被覆盖」时触发，且传入的一定是池内的**空闲**对象；
//     已 Get 出去、尚未归还的对象不会被 evict（其残留的槽位引用被覆盖时不触发）。
//   - fn 在池锁外调用，因此 fn 里可以安全调用本池的 Get/Put，不会死锁。
//   - fn 在调用 Put 的那个 goroutine 上执行：不要在其中做长时间阻塞操作，
//     否则拖慢该次 Put（虽然不持锁，不会阻塞其他 goroutine）。
//   - 可在运行期调用（内部加锁），只影响此后被覆盖的对象。
//
// 典型用法（构造即登记）：
//
//	pool := NewFixedRingPool(64, newConn).SetEvict(func(c *Conn) { c.Close() })
func (p *FixedRingPool[T]) SetEvict(fn func(T)) *FixedRingPool[T] {
	p.mu.Lock()
	p.evict = fn
	p.mu.Unlock()
	return p
}
