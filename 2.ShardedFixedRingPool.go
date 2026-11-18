package buffer

import "runtime"

// ShardedFixedRingPool 分片固定容量池：外层按 key 分片，内层每片一个 FixedRingPool。
// 兼具 FixedRingPool 的「极简热路径 + 固定容量 + 满时覆盖最旧」与分片的「争用随分片数下降」，
// 适用于「高并发 + 容量可预先确定」的场景。
//
// 与 ShardedPool（内层 Pool 含 EMA 校准 + reset）不同：本池无校准、无 reset，
// Get 复用对象时不做清理，调用方须自行 reset——
//   - *bytes.Buffer：Get 后调用 b.Reset()
//   - []byte：Put 时传 b[:0]（否则下次 Get 复用的 slice 的 len 非 0，残留旧数据）
//
// key 语义同 ShardedPool：同一对象的 Get/Put 必须传相同 key（如连接 ID 的哈希），
// 保证对象始终回到同一分片，避免跨分片漂移。
type ShardedFixedRingPool[T any] struct {
	shards []*FixedRingPool[T]
	mask   uint32
}

// NewShardedFixedRingPool 创建分片固定容量池。
// shardCount 为分片数，<=0 时取 GOMAXPROCS 并向上取整到 2 的幂（上限 1024）。
// capacity 为每片 FixedRingPool 的容量（每片最多缓存多少个对象）。
// newFunc 为池空时创建对象的函数（决定对象的初始大小）。
func NewShardedFixedRingPool[T any](shardCount, capacity int, newFunc func() T) *ShardedFixedRingPool[T] {
	if shardCount <= 0 {
		shardCount = runtime.GOMAXPROCS(0)
	}
	shardCount = int(roundUpPow2(uint64(shardCount)))
	// if shardCount > 1024 {
	// 	shardCount = 1024
	// }
	shardCount = min(1024, shardCount)
	sp := &ShardedFixedRingPool[T]{
		shards: make([]*FixedRingPool[T], shardCount),
		mask:   uint32(shardCount - 1),
	}
	for i := range sp.shards {
		sp.shards[i] = NewFixedRingPool(capacity, newFunc)
	}
	return sp
}

// Get 取对象：按 key 路由到对应分片（取 key 的低 log2(shardCount) 位，
// 分片键语义同 ShardedPool 的 uint32 key）。
// 若 key 低位分布不均（如时间戳），可先用 Hash 混合。返回的对象可能残留旧数据，调用方须自行 reset。
func (sp *ShardedFixedRingPool[T]) Get(key uint32) T {
	return sp.shards[int(key&sp.mask)].Get()
}

// Put 归还对象：按 key 路由到对应分片（须与 Get 使用相同 key）。
func (sp *ShardedFixedRingPool[T]) Put(key uint32, obj T) {
	sp.shards[int(key&sp.mask)].Put(obj)
}

// SetEvict 为**所有分片**登记淘汰回调（语义同 FixedRingPool.SetEvict）。返回自身以便链式调用。
//
// 池化需要显式释放的资源（fd、CGO 内存、连接、外部句柄）时必须设置：
// 每个分片有独立的 capacity，任一分片满时会覆盖最旧，
// 被淘汰的对象若没有登记回调就会被直接丢弃 → 资源泄漏。
//
// 建议在开始并发使用前调用；运行期调用也安全（逐片加锁），
// 但只影响此后被覆盖的对象。传 nil 清除回调。
//
// 典型用法（构造即登记）：
//
//	pool := NewShardedFixedRingPool(0, 64, newConn).SetEvict(func(c *Conn) { c.Close() })
func (sp *ShardedFixedRingPool[T]) SetEvict(fn func(T)) *ShardedFixedRingPool[T] {
	for _, shard := range sp.shards {
		shard.SetEvict(fn)
	}
	return sp
}
