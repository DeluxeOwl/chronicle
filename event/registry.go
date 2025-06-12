package event

import (
	"sync"
)

type EventLister interface {
	ListEvents() []EventAny
}

type RootRegister interface {
	RegisterRoot(root EventLister)
}

type NewEventer interface {
	NewEvent(eventName string) (func() EventAny, bool)
}

type Registry interface {
	RootRegister
	NewEventer
}

var GlobalRegistry = NewRegistry()

func NewRegistry() *eventRegistry {
	return &eventRegistry{
		eventFactories: make(map[string]func() EventAny),
		registryMu:     sync.RWMutex{},
	}
}

type eventRegistry struct {
	eventFactories map[string]func() EventAny
	registryMu     sync.RWMutex
}

var _ Registry = (*eventRegistry)(nil)

func (r *eventRegistry) NewEvent(eventName string) (func() EventAny, bool) {
	r.registryMu.RLock()
	defer r.registryMu.RUnlock()

	factory, ok := r.eventFactories[eventName]
	return factory, ok
}

// Registers the aggregate root with its events.
func (r *eventRegistry) RegisterRoot(root EventLister) {
	r.registryMu.Lock()
	defer r.registryMu.Unlock()

	for _, ev := range root.ListEvents() {
		// TODO: bug - this returns the same instance, which means that json is marshaled/unmarshaled overwriting previous
		// Maybe the register needs to register functions
		r.eventFactories[ev.EventName()] = func() EventAny {
			return ev
		}
	}
}
