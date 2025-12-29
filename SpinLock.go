package buffer

import (
	"runtime"
	"sync/atomic"
)

type SpinLock uint32

func (sl *SpinLock) Lock() {
	for i := 0; i < 200; i++ {
		if atomic.CompareAndSwapUint32((*uint32)(sl), 0, 1) {
			return
		}
		runtime.Gosched()
	}
	for !atomic.CompareAndSwapUint32((*uint32)(sl), 0, 1) {
		runtime.Gosched()
	}
}

func (sl *SpinLock) Unlock() {
	atomic.StoreUint32((*uint32)(sl), 0)
}
