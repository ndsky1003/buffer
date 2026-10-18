package buffer

import (
	"bytes"
	"runtime"
)

// ShardedClassPool 分片分级缓冲池：ShardedPool 与 ClassPool 的组合。
//
// 外层按 key 分片（避免单锁争用、吞吐随并发扩展），
// 内层每个分片是一个 ClassPool（按对象大小分档，应对多峰流量）。
// 适用于「高并发 + 流量大小多峰」的场景——典型如 RPC 网络库：
// 既有小的心跳/ACK 包，又有大的 payload，且每连接并发处理。
//
// 与 ShardedPool 相同：调用方需为同一对象的 Get/Put 传入相同的 key（如连接 ID 的哈希），
// 保证对象始终回到同一分片，避免跨分片漂移。
//
// 注意内存总量为「分片数 × 档数 × 每档 buffer 数」，但 buffer 均惰性创建
// （池空时才 New），因此实际内存取决于真实流量而非满配。
type ShardedClassPool[T any] struct {
	shards []*ClassPool[T]
	mask   uint32
}

// newShardedClassPool 通用构造。
// boundaries 为分级分界；roundUp 非 nil 时表示自动分桶（此时 boundaries 应为 pow2Boundaries 的结果）。
func newShardedClassPool[T any](
	makeFunc func(uint64) T,
	resetFunc func(T) T,
	statFunc func(T) (uint64, uint64),
	shardCount int,
	boundaries []uint64,
	roundUp func(uint64) uint64,
	opts ...*Option,
) *ShardedClassPool[T] {
	if shardCount <= 0 {
		shardCount = runtime.GOMAXPROCS(0)
	}
	shardCount = int(roundUpPow2(uint64(shardCount)))
	if shardCount > 1024 {
		shardCount = 1024
	}
	shardCount = min(shardCount, 1024)
	sp := &ShardedClassPool[T]{
		shards: make([]*ClassPool[T], shardCount),
		mask:   uint32(shardCount - 1),
	}
	for i := range sp.shards {
		cp := NewClassPool(makeFunc, resetFunc, statFunc, boundaries, opts...)
		cp.roundUp = roundUp
		sp.shards[i] = cp
	}
	return sp
}

// NewShardedClassPool 创建手动分界的分片分级池。
// boundaries 语义同 NewClassPool（升序分界，越界/不递增被忽略）。
func NewShardedClassPool[T any](
	makeFunc func(uint64) T,
	resetFunc func(T) T,
	statFunc func(T) (uint64, uint64),
	shardCount int,
	boundaries []uint64,
	opts ...*Option,
) *ShardedClassPool[T] {
	return newShardedClassPool(makeFunc, resetFunc, statFunc, shardCount, boundaries, nil, opts...)
}

// NewShardedAutoClassPool 创建按 2 的幂自动分桶的分片分级池（无需指定分界，误差 ≤2×）。
func NewShardedAutoClassPool[T any](
	makeFunc func(uint64) T,
	resetFunc func(T) T,
	statFunc func(T) (uint64, uint64),
	shardCount int,
	opts ...*Option,
) *ShardedClassPool[T] {
	base := Options().SetMinSize(512).SetMaxSize(64 << 20).Merge(opts...)
	boundaries := pow2Boundaries(*base.MinSize, *base.MaxSize)
	return newShardedClassPool(makeFunc, resetFunc, statFunc, shardCount, boundaries, roundUpPow2, opts...)
}

// Get 取对象：按 key 选分片，按期望大小 size 选档。
func (sp *ShardedClassPool[T]) Get(key uint32, size uint64) T {
	return sp.shards[int(key&sp.mask)].Get(size)
}

// Put 归还对象：按 key 选分片，按对象实际容量归拢（须与 Get 使用相同 key）。
func (sp *ShardedClassPool[T]) Put(key uint32, v T) {
	sp.shards[int(key&sp.mask)].Put(v)
}

// NewShardedBufferClassPool 创建 *bytes.Buffer 专用分片分级池（手动分界）。
func NewShardedBufferClassPool(shardCount int, boundaries []uint64, opts ...*Option) *ShardedClassPool[*bytes.Buffer] {
	return NewShardedClassPool(
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
		boundaries,
		opts...,
	)
}

// NewShardedByteClassPool 创建 []byte 专用分片分级池（手动分界）。
func NewShardedByteClassPool(shardCount int, boundaries []uint64, opts ...*Option) *ShardedClassPool[[]byte] {
	return NewShardedClassPool(
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
		boundaries,
		opts...,
	)
}

// NewShardedBufferAutoClassPool 创建 *bytes.Buffer 专用分片分级池（按 2 的幂自动分桶）。
func NewShardedBufferAutoClassPool(shardCount int, opts ...*Option) *ShardedClassPool[*bytes.Buffer] {
	return NewShardedAutoClassPool(
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

// NewShardedByteAutoClassPool 创建 []byte 专用分片分级池（按 2 的幂自动分桶）。
func NewShardedByteAutoClassPool(shardCount int, opts ...*Option) *ShardedClassPool[[]byte] {
	return NewShardedAutoClassPool(
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
