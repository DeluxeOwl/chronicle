package event

import (
	"reflect"
	"sync"
)

type RegisterFunc func(ev EventAny)

type Registerer interface {
	RegisterEvents(r RegisterFunc)
}

type RootRegister interface {
	RegisterRoot(root Registerer)
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
		eventFactories:      make(map[string]func() EventAny),
		registryMu:          sync.RWMutex{},
		typeRegistrations:   make(map[reflect.Type]*sync.Once),
		typeRegistrationsMu: sync.Mutex{},
	}
}

type eventRegistry struct {
	eventFactories      map[string]func() EventAny
	registryMu          sync.RWMutex
	typeRegistrations   map[reflect.Type]*sync.Once
	typeRegistrationsMu sync.Mutex
}

var _ Registry = (*eventRegistry)(nil)

func (r *eventRegistry) registerEvent(ev EventAny) {
	r.registryMu.Lock()
	defer r.registryMu.Unlock()

	// TODO: bug - this returns the same instance, which means that json is marshaled/unmarshaled overwriting previous
	// Maybe the register needs to register functions
	r.eventFactories[ev.EventName()] = func() EventAny {
		return ev
	}
}

func (r *eventRegistry) NewEvent(eventName string) (func() EventAny, bool) {
	r.registryMu.RLock()
	defer r.registryMu.RUnlock()

	factory, ok := r.eventFactories[eventName]
	return factory, ok
}

// Registers the aggregate root with its events.
func (r *eventRegistry) RegisterRoot(root Registerer) {
	var aggType reflect.Type
	val := reflect.ValueOf(root)
	if val.Kind() == reflect.Ptr {
		aggType = val.Elem().Type()
	} else {
		aggType = val.Type()
	}

	r.typeRegistrationsMu.Lock()
	once, ok := r.typeRegistrations[aggType]
	if !ok {
		once = new(sync.Once)
		r.typeRegistrations[aggType] = once
	}
	r.typeRegistrationsMu.Unlock()

	// This will execute root.RegisterEvents(registry.Register) only once
	// for the given aggregate root, across all instances and goroutines.
	once.Do(func() {
		root.RegisterEvents(r.registerEvent)
	})
}
