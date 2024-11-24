package buffer

import (
	"bytes"
	"sync"
)

type buffer struct {
	bytes.Buffer
}

var pool = &sync.Pool{
	New: func() any {
		return &buffer{}
	},
}

func Get() *buffer {
	return pool.Get().(*buffer)
}

func Release(b *buffer) {
	b.Release()
}

func (this *buffer) Release() {
	if this == nil {
		return
	}
	this.Reset()
	pool.Put(this)
}

// 默认的copy一份,避免共用底层,修改会影响当前数据
// this.Buffer.Bytes() 返回没copy的data
func (this *buffer) Bytes() []byte {
	bs := this.Buffer.Bytes()
	b := make([]byte, len(bs))
	copy(b, bs)
	return b
}
