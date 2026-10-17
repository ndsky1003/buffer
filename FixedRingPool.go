package buffer

// FixedRingPool 固定容量的环形对象池。
//
// 与 AdaptiveRingPool 的区别：容量固定、不伸缩、无命中率统计，
// 因此热路径更短（省去 getCount/hitCount 的原子操作和 autoScale 的浮点除法）。
// 实测相比 AdaptiveRingPool 快约 16%~21%。
// 队列满时采用「覆盖最旧」（而非丢弃），使池子始终保持为最新归还的对象，
// 有助于复用大容量 buffer、减少 grow。
// 适用于容量可预先确定、追求极致吞吐的场景。
type FixedRingPool[T any] struct {
	mu       SpinLock // 锁
	head     int      // 取数位置
	tail     int      // 放数位置
	count    int      // 空闲数
	capacity int      // 固定容量
	New      func() T // 创建函数（池空时调用）
	buffer   []T      // 环形队列
}

// NewFixedRingPool 创建固定容量的环形对象池。
func NewFixedRingPool[T any](capacity int, newFunc func() T) *FixedRingPool[T] {
	if capacity < 1 {
		capacity = 1
	}
	return &FixedRingPool[T]{
		capacity: capacity,
		buffer:   make([]T, capacity),
		New:      newFunc,
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
	return p.New()
}

// Put 归还对象：队列未满则放回；已满则覆盖最旧的对象（淘汰 head，保持池子始终为最新状态）。
func (p *FixedRingPool[T]) Put(obj T) {
	p.mu.Lock()
	if p.count < p.capacity {
		p.buffer[p.tail] = obj
		p.tail = (p.tail + 1) % p.capacity
		p.count++
	} else {
		// 队列已满：覆盖最旧（head），head/tail 同步前进，count 保持满
		p.buffer[p.head] = obj
		p.head = (p.head + 1) % p.capacity
		p.tail = p.head
	}
	p.mu.Unlock()
}
