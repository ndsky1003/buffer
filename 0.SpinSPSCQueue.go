package buffer

import (
	"runtime"
	"sync/atomic"

	"golang.org/x/sys/cpu"
)

// SpinSPSCQueue 无锁 SPSC（单生产者单消费者）环形队列。
// 无锁、无信号量、无条件变量；用于在两个 goroutine 之间搬运对象
// （典型如日志采集 → 落盘、单连接读写 → 处理、pipeline 环节间传递）。
//
// 并发模型：
//   - w 只有生产者写、消费者只读；r 只有消费者写、生产者只读。
//   - 生产者「先写槽位、再发布 w」；消费者「先读 w、再读槽位、最后发布 r」。
//     这个顺序**不能反**——若先发布 r 再读槽位，生产者会以为该槽已被消费、
//     在回绕时覆盖它，导致数据竞争（读到错数据）。
//   - 满判 w-r >= cap、空判 r == w；读到对方下标过期值也安全（只会更保守）。
//
// 内存模型：Go sync/atomic 是顺序一致，w/r 的原子读写作同步边，
// 既发布槽位数据、也保护槽位复用（含回绕覆盖），因此无数据竞争。
//
// 元素类型是 *T（指针）：对象由调用方分配，入队后所有权移交消费者，
// 调用方不得在入队后再修改该对象。存指针是为了避免拷贝大 T。
type SpinSPSCQueue[T any] struct {
	cap   uint64
	mask  uint64
	slots []*T

	closed atomic.Bool // 关闭标志：Close 后 Enqueue 拒绝，Dequeue 排空后返回 nil

	w uint64 // 生产者独占写（单调递增，永不清零，用 &mask 取槽位）
	// 用 cpu.CacheLinePad 把 w 与 r 隔到不同缓存行，避免生产者写 w / 消费者写 r 时的缓存行乒乓（伪共享）。
	// 注意这跟对象池里「字段紧凑排列、填充有害」的结论相反：池里锁已串行化访问，
	// 而 SPSC 是生产/消费者各占一个核、各写各的下标。
	_ cpu.CacheLinePad

	r uint64 // 消费者独占读
	_ cpu.CacheLinePad
}

// NewSpinSPSCQueue 创建 SPSC 队列。cap 向上取整到 2 的幂；cap<=0 时取 128。
func NewSpinSPSCQueue[T any](cap uint64) *SpinSPSCQueue[T] {
	if cap == 0 {
		cap = 128
	}
	c := uint64(1)
	for c < cap {
		c <<= 1
	}
	return &SpinSPSCQueue[T]{
		cap:   c,
		mask:  c - 1,
		slots: make([]*T, c),
	}
}

// Enqueue 入队。队列满或已关闭返回 false；成功返回 true。
// 调用方须自行处理「满」：重试（自旋/稍后再试）、丢弃、或触发背压。
func (q *SpinSPSCQueue[T]) Enqueue(item *T) bool {
	if q.closed.Load() {
		return false
	}
	w := q.w // 生产者自己的下标，普通读即可（单写者）
	r := atomic.LoadUint64(&q.r)
	if w-r >= q.cap {
		return false
	}
	q.slots[w&q.mask] = item
	atomic.StoreUint64(&q.w, w+1) // 先写槽位、后发布 w，顺序不能反
	return true
}

// Dequeue 出队。队列空时自旋等待（Gosched 让出 CPU，降低空转烧核）；
// 关闭且排空后返回 nil。
//
// 延迟权衡：空转时用 runtime.Gosched 让出而非死转，省 CPU 但引入唤醒延迟（µs 级）。
// 若追求极低延迟，可改为有限次自旋再退让。
func (q *SpinSPSCQueue[T]) Dequeue() *T {
	for {
		w := atomic.LoadUint64(&q.w)
		r := q.r // 消费者自己的下标，普通读即可
		if r == w {
			if q.closed.Load() {
				return nil
			}
			runtime.Gosched()
			continue
		}
		idx := r & q.mask
		item := q.slots[idx]
		q.slots[idx] = nil // 先读并清空槽位、再发布 r：清空防 GC 滞留，且顺序不能反
		atomic.StoreUint64(&q.r, r+1)
		return item
	}
}

// TryDequeue 非阻塞出队。空（或已关闭且排空）返回 nil，否则返回队首元素。
// 用 nil 表示「空」：若 T 本身可能为合法的 nil 元素，请结合 Closed() 区分状态。
func (q *SpinSPSCQueue[T]) TryDequeue() *T {
	w := atomic.LoadUint64(&q.w)
	r := q.r
	if r == w {
		return nil
	}
	idx := r & q.mask
	item := q.slots[idx]
	q.slots[idx] = nil
	atomic.StoreUint64(&q.r, r+1)
	return item
}

// Close 关闭队列：此后 Enqueue 拒绝新元素，Dequeue/TryDequeue 在排空后返回 nil。
// Close 不会等待已入队元素被消费完，也不阻塞正在进行的 Enqueue/Dequeue。
// 调用方应确保 Close 前生产者已停止入队，否则与在途 Enqueue 的竞态会丢失个别元素。
func (q *SpinSPSCQueue[T]) Close() {
	q.closed.Store(true)
}

// Closed 报告队列是否已关闭。
func (q *SpinSPSCQueue[T]) Closed() bool {
	return q.closed.Load()
}
