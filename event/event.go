package event

import (
	"fmt"
	"iter"

	"github.com/DeluxeOwl/chronicle/internal/typeutils"
)

type Any interface {
	EventName() string
}

func AnyToConcrete[E Any](event Event[Any]) (Event[E], bool) {
	concrete, ok := event.Unwrap().(E)
	if !ok {
		return typeutils.Zero[Event[E]](), false
	}

	return Event[E]{
		wrappedEvent: concrete,
	}, true
}

type Event[E Any] struct {
	wrappedEvent E
}

func New(event Any) Event[Any] {
	return Event[Any]{
		wrappedEvent: event,
	}
}

func (ge *Event[T]) Unwrap() T {
	return ge.wrappedEvent
}

func (ge *Event[T]) EventName() string {
	return ge.wrappedEvent.EventName()
}

func (ge *Event[T]) ToRaw(serde Serializer) (Raw, error) {
	bytes, err := serde.MarshalEvent(ge.Unwrap())
	if err != nil {
		return Raw{}, fmt.Errorf("convert event to raw event: marshal event: %w", err)
	}

	return NewRaw(ge.EventName(), bytes), nil
}

type (
	UncommitedEvents[E Any] []Event[E]
	CommitedEvents[E Any]   []Event[E]
)

func (uncommitted UncommitedEvents[E]) ToRaw(serializer Serializer) ([]Raw, error) {
	rawEvents := make([]Raw, len(uncommitted))
	for i := range uncommitted {
		raw, err := uncommitted[i].ToRaw(serializer)
		if err != nil {
			return nil, fmt.Errorf("convert events: %w", err)
		}

		rawEvents[i] = raw
	}

	return rawEvents, nil
}

func (committed CommitedEvents[E]) All() iter.Seq[Event[E]] {
	return func(yield func(Event[E]) bool) {
		for _, evt := range committed {
			if !yield(evt) {
				return
			}
		}
	}
}
