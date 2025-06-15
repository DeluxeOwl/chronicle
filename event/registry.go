package event

import (
	"fmt"
	"reflect"
	"sync"

	"github.com/DeluxeOwl/chronicle/internal/assert"
)

// The core purpose of this type is to create a new, zero-value instance of a specific event type.
// This instance is then used as a target for unmarshaling data from the event store.
// It acts as a bridge between a string EventName and a concrete Go type.
type NewFunc func() Any

type NewFuncs []NewFunc

func (nf NewFuncs) EventNames() []string {
	names := make([]string, len(nf))

	for i, newFunc := range nf {
		names[i] = newFunc().EventName()
	}

	return names
}

func NewFuncFor[T Any]() NewFunc {
	var zero T
	typ := reflect.TypeOf(zero).Elem()

	return func() Any {
		newInstance := reflect.New(typ).Interface()

		eventInstance, ok := newInstance.(Any)
		// This should theoretically never happen because of the
		// generic constraint.
		assert.That(ok, "assert type to event.Any")

		return eventInstance
	}
}

type ConstructorProvider interface {
	EventConstructors() NewFuncs
}

type RootRegister interface {
	RegisterRoot(root ConstructorProvider) error
}

type NewEventer interface {
	NewEventFactory(eventName string) (NewFunc, bool)
}

type Registry interface {
	RootRegister
	NewEventer
}

var GlobalRegistry = NewRegistry()

func NewRegistry() *eventRegistry {
	return &eventRegistry{
		eventFuncs: make(map[string]NewFunc),
		registryMu: sync.RWMutex{},
	}
}

type eventRegistry struct {
	eventFuncs map[string]NewFunc
	registryMu sync.RWMutex
}

var _ Registry = (*eventRegistry)(nil)

func (r *eventRegistry) NewEventFactory(eventName string) (NewFunc, bool) {
	r.registryMu.RLock()
	defer r.registryMu.RUnlock()

	factory, ok := r.eventFuncs[eventName]
	return factory, ok
}

func (r *eventRegistry) RegisterRoot(root ConstructorProvider) error {
	r.registryMu.Lock()
	defer r.registryMu.Unlock()

	for _, newFunc := range root.EventConstructors() {
		event := newFunc()
		eventName := event.EventName()

		if _, ok := r.eventFuncs[eventName]; ok {
			return fmt.Errorf("duplicate event %q in registry", eventName)
		}

		r.eventFuncs[eventName] = newFunc
	}

	return nil
}
