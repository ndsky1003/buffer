package buffer

import "bytes"

// NewBufferPool 创建 *bytes.Buffer 专用池
func NewBufferPool(opts ...*Option) *Pool[*bytes.Buffer] {
	return New(
		// make
		func(size uint64) *bytes.Buffer {
			return bytes.NewBuffer(make([]byte, 0, size))
		},
		// reset
		func(b *bytes.Buffer) *bytes.Buffer {
			b.Reset()
			return b
		},
		// stat
		func(b *bytes.Buffer) (uint64, uint64) {
			return uint64(b.Len()), uint64(b.Cap())
		},
		opts...,
	)
}

// NewBytePool 创建 []byte 专用池
func NewBytePool(opts ...*Option) *Pool[[]byte] {
	return New(
		// make: 直接 make slice
		func(size uint64) []byte {
			return make([]byte, 0, size)
		},
		// reset: 必须 reslice 为 0，并返回新的 slice header
		func(b []byte) []byte {
			return b[:0]
		},
		// stat: 使用内置 len/cap
		func(b []byte) (uint64, uint64) {
			return uint64(len(b)), uint64(cap(b))
		},
		opts...,
	)
}
