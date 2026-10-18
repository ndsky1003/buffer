package buffer

import (
	"bytes"
	"runtime"
)

// ShardedPool 分片缓冲池：将对象按 key 分散到多个内层 Pool，避免全局单锁争用，
// 使吞吐随分片数扩展。适用于高并发场景（游戏服、网关等）。
//
// 每个分片是一个独立的 Pool（含 EMA 校准 + AdaptiveRingPool）。
// 调用方需为「同一个对象的 Get/Put」传入相同的 key（如连接 ID 的哈希），
// 保证对象始终回到同一分片，避免跨分片漂移导致的额外分配。
type ShardedPool[T any] struct {
	shards []*Pool[T]
	mask   uint32
}

// NewShardedPool 创建分片池。
// shardCount 为分片数，<=0 时默认取 runtime.GOMAXPROCS(0)，并向上取整到 2 的幂（上限 1024）。
func NewShardedPool[T any](
	makeFunc func(uint64) T,
	resetFunc func(T) T,
	statFunc func(T) (uint64, uint64),
	shardCount int,
	opts ...*Option,
) *ShardedPool[T] {
	if shardCount <= 0 {
		shardCount = runtime.GOMAXPROCS(0)
	}
	shardCount = int(roundUpPow2(uint64(shardCount)))
	if shardCount > 1024 {
		shardCount = 1024
	}
	sp := &ShardedPool[T]{
		shards: make([]*Pool[T], shardCount),
		mask:   uint32(shardCount - 1),
	}
	for i := range sp.shards {
		sp.shards[i] = NewPool(makeFunc, resetFunc, statFunc, opts...)
	}
	return sp
}

// Get 取对象：按 key 路由到对应分片。
func (sp *ShardedPool[T]) Get(key uint32) T {
	return sp.shards[int(key&sp.mask)].Get()
}

// Put 归还对象：按 key 路由到对应分片（须与 Get 使用相同 key）。
func (sp *ShardedPool[T]) Put(key uint32, v T) {
	sp.shards[int(key&sp.mask)].Put(v)
}

// NewShardedBufferPool 创建 *bytes.Buffer 专用分片池。
func NewShardedBufferPool(shardCount int, opts ...*Option) *ShardedPool[*bytes.Buffer] {
	return NewShardedPool(
		func(size uint64) *bytes.Buffer {
			return bytes.NewBuffer(make([]byte, 0, size))
		},
		func(b *bytes.Buffer) *bytes.Buffer {
			b.Reset()
			return b
		},
		func(b *bytes.Buffer) (uint64, uint64) {
			if b == nil {
				return 0, 0
			}
			return uint64(b.Len()), uint64(b.Cap())
		},
		shardCount,
		opts...,
	)
}

// NewShardedBytePool 创建 []byte 专用分片池。
func NewShardedBytePool(shardCount int, opts ...*Option) *ShardedPool[[]byte] {
	return NewShardedPool(
		func(size uint64) []byte {
			return make([]byte, 0, size)
		},
		func(b []byte) []byte {
			return b[:0]
		},
		func(b []byte) (uint64, uint64) {
			if b == nil {
				return 0, 0
			}
			return uint64(len(b)), uint64(cap(b))
		},
		shardCount,
		opts...,
	)
}
