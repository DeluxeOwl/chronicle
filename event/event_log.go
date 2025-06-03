package event

import (
	"context"
	"iter"

	"github.com/DeluxeOwl/eventuallynow/version"
)

type AllReader interface {
	ReadAllEvents(ctx context.Context, selector version.Selector) RecordedEvents
}

type Reader interface {
	ReadEvents(ctx context.Context, id LogID, selector version.Selector) RecordedEvents
}

type Appender interface {
	AppendEvents(ctx context.Context, id LogID, expected version.Check, events ...Event) (version.Version, error)
}

type Log interface {
	Reader
	Appender
}

type RecordedEvents iter.Seq2[*RecordedEvent, error]

func (re RecordedEvents) AsSlice() ([]RecordedEvent, error) {
	ee := []RecordedEvent{}
	for ev, err := range re {
		if err != nil {
			return nil, err
		}
		ee = append(ee, *ev)
	}
	return ee, nil
}
