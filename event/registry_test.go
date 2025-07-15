package event_test

import (
	"fmt"
	"reflect"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/DeluxeOwl/chronicle/event"
)

type FooEvent interface {
	event.Any
	isFooEvent()
}

type fooEventAlpha struct{}

func (e *fooEventAlpha) EventName() string { return "test-event-alpha" }
func (e *fooEventAlpha) isFooEvent()       {}

type fooEventBeta struct{}

func (e *fooEventBeta) EventName() string { return "test-event-beta" }
func (e *fooEventBeta) isFooEvent()       {}

type BarEvent interface {
	event.Any
	isBarEvent()
}

// barEventAlpha is an event with a different type but a conflicting name.
type barEventAlpha struct{}

func (e *barEventAlpha) EventName() string { return "test-event-alpha" } // Same name as fooEventAlpha
func (e *barEventAlpha) isBarEvent()       {}

type rogueEvent struct {
	name string
}

func (e rogueEvent) EventName() string { return e.name }

type testEventCreator[E event.Any] struct {
	funcs event.FuncsFor[E]
}

func (c *testEventCreator[E]) EventFuncs() event.FuncsFor[E] {
	return c.funcs
}

func TestEventRegistry_RegisterAndGet(t *testing.T) {
	t.Parallel()
	r := event.NewRegistry[FooEvent]()
	creator := &testEventCreator[FooEvent]{
		funcs: event.FuncsFor[FooEvent]{
			func() FooEvent { return new(fooEventAlpha) },
			func() FooEvent { return new(fooEventBeta) },
		},
	}

	err := r.RegisterEvents(creator)
	require.NoError(t, err)

	// Get and verify event A
	fnA, okA := r.GetFunc("test-event-alpha")
	require.True(t, okA)
	require.NotNil(t, fnA)
	require.IsType(t, new(fooEventAlpha), fnA())

	// Get and verify event B
	fnB, okB := r.GetFunc("test-event-beta")
	require.True(t, okB)
	require.NotNil(t, fnB)
	require.IsType(t, new(fooEventBeta), fnB())
}

func TestEventRegistry_RegisterDuplicate(t *testing.T) {
	t.Parallel()
	r := event.NewRegistry[FooEvent]()
	creator := &testEventCreator[FooEvent]{
		funcs: event.FuncsFor[FooEvent]{
			func() FooEvent { return new(fooEventAlpha) },
		},
	}

	// First registration should succeed
	err := r.RegisterEvents(creator)
	require.NoError(t, err)

	// Second registration of the same event should fail
	err = r.RegisterEvents(creator)
	require.Error(t, err)
	require.EqualError(t, err, `duplicate event "test-event-alpha" in registry`)
}

func TestEventRegistry_GetNonExistent(t *testing.T) {
	t.Parallel()
	r := event.NewRegistry[FooEvent]()

	fn, ok := r.GetFunc("non-existent-event")
	require.False(t, ok)
	require.Nil(t, fn)
}

func TestEventRegistry_Concurrency(t *testing.T) {
	t.Parallel()
	r := event.NewRegistry[event.Any]()
	var wg sync.WaitGroup
	eventCount := 100

	// Concurrently register 100 different events
	for i := range eventCount {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			eventName := fmt.Sprintf("concurrent-event-%d", idx)
			creator := &testEventCreator[event.Any]{
				funcs: event.FuncsFor[event.Any]{
					func() event.Any { return &rogueEvent{name: eventName} },
				},
			}
			err := r.RegisterEvents(creator)
			assert.NoError(t, err)
		}(i)
	}

	wg.Wait()

	// Verify all events were registered correctly
	for i := range eventCount {
		eventName := fmt.Sprintf("concurrent-event-%d", i)
		fn, ok := r.GetFunc(eventName)
		require.True(t, ok, "event %s should be registered", eventName)
		require.NotNil(t, fn)
		require.Equal(t, eventName, fn().EventName())
	}
}

func TestConcreteRegistry_RegisterAndGet(t *testing.T) {
	t.Parallel()
	anyRegistry := event.NewRegistry[event.Any]()
	concreteReg := event.NewConcreteRegistryFromAny[FooEvent](anyRegistry)

	creator := &testEventCreator[FooEvent]{
		funcs: event.FuncsFor[FooEvent]{
			func() FooEvent { return new(fooEventAlpha) },
			func() FooEvent { return new(fooEventBeta) },
		},
	}

	err := concreteReg.RegisterEvents(creator)
	require.NoError(t, err)

	// Get and verify event A
	fnA, okA := concreteReg.GetFunc("test-event-alpha")
	require.True(t, okA)
	require.NotNil(t, fnA)
	eventA := fnA()
	require.IsType(t, new(fooEventAlpha), eventA)
	require.Equal(t, "test-event-alpha", eventA.EventName())

	// Get and verify event B
	fnB, okB := concreteReg.GetFunc("test-event-beta")
	require.True(t, okB)
	require.NotNil(t, fnB)
	eventB := fnB()
	require.IsType(t, new(fooEventBeta), eventB)
	require.Equal(t, "test-event-beta", eventB.EventName())
}

func TestConcreteRegistry_DuplicateEventRegistration(t *testing.T) {
	t.Parallel()
	anyRegistry := event.NewRegistry[event.Any]()

	// Registry for FooEvent interface
	concreteReg1 := event.NewConcreteRegistryFromAny[FooEvent](anyRegistry)
	creator1 := &testEventCreator[FooEvent]{
		funcs: event.FuncsFor[FooEvent]{
			func() FooEvent { return new(fooEventAlpha) },
		},
	}
	err := concreteReg1.RegisterEvents(creator1)
	require.NoError(t, err)

	// Registry for BarEvent interface, trying to register a conflicting name
	concreteReg2 := event.NewConcreteRegistryFromAny[BarEvent](anyRegistry)
	creator2 := &testEventCreator[BarEvent]{
		funcs: event.FuncsFor[BarEvent]{
			func() BarEvent { return new(barEventAlpha) },
		},
	}
	err = concreteReg2.RegisterEvents(creator2)
	require.Error(t, err)
	require.EqualError(t, err, `duplicate event "test-event-alpha" in registry`)
}

func TestConcreteRegistry_GetFuncCachesFactory(t *testing.T) {
	t.Parallel()
	anyRegistry := event.NewRegistry[event.Any]()
	concreteReg := event.NewConcreteRegistryFromAny[FooEvent](anyRegistry)

	creator := &testEventCreator[FooEvent]{
		funcs: event.FuncsFor[FooEvent]{
			func() FooEvent { return new(fooEventAlpha) },
		},
	}
	err := concreteReg.RegisterEvents(creator)
	require.NoError(t, err)

	// Get the factory twice
	f1, ok1 := concreteReg.GetFunc("test-event-alpha")
	require.True(t, ok1)
	f2, ok2 := concreteReg.GetFunc("test-event-alpha")
	require.True(t, ok2)

	// Check that the same function instance is returned (i.e., it was cached)
	f1Ptr := reflect.ValueOf(f1).Pointer()
	f2Ptr := reflect.ValueOf(f2).Pointer()
	require.Equal(t, f1Ptr, f2Ptr, "factory function should be cached and reused")
}

func TestConcreteRegistry_GetFuncPanicsOnTypeMismatch(t *testing.T) {
	t.Parallel()
	anyRegistry := event.NewRegistry[event.Any]()

	// Manually register a "rogue" event into the anyRegistry that does not conform to TestEvent
	rogueCreator := &testEventCreator[event.Any]{
		funcs: event.FuncsFor[event.Any]{
			func() event.Any { return rogueEvent{name: "rogue-event"} },
		},
	}
	err := anyRegistry.RegisterEvents(rogueCreator)
	require.NoError(t, err)

	concreteReg := event.NewConcreteRegistryFromAny[FooEvent](anyRegistry)

	factory, ok := concreteReg.GetFunc("rogue-event")
	require.True(t, ok)
	require.NotNil(t, factory)

	require.Panics(t, func() {
		factory()
	}, "should panic when the underlying event type does not match the concrete registry's type")
}
