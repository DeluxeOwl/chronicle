package aggregate

import (
	"fmt"
	"iter"

	"github.com/DeluxeOwl/chronicle/event"
	"github.com/DeluxeOwl/chronicle/serde"
)

func AnyToConcrete[E event.Any](event Event[event.Any]) (Event[E], bool) {
	concrete, ok := event.Unwrap().(E)
	if !ok {
		var empty Event[E]
		return empty, false
	}

	return Event[E]{
		wrappedEvent: concrete,
	}, true
}

type Event[E event.Any] struct {
	wrappedEvent E
}

func createWrappedEvent[E event.Any](event E) Event[E] {
	return Event[E]{
		wrappedEvent: event,
	}
}

func (ge *Event[T]) Unwrap() T {
	return ge.wrappedEvent
}

func (ge *Event[T]) EventName() string {
	return ge.wrappedEvent.EventName()
}

func (ge *Event[T]) ToRaw(serializer serde.BinarySerializer) (event.Raw, error) {
	bytes, err := serializer.SerializeBinary(ge.Unwrap())
	if err != nil {
		return event.Raw{}, fmt.Errorf("convert event to raw event: marshal event: %w", err)
	}

	return event.NewRaw(ge.EventName(), bytes), nil
}

type (
	UncommitedEvents[E event.Any] []Event[E]
	CommitedEvents[E event.Any]   []Event[E]
)

func (uncommitted UncommitedEvents[E]) ToRaw(serializer serde.BinarySerializer) ([]event.Raw, error) {
	rawEvents := make([]event.Raw, len(uncommitted))
	for i := range uncommitted {
		raw, err := uncommitted[i].ToRaw(serializer)
		if err != nil {
			return nil, fmt.Errorf("convert events: %w", err)
		}

		rawEvents[i] = raw
	}

	return rawEvents, nil
}

func (committed CommitedEvents[E]) All() iter.Seq[E] {
	return func(yield func(E) bool) {
		for _, evt := range committed {
			if !yield(evt.Unwrap()) {
				return
			}
		}
	}
}
