package event

import (
	"fmt"
	"sync"
)

type Factory func() Any

type EventLister interface {
	ListEvents() []Factory
}

type RootRegister interface {
	RegisterRoot(root EventLister)
}

type NewEventer interface {
	NewEventFactory(eventName string) (Factory, bool)
}

type Registry interface {
	RootRegister
	NewEventer
}

var GlobalRegistry = NewRegistry()

func NewRegistry() *eventRegistry {
	return &eventRegistry{
		eventFactories: make(map[string]Factory),
		registryMu:     sync.RWMutex{},
	}
}

type eventRegistry struct {
	eventFactories map[string]Factory
	registryMu     sync.RWMutex
}

var _ Registry = (*eventRegistry)(nil)

func (r *eventRegistry) NewEventFactory(eventName string) (Factory, bool) {
	r.registryMu.RLock()
	defer r.registryMu.RUnlock()

	factory, ok := r.eventFactories[eventName]
	return factory, ok
}

func (r *eventRegistry) RegisterRoot(root EventLister) {
	r.registryMu.Lock()
	defer r.registryMu.Unlock()

	for _, evFact := range root.ListEvents() {
		ev := evFact()
		eventName := ev.EventName()

		if _, ok := r.eventFactories[eventName]; ok {
			panic(fmt.Sprintf("duplicate event %q in registry, did you create multipler repos? (todo)", eventName))
		}

		r.eventFactories[eventName] = evFact
	}
}
