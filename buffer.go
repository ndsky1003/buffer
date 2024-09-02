package buffer

import (
	"bytes"
	"sync"
)

var pool = &sync.Pool{
	New: func() any {
		return &bytes.Buffer{}
	},
}

func Get() *bytes.Buffer {
	return pool.Get().(*bytes.Buffer)
}

func Release(buf *bytes.Buffer) {
	if buf == nil {
		return
	}
	buf.Reset()
	pool.Put(buf)
}
