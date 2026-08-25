package media

import (
	"sync"
)

type Queue[T any] struct {
	mu       sync.Mutex
	cond     *sync.Cond
	items    []T
	capacity int
	policy   OverflowPolicy
	closed   bool
	dropped  uint64
}

func NewQueue[T any](capacity int, policy OverflowPolicy) *Queue[T] {
	if capacity <= 0 {
		capacity = 32
	}
	q := &Queue[T]{capacity: capacity, policy: policy}
	q.cond = sync.NewCond(&q.mu)
	return q
}

func (q *Queue[T]) Push(item T) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return ErrClosed
	}
	if len(q.items) >= q.capacity {
		q.dropped++
		switch q.policy {
		case OverflowFail:
			return ErrOverflow
		default:
			q.items = q.items[1:]
		}
	}
	q.items = append(q.items, item)
	q.cond.Signal()
	return nil
}

func (q *Queue[T]) Pop() (T, bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.items) == 0 && !q.closed {
		q.cond.Wait()
	}
	var zero T
	if len(q.items) == 0 {
		return zero, false, nil
	}
	item := q.items[0]
	q.items = q.items[1:]
	return item, true, nil
}

func (q *Queue[T]) Close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	q.closed = true
	q.cond.Broadcast()
}

func (q *Queue[T]) Reset() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.items = nil
	q.closed = false
}

func (q *Queue[T]) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items)
}

func (q *Queue[T]) Dropped() uint64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.dropped
}
