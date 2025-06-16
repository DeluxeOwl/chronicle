package aggregate

import (
	"fmt"
	"iter"

	"github.com/DeluxeOwl/chronicle/event"
	"github.com/DeluxeOwl/chronicle/serde"
)

func AnyToConcrete[E event.Any](event eventWrapper[event.Any]) (eventWrapper[E], bool) {
	concrete, ok := event.Unwrap().(E)
	if !ok {
		var empty eventWrapper[E]
		return empty, false
	}

	return eventWrapper[E]{
		event: concrete,
	}, true
}

type eventWrapper[E event.Any] struct {
	event E
}

func createWrappedEvent[E event.Any](event E) eventWrapper[E] {
	return eventWrapper[E]{
		event: event,
	}
}

func (w *eventWrapper[T]) Unwrap() T {
	return w.event
}

func (w *eventWrapper[T]) EventName() string {
	return w.event.EventName()
}

func (w *eventWrapper[T]) ToRaw(serializer serde.BinarySerializer) (event.Raw, error) {
	bytes, err := serializer.SerializeBinary(w.Unwrap())
	if err != nil {
		return event.Raw{}, fmt.Errorf("convert event to raw event: marshal event: %w", err)
	}

	return event.NewRaw(w.EventName(), bytes), nil
}

type (
	UncommitedEvents[E event.Any] []eventWrapper[E]
	CommitedEvents[E event.Any]   []eventWrapper[E]
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
