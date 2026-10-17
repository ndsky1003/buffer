package buffer

import "bytes"

// ClassPool 分级缓冲池：按对象大小自动路由到不同水位的内层池，
// 解决单池面对多峰流量（如 2KB 与 2MB 长期并存）时校准水位被大对象拉高、
// 小对象被迫占用大内存的问题。
//
// 每个内层池拥有独立的 EMA 校准水位，水位被限制在所属档位的 [minSize, maxSize] 内，
// 互不干扰。Get 按期望大小选择档位，Put 按对象实际容量归拢到对应档位。
type ClassPool[T any] struct {
	statFunc func(T) (used, cap uint64)
	roundUp  func(uint64) uint64 // 可选：Get 前对 size 向上取整（自动分桶用）
	classes  []*classPool[T]
}

type classPool[T any] struct {
	maxSize uint64 // 本档上限（不含）
	pool    *Pool[T]
}

// NewClassPool 创建分级池。
//
//   - makeFunc/resetFunc/statFunc：与 Pool 相同的适配函数。
//   - boundaries：升序排列的分界（字节）。例如 {64 << 10, 1 << 20} 生成三档：
//     [minSize, 64K)、[64K, 1M)、[1M, maxSize]。
//   - opts：作用于每个内层池的公共配置；各档的 MinSize/MaxSize 由分界自动推导并覆盖，
//     因此不要依赖 opts 中的 MinSize/MaxSize。
//
// 越界或不递增的分界会被忽略。
func NewClassPool[T any](
	makeFunc func(uint64) T,
	resetFunc func(T) T,
	statFunc func(T) (uint64, uint64),
	boundaries []uint64,
	opts ...*Option,
) *ClassPool[T] {
	cp := &ClassPool[T]{statFunc: statFunc}

	// 解析默认/用户配置，取出首档下限与末档上限。
	base := Options().
		SetMinSize(512).
		SetMaxSize(64 << 20).
		Merge(opts...)
	minSize := *base.MinSize
	maxSize := *base.MaxSize

	low := minSize
	for _, b := range boundaries {
		if b <= low || b > maxSize {
			continue // 忽略不递增或越界的分界
		}
		cp.classes = append(cp.classes, &classPool[T]{
			maxSize: b,
			pool:    newClassPool(makeFunc, resetFunc, statFunc, low, b, opts...),
		})
		low = b
	}
	// 末档：下限为最后一个分界，上限为全局 maxSize。
	cp.classes = append(cp.classes, &classPool[T]{
		maxSize: maxSize,
		pool:    newClassPool(makeFunc, resetFunc, statFunc, low, maxSize, opts...),
	})
	return cp
}

func newClassPool[T any](
	makeFunc func(uint64) T,
	resetFunc func(T) T,
	statFunc func(T) (uint64, uint64),
	minS, maxS uint64,
	opts ...*Option,
) *Pool[T] {
	opt := Options().Merge(opts...)
	opt.SetMinSize(minS)
	opt.SetMaxSize(maxS)
	return New(makeFunc, resetFunc, statFunc, &opt)
}

// Get 按期望大小 size 选择对应档位。
func (cp *ClassPool[T]) Get(size uint64) T {
	if cp.roundUp != nil {
		size = cp.roundUp(size)
	}
	for _, c := range cp.classes {
		if size < c.maxSize {
			return c.pool.Get()
		}
	}
	return cp.classes[len(cp.classes)-1].pool.Get()
}

// Put 按对象实际容量归拢到对应档位。
func (cp *ClassPool[T]) Put(v T) {
	_, capVal := cp.statFunc(v)
	if capVal == 0 {
		return
	}
	for _, c := range cp.classes {
		if capVal < c.maxSize {
			c.pool.Put(v)
			return
		}
	}
	cp.classes[len(cp.classes)-1].pool.Put(v)
}

// NewBufferClassPool 创建 *bytes.Buffer 专用分级池。
func NewBufferClassPool(boundaries []uint64, opts ...*Option) *ClassPool[*bytes.Buffer] {
	return NewClassPool(
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
		boundaries,
		opts...,
	)
}

// NewByteClassPool 创建 []byte 专用分级池。
func NewByteClassPool(boundaries []uint64, opts ...*Option) *ClassPool[[]byte] {
	return NewClassPool(
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
		boundaries,
		opts...,
	)
}

// NewBufferAutoClassPool 创建 *bytes.Buffer 专用自动分级池。
// 按 2 的幂次自动分桶（512B、1K、2K、4K…），无需手动指定分界：
// 任何大小的对象都会落到最接近的 2 的幂桶，内存误差不超过 2 倍。
func NewBufferAutoClassPool(opts ...*Option) *ClassPool[*bytes.Buffer] {
	base := Options().SetMinSize(512).SetMaxSize(64 << 20).Merge(opts...)
	cp := NewBufferClassPool(pow2Boundaries(*base.MinSize, *base.MaxSize), opts...)
	cp.roundUp = roundUpPow2
	return cp
}

// NewByteAutoClassPool 创建 []byte 专用自动分级池，分桶规则同 NewBufferAutoClassPool。
func NewByteAutoClassPool(opts ...*Option) *ClassPool[[]byte] {
	base := Options().SetMinSize(512).SetMaxSize(64 << 20).Merge(opts...)
	cp := NewByteClassPool(pow2Boundaries(*base.MinSize, *base.MaxSize), opts...)
	cp.roundUp = roundUpPow2
	return cp
}

// pow2Boundaries 生成「严格大于 minSize 且严格小于 maxSize」的 2 的幂序列，作为分桶分界。
func pow2Boundaries(minSize, maxSize uint64) []uint64 {
	var bs []uint64
	s := uint64(1)
	for s <= minSize {
		s <<= 1
	}
	for ; s < maxSize; s <<= 1 {
		bs = append(bs, s)
	}
	return bs
}

// roundUpPow2 将 v 向上取整到 2 的幂（v>=1）。
func roundUpPow2(v uint64) uint64 {
	if v <= 1 {
		return 1
	}
	v--
	v |= v >> 1
	v |= v >> 2
	v |= v >> 4
	v |= v >> 8
	v |= v >> 16
	v |= v >> 32
	return v + 1
}
