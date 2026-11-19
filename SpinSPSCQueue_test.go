package buffer

import (
	"runtime"
	"sync/atomic"
	"testing"
)

// 基本 FIFO 顺序 + TryDequeue 空返回 nil
func TestSpinSPSCQueueBasic(t *testing.T) {
	q := NewSpinSPSCQueue[int](8)
	if q.TryDequeue() != nil {
		t.Fatal("空队列 TryDequeue 应返回 nil")
	}
	for i := 0; i < 8; i++ {
		v := i
		if !q.Enqueue(&v) {
			t.Fatalf("第 %d 个应入队成功", i)
		}
	}
	for i := 0; i < 8; i++ {
		item := q.Dequeue()
		if item == nil || *item != i {
			t.Fatalf("第 %d 个出队=%v，期望 %d", i, item, i)
		}
	}
	if q.TryDequeue() != nil {
		t.Fatal("排空后 TryDequeue 应返回 nil")
	}
}

// 满/空边界 + cap 向上取整到 2 的幂
func TestSpinSPSCQueueBoundary(t *testing.T) {
	q := NewSpinSPSCQueue[int](3) // 向上取整到 4
	if q.cap != 4 {
		t.Fatalf("cap=%d，期望 4（向上取整到 2 的幂）", q.cap)
	}
	for i := 0; i < 4; i++ {
		v := i
		if !q.Enqueue(&v) {
			t.Fatalf("第 %d 个应入队成功", i)
		}
	}
	v := 99
	if q.Enqueue(&v) {
		t.Fatal("队列满时应返回 false")
	}
	// 出队一个，腾出一个槽位后可再入队一个
	if item := q.Dequeue(); item == nil || *item != 0 {
		t.Fatalf("Dequeue=%v，期望 0", item)
	}
	if !q.Enqueue(&v) {
		t.Fatal("出队后应能再入队")
	}
}

// 槽位出队后清空：防止已出队对象被 GC 滞留
func TestSpinSPSCQueueSlotReleased(t *testing.T) {
	q := NewSpinSPSCQueue[int](4)
	v := 1
	q.Enqueue(&v)
	if q.slots[0] == nil {
		t.Fatal("入队后槽位应持有元素")
	}
	if item := q.Dequeue(); item == nil || *item != 1 {
		t.Fatalf("Dequeue=%v，期望 1", item)
	}
	if q.slots[0] != nil {
		t.Fatal("出队后槽位应清空为 nil（防止 GC 滞留）")
	}
}

// 关闭语义：Close 后 Enqueue 拒绝、Dequeue 排空后返回 nil
func TestSpinSPSCQueueClose(t *testing.T) {
	q := NewSpinSPSCQueue[int](8)
	for i := 0; i < 3; i++ {
		v := i
		if !q.Enqueue(&v) {
			t.Fatalf("关闭前第 %d 个应入队成功", i)
		}
	}
	q.Close()
	if !q.Closed() {
		t.Fatal("Closed() 应为 true")
	}
	v := 99
	if q.Enqueue(&v) {
		t.Fatal("关闭后 Enqueue 应返回 false")
	}
	for i := 0; i < 3; i++ {
		if item := q.Dequeue(); item == nil || *item != i {
			t.Fatalf("关闭后第 %d 个出队=%v，期望 %d", i, item, i)
		}
	}
	if q.Dequeue() != nil {
		t.Fatal("关闭且排空后 Dequeue 应返回 nil")
	}
	if q.TryDequeue() != nil {
		t.Fatal("关闭且排空后 TryDequeue 应返回 nil")
	}
}

// 并发 FIFO：单生产者按序入队、单消费者按序出队，顺序必须严格 0..N-1。
// 在 -race 下能捕获「先发布 r 再读槽位」这类回绕覆盖的数据竞争。
func TestSpinSPSCQueueFIFOConcurrent(t *testing.T) {
	const N = 500000
	q := NewSpinSPSCQueue[int](64)
	var produced atomic.Int64

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < N; i++ {
			v := i
			for !q.Enqueue(&v) { // 满则自旋重试（配合消费者腾位）
				runtime.Gosched()
			}
		}
		produced.Store(N)
	}()

	for i := 0; i < N; i++ {
		item := q.Dequeue()
		if item == nil {
			t.Fatalf("第 %d 个出队为 nil", i)
		}
		if *item != i {
			t.Fatalf("第 %d 个出队=%d，期望 %d（顺序被破坏）", i, *item, i)
		}
	}
	<-done
	if produced.Load() != N {
		t.Fatalf("生产者只完成 %d/%d", produced.Load(), N)
	}
}
