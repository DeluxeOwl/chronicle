package aggregate

import (
	"fmt"
	"reflect"
	"sync"

	"github.com/DeluxeOwl/eventuallynow/event"
	"github.com/DeluxeOwl/eventuallynow/internal/registry"
	"github.com/DeluxeOwl/eventuallynow/version"
)

type AggregateError string

const (
	ErrFailedToRecord AggregateError = "failed_to_record_event"
)

type ID interface {
	fmt.Stringer
}

type Aggregate interface {
	Apply(event.EventAny) error
}

type RecordedEventsFlusher interface {
	FlushRecordedEvents() []event.Event
}

type Root[TypeID ID] interface {
	Aggregate
	RecordedEventsFlusher

	ID() TypeID
	Version() version.Version
	RegisterEvents(r RegisterFunc)

	// EventRecorder implements these, so you *have* to embed EventRecorder.
	setVersion(version.Version)
	setRegisteredEvents()
	hasRegisteredEvents() bool
	recordThat(Aggregate, ...event.Event) error
}

type RegisterFunc func(eventName string, kind any)

// Global map to track registration status per aggregate type.
// The key is the reflect.Type of the aggregate (e.g., person.Person),
// and the value is a sync.Once to ensure its RegisterEvents is called once.
var (
	typeRegistrations   = make(map[reflect.Type]*sync.Once)
	typeRegistrationsMu sync.Mutex
)

func RecordEvent[TypeID ID](root Root[TypeID], e event.EventAny) error {
	// Instance-level check: if this specific instance has already processed registration, skip.
	// This is an optimization to avoid map lookups and sync.Once.Do checks on every event for an instance.
	if !root.hasRegisteredEvents() {
		// Determine the concrete type of the aggregate root.
		// We use .Elem() because 'root' is typically a pointer to a struct (e.g., *Person).
		// We want the type of the struct itself (e.g., Person) as the key.
		// Note: If 'root' could be a non-pointer struct that implements the interface,
		// this might need adjustment, but typically aggregates are pointers.
		var aggType reflect.Type
		val := reflect.ValueOf(root)
		if val.Kind() == reflect.Ptr {
			aggType = val.Elem().Type()
		} else {
			aggType = val.Type() // Should not happen with current design if Root implies methods with ptr receivers
		}

		typeRegistrationsMu.Lock()
		once, ok := typeRegistrations[aggType]
		if !ok {
			once = new(sync.Once)
			typeRegistrations[aggType] = once
		}
		typeRegistrationsMu.Unlock()

		// This will execute root.RegisterEvents(registry.Register) only once
		// for the given aggType, across all instances and goroutines.
		once.Do(func() {
			root.RegisterEvents(registry.Register)
		})

		// Mark this specific instance as having gone through the registration process.
		root.setRegisteredEvents()
	}

	return root.recordThat(root, event.New(e))
}
