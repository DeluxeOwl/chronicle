package event

import (
	"fmt"
	"sync"

	"github.com/DeluxeOwl/chronicle/internal/assert"
)

// The core purpose of this type is to create a new, zero-value instance of a specific event type.
// This instance is then used as a target for unmarshaling data from the event store.
// It acts as a bridge between a string EventName and a concrete Go type.
type FuncFor[E Any] func() E

type FuncsFor[E Any] []FuncFor[E]

func (funcs FuncsFor[E]) EventNames() []string {
	names := make([]string, len(funcs))

	for i, createEvent := range funcs {
		names[i] = createEvent().EventName()
	}

	return names
}

type EventFuncCreator[E Any] interface {
	EventFuncs() FuncsFor[E]
}

type EventRegistrar[E Any] interface {
	RegisterEvents(root EventFuncCreator[E]) error
}

type EventFuncGetter[E Any] interface {
	GetFunc(eventName string) (FuncFor[E], bool)
}

type Registry[E Any] interface {
	EventRegistrar[E]
	EventFuncGetter[E]
}

var _ Registry[Any] = (*EventRegistry[Any])(nil)

type EventRegistry[E Any] struct {
	eventFuncs map[string]FuncFor[E]
	registryMu sync.RWMutex
}

func NewRegistry[E Any]() *EventRegistry[E] {
	return &EventRegistry[E]{
		eventFuncs: make(map[string]FuncFor[E]),
		registryMu: sync.RWMutex{},
	}
}

func (r *EventRegistry[E]) RegisterEvents(root EventFuncCreator[E]) error {
	r.registryMu.Lock()
	defer r.registryMu.Unlock()

	for _, createEvent := range root.EventFuncs() {
		event := createEvent()
		eventName := event.EventName()

		if _, ok := r.eventFuncs[eventName]; ok {
			return fmt.Errorf("duplicate event %q in registry", eventName)
		}

		r.eventFuncs[eventName] = createEvent
	}

	return nil
}

func (r *EventRegistry[E]) GetFunc(eventName string) (FuncFor[E], bool) {
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

func (r *concreteRegistry[E]) RegisterEvents(root EventFuncCreator[E]) error {
	concreteNewFuncs := root.EventFuncs()

	anyNewFuncs := make(FuncsFor[Any], len(concreteNewFuncs))

	for i, concreteFunc := range concreteNewFuncs {
		capturedFunc := concreteFunc
		anyNewFuncs[i] = func() Any {
			return capturedFunc()
		}
	}

	adapter := &constructorProviderAdapter[E]{
		anyConstructors: anyNewFuncs,
	}

	return r.anyRegistry.RegisterEvents(adapter)
}

func (r *concreteRegistry[E]) GetFunc(eventName string) (FuncFor[E], bool) {
	anyFactory, ok := r.anyRegistry.GetFunc(eventName)
	if !ok {
		return nil, false
	}

	// TODO: can we cache this
	concreteFactory := func() E {
		anyInstance := anyFactory()

		concreteInstance, ok := anyInstance.(E)
		if !ok {
			var empty E
			assert.Never("type assertion failed: event type %T from registry is not assignable to target type %T", anyInstance, empty)
		}

		return concreteInstance
	}

	return concreteFactory, true
}

// constructorProviderAdapter adapts a EventFuncCreator[E] to a EventFuncCreator[Any].
type constructorProviderAdapter[E Any] struct {
	anyConstructors FuncsFor[Any]
}

func (a *constructorProviderAdapter[E]) EventFuncs() FuncsFor[Any] {
	return a.anyConstructors
}
