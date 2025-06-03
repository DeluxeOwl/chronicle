package registry

import (
	"reflect"
	"sync"

	"github.com/DeluxeOwl/eventuallynow/event"
)

// TODO: what if not global, but injected

type RegisterFunc func(ev event.EventAny)

type aggRoot interface {
	RegisterEvents(r RegisterFunc)
}

type RootRegister interface {
	RegisterRoot(root aggRoot)
}

type NewEventer interface {
	NewEvent(eventName string) (func() event.EventAny, bool)
}

type EventRegistry interface {
	RootRegister
	NewEventer
}

var GlobalEventRegistry = NewEventRegistry()

func NewEventRegistry() *eventRegistry {
	return &eventRegistry{
		eventFactories:      make(map[string]func() event.EventAny),
		registryMu:          sync.RWMutex{},
		typeRegistrations:   make(map[reflect.Type]*sync.Once),
		typeRegistrationsMu: sync.Mutex{},
	}
}

type eventRegistry struct {
	eventFactories      map[string]func() event.EventAny
	registryMu          sync.RWMutex
	typeRegistrations   map[reflect.Type]*sync.Once
	typeRegistrationsMu sync.Mutex
}

var _ EventRegistry = (*eventRegistry)(nil)

func (r *eventRegistry) registerEvent(ev event.EventAny) {
	r.registryMu.Lock()
	defer r.registryMu.Unlock()

	r.eventFactories[ev.EventName()] = func() event.EventAny {
		return ev
	}
}

func (r *eventRegistry) NewEvent(eventName string) (func() event.EventAny, bool) {
	r.registryMu.RLock()
	defer r.registryMu.RUnlock()

	factory, ok := r.eventFactories[eventName]
	return factory, ok
}

// Registers the aggregate root with its events.
func (r *eventRegistry) RegisterRoot(root aggRoot) {
	var aggType reflect.Type
	val := reflect.ValueOf(root)
	if val.Kind() == reflect.Ptr {
		aggType = val.Elem().Type()
	} else {
		aggType = val.Type() // Should not happen with current design if Root implies methods with ptr receivers
	}

	r.typeRegistrationsMu.Lock()
	once, ok := r.typeRegistrations[aggType]
	if !ok {
		once = new(sync.Once)
		r.typeRegistrations[aggType] = once
	}
	r.typeRegistrationsMu.Unlock()

	// This will execute root.RegisterEvents(registry.Register) only once
	// for the given aggType, across all instances and goroutines.
	once.Do(func() {
		root.RegisterEvents(r.registerEvent)
	})
}
