package aggregate

import (
	"fmt"
	"iter"

	"github.com/DeluxeOwl/chronicle/event"
	"github.com/DeluxeOwl/chronicle/serde"
)

func AnyToConcrete[E event.Any](event event.Any) (E, bool) {
	concrete, ok := event.(E)
	if !ok {
		var empty E
		return empty, false
	}

	return concrete, true
}

type (
	UncommitedEvents[E event.Any] []E
	CommitedEvents[E event.Any]   []E
)

func (uncommitted UncommitedEvents[E]) ToRaw(serializer serde.BinarySerializer) ([]event.Raw, error) {
	rawEvents := make([]event.Raw, len(uncommitted))
	for i, evt := range uncommitted {
		bytes, err := serializer.SerializeBinary(evt)
		if err != nil {
			return nil, fmt.Errorf("convert events: %w", err)
		}

		raw := event.NewRaw(evt.EventName(), bytes)

		rawEvents[i] = raw
	}

	return rawEvents, nil
}

func (committed CommitedEvents[E]) All() iter.Seq[E] {
	return func(yield func(E) bool) {
		for _, evt := range committed {
			if !yield(evt) {
				return
			}
		}
	}
}
