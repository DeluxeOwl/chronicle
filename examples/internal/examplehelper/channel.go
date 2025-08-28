package examplehelper

import "sync"

// ⚠️ This is used for example purposes only
type PubSubMemory[T any] struct {
	mu   sync.RWMutex
	subs []chan T
}

// ⚠️ This is used for example purposes only
func NewPubSubMemory[T any]() *PubSubMemory[T] {
	return &PubSubMemory[T]{
		mu:   sync.RWMutex{},
		subs: []chan T{},
	}
}

func (ps *PubSubMemory[T]) Subscribe() chan T {
	ps.mu.Lock()
	defer ps.mu.Unlock()

	ch := make(chan T)

	ps.subs = append(ps.subs, ch)

	return ch
}

func (ps *PubSubMemory[T]) Publish(msg T) {
	ps.mu.RLock()
	defer ps.mu.RUnlock()

	for _, ch := range ps.subs {
		ch <- msg
	}
}
