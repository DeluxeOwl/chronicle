package event

import (
	"fmt"
	"sync"

	"github.com/DeluxeOwl/chronicle/internal/assert"
)

// FuncFor creates a new, zero-value instance of a specific event type.
// This function acts as a factory, enabling the framework to deserialize event data
// from the event store into concrete Go types without knowing the types at compile time.
//
// Usage:
//
//	// Inside an aggregate's EventFuncs method.
//
//	func (a *Account) EventFuncs() event.FuncsFor[AccountEvent] {
//	    return event.FuncsFor[AccountEvent]{
//	        func() AccountEvent { return new(accountOpened) },
//	        // ... more event factories
//	    }
//	}
//
// Returns a new instance of the event type `E`.
type FuncFor[E Any] func() E

// FuncsFor is a slice of event factory functions.
// It represents all the event types associated with a particular aggregate.
type FuncsFor[E Any] []FuncFor[E]

// EventNames extracts the string names from all factory functions in the slice.
// It is a helper method primarily used for internal checks and debugging.
//
// Returns a slice of strings, where each string is an event name.
func (funcs FuncsFor[E]) EventNames() []string {
	names := make([]string, len(funcs))

	for i, createEvent := range funcs {
		names[i] = createEvent().EventName()
	}

	return names
}

// EventFuncCreator is an interface that an aggregate root must implement.
// It provides a way for the aggregate to declare all of its possible event types
// to the event registry.
//
// Usage:
//
//	// An aggregate implements this interface.
//	type Account struct { /* ... */ }
//	func (a *Account) EventFuncs() event.FuncsFor[AccountEvent] { /* ... */ }
type EventFuncCreator[E Any] interface {
	EventFuncs() FuncsFor[E]
}

// EventRegistrar is responsible for the "write" side of a registry.
// It defines the contract for registering event factory functions from an aggregate.
type EventRegistrar[E Any] interface {
	RegisterEvents(root EventFuncCreator[E]) error
}

// EventFuncGetter is responsible for the "read" side of a registry.
// It defines the contract for retrieving a specific event factory function by its name.
type EventFuncGetter[E Any] interface {
	GetFunc(eventName string) (FuncFor[E], bool)
}

// Registry maps event names (strings) to factory functions that create concrete
// event types. It is a critical component for deserialization, allowing the
// framework to reconstruct typed events from the raw data stored in the event log.
type Registry[E Any] interface {
	EventRegistrar[E]
	EventFuncGetter[E]
}

var _ Registry[Any] = (*EventRegistry[Any])(nil)

// EventRegistry is a thread-safe implementation of the Registry interface.
// It uses a map to store event factories and a RWMutex to handle concurrent access.
type EventRegistry[E Any] struct {
	eventFuncs map[string]FuncFor[E]
	registryMu sync.RWMutex
}

// NewRegistry creates a new, empty event registry for a specific event base type.
//
// Usage:
//
//	registry := event.NewRegistry[account.AccountEvent]()
//
// Returns a pointer to a new EventRegistry.
func NewRegistry[E Any]() *EventRegistry[E] {
	return &EventRegistry[E]{
		eventFuncs: make(map[string]FuncFor[E]),
		registryMu: sync.RWMutex{},
	}
}

// RegisterEvents populates the registry with the event factories from an aggregate.
// It iterates through the functions provided by the EventFuncCreator and maps them
// by their event name. This is typically called once during application startup.
// The default repositories call this function for you.
//
// Usage:
//
//	accountAggregate := account.NewEmpty()
//	err := registry.RegisterEvents(accountAggregate)
//
// Returns an error if an event with the same name is already registered.
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

// GetFunc retrieves an event factory function from the registry using its unique name.
// The framework uses this during deserialization to create a concrete event instance
// before unmarshaling the data.
//
// Usage:
//
//	factory, found := registry.GetFunc("account/money_deposited")
//	if found {
//	    event := factory() // returns a new(moneyDeposited)
//	}
//
// Returns the factory function and a boolean indicating whether the name was found.
func (r *EventRegistry[E]) GetFunc(eventName string) (FuncFor[E], bool) {
	r.registryMu.RLock()
	defer r.registryMu.RUnlock()

	factory, ok := r.eventFuncs[eventName]
	return factory, ok
}

// concreteRegistry is an adapter that allows a type-specific registry (Registry[E])
// to be backed by a global, type-erased registry (Registry[Any]). This is useful in
// larger systems to ensure event name uniqueness across all aggregates.
type concreteRegistry[E Any] struct {
	anyRegistry       Registry[Any]
	concreteFactories map[string]func() E
}

// NewConcreteRegistryFromAny creates a new type-safe registry that wraps a global,
// shared `event.Any` registry. This enforces application-wide uniqueness of event names.
//
// Usage:
//
//	// In main.go or setup code:
//	globalRegistry := event.NewRegistry[event.Any]()
//
//	// When creating a repository for a specific aggregate:
//	accountRepo, err := chronicle.NewEventSourcedRepository(
//	    ...,
//	    chronicle.AnyEventRegistry(globalRegistry),
//	)
//
// Returns a new `Registry[E]`.
func NewConcreteRegistryFromAny[E Any](anyRegistry Registry[Any]) Registry[E] {
	return &concreteRegistry[E]{
		anyRegistry:       anyRegistry,
		concreteFactories: map[string]func() E{},
	}
}

// RegisterEvents registers the aggregate's event factories with the underlying
// shared `event.Any` registry.
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

// GetFunc retrieves a factory from the underlying `event.Any` registry and
// safely casts it to the expected concrete type `E`.
func (r *concreteRegistry[E]) GetFunc(eventName string) (FuncFor[E], bool) {
	anyFactory, ok := r.anyRegistry.GetFunc(eventName)
	if !ok {
		return nil, false
	}

	if concreteFactory, exists := r.concreteFactories[eventName]; exists {
		return concreteFactory, true
	}

	// Create and cache new factory
	newFactory := func() E {
		anyInstance := anyFactory()
		concreteInstance, ok := anyInstance.(E)
		if !ok {
			var empty E
			assert.Never(
				"type assertion failed: event type %T from registry is not assignable to target type %T",
				anyInstance,
				empty,
			)
		}
		return concreteInstance
	}

	r.concreteFactories[eventName] = newFactory
	return newFactory, true
}

// constructorProviderAdapter adapts a EventFuncCreator[E] to a EventFuncCreator[Any].
// It's a helper type used internally by `concreteRegistry`.
type constructorProviderAdapter[E Any] struct {
	anyConstructors FuncsFor[Any]
}

func (a *constructorProviderAdapter[E]) EventFuncs() FuncsFor[Any] {
	return a.anyConstructors
}
