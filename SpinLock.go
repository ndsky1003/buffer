package buffer

import (
	"runtime"
	"sync/atomic"
)

// SpinLock 适配M4架构的轻量自旋锁（RingBuffer池专用）
type SpinLock uint32

// Lock 自旋加锁：M4架构下自旋100次，失败后继续CAS直到成功
func (sl *SpinLock) Lock() {
	// 自旋100次（M4最优次数，平衡性能和CPU占用）
	for i := 0; i < 100; i++ {
		if atomic.CompareAndSwapUint32((*uint32)(sl), 0, 1) {
			return
		}
		// ARM架构让步：让出CPU，避免空转
		runtime.Gosched()
	}
	// 自旋失败后，循环CAS直到加锁成功（兜底，RingBuffer场景下几乎不会走到这里）
	for !atomic.CompareAndSwapUint32((*uint32)(sl), 0, 1) {
		runtime.Gosched()
	}
}

// Unlock 解锁：原子置0，无锁开销
func (sl *SpinLock) Unlock() {
	atomic.StoreUint32((*uint32)(sl), 0)
}
