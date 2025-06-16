package event

import (
	"fmt"
	"sync"

	"github.com/DeluxeOwl/chronicle/internal/assert"
	"github.com/DeluxeOwl/chronicle/internal/typeutils"
)

// The core purpose of this type is to create a new, zero-value instance of a specific event type.
// This instance is then used as a target for unmarshaling data from the event store.
// It acts as a bridge between a string EventName and a concrete Go type.
type NewFunc[E Any] func() E

type NewFuncs[E Any] []NewFunc[E]

func (nf NewFuncs[E]) EventNames() []string {
	names := make([]string, len(nf))

	for i, newFunc := range nf {
		names[i] = newFunc().EventName()
	}

	return names
}

type ConstructorProvider[E Any] interface {
	EventConstructors() NewFuncs[E]
}

type RootRegister[E Any] interface {
	RegisterRoot(root ConstructorProvider[E]) error
}

type NewEventer[E Any] interface {
	NewEventFactory(eventName string) (NewFunc[E], bool)
}

type Registry[E Any] interface {
	RootRegister[E]
	NewEventer[E]
}

var _ Registry[Any] = (*eventRegistry[Any])(nil)

type eventRegistry[E Any] struct {
	eventFuncs map[string]NewFunc[E]
	registryMu sync.RWMutex
}

func NewRegistry[E Any]() *eventRegistry[E] {
	return &eventRegistry[E]{
		eventFuncs: make(map[string]NewFunc[E]),
		registryMu: sync.RWMutex{},
	}
}

func (r *eventRegistry[E]) RegisterRoot(root ConstructorProvider[E]) error {
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

func (r *eventRegistry[E]) NewEventFactory(eventName string) (NewFunc[E], bool) {
	r.registryMu.RLock()
	defer r.registryMu.RUnlock()

	factory, ok := r.eventFuncs[eventName]
	return factory, ok
}

type concreteRegistry[E Any] struct {
	anyRegistry Registry[Any]
}

// This is useful if you want a shared registry in a bigger system
// Which will fail if you have duplicate events

func NewConcreteRegistryFromAny[E Any](anyRegistry Registry[Any]) Registry[E] {
	return &concreteRegistry[E]{
		anyRegistry: anyRegistry,
	}
}

func (r *concreteRegistry[E]) RegisterRoot(root ConstructorProvider[E]) error {
	concreteNewFuncs := root.EventConstructors()

	anyNewFuncs := make(NewFuncs[Any], len(concreteNewFuncs))

	for i, concreteFunc := range concreteNewFuncs {
		capturedFunc := concreteFunc
		anyNewFuncs[i] = func() Any {
			// Call the original function returning E, which is assignable to Any.
			return capturedFunc()
		}
	}

	adapter := &constructorProviderAdapter[E]{
		anyConstructors: anyNewFuncs,
	}

	return r.anyRegistry.RegisterRoot(adapter)
}

func (r *concreteRegistry[E]) NewEventFactory(eventName string) (NewFunc[E], bool) {
	anyFactory, ok := r.anyRegistry.NewEventFactory(eventName)
	if !ok {
		return nil, false
	}

	concreteFactory := func() E {
		anyInstance := anyFactory()

		concreteInstance, ok := anyInstance.(E)
		assert.That(ok,
			"type assertion failed: event type %T from registry is not assignable to target type %T",
			anyInstance,
			typeutils.Zero[E]())

		return concreteInstance
	}

	return concreteFactory, true
}

// constructorProviderAdapter adapts a ConstructorProvider[E] to a ConstructorProvider[Any].
type constructorProviderAdapter[E Any] struct {
	anyConstructors NewFuncs[Any]
}

func (a *constructorProviderAdapter[E]) EventConstructors() NewFuncs[Any] {
	return a.anyConstructors
}
