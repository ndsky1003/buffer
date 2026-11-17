package buffer

import "runtime"

// integer 约束所有有符号/无符号整型，用作分片 key 的类型约束。
type integer interface {
	~int | ~int8 | ~int16 | ~int32 | ~int64 |
		~uint | ~uint8 | ~uint16 | ~uint32 | ~uint64 | ~uintptr
}

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

// Get 取对象：按 key 路由到对应分片。key 可为任意整型（int64、int、uint32 等），
// 内部取低 32 位路由（分片键语义，同 ShardedPool 的 uint32 key）。
// 若 key 低位分布不均（如时间戳），可先用 Hash 混合。返回的对象可能残留旧数据，调用方须自行 reset。
func (sp *ShardedFixedRingPool[T]) Get(key string) T {
	return sp.shards[int(HashString(key)&sp.mask)].Get()
}

// Put 归还对象：按 key 路由到对应分片（须与 Get 使用相同 key）。
func (sp *ShardedFixedRingPool[T]) Put(key string, obj T) {
	sp.shards[int(HashString(key)&sp.mask)].Put(obj)
}
