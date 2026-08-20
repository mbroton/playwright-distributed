package scheduler

import "sync"

type Waker struct {
	mu      sync.Mutex
	nextID  uint64
	waiters map[uint64]chan struct{}
}

func NewWaker() *Waker {
	return &Waker{waiters: map[uint64]chan struct{}{}}
}

func (w *Waker) Subscribe() (<-chan struct{}, func()) {
	w.mu.Lock()
	id := w.nextID
	w.nextID++
	waiter := make(chan struct{}, 1)
	w.waiters[id] = waiter
	w.mu.Unlock()

	return waiter, func() {
		w.mu.Lock()
		delete(w.waiters, id)
		w.mu.Unlock()
	}
}

func (w *Waker) Wake() {
	w.mu.Lock()
	defer w.mu.Unlock()

	for _, waiter := range w.waiters {
		select {
		case waiter <- struct{}{}:
		default:
		}
	}
}
