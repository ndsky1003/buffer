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

func (this *buffer) Release() {
	if this == nil {
		return
	}
	this.Reset()
	pool.Put(this)
}
