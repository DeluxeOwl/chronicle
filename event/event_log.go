package event

import (
	"context"
	"iter"

	"github.com/DeluxeOwl/eventuallynow/version"
)

type RawEvent struct {
	// TODO: custom type here?
	data []byte
	name string
}

func (re *RawEvent) EventName() string {
	return re.name
}

func (re *RawEvent) Bytes() []byte {
	return re.data
}

type AllReader interface {
	ReadAllEvents(ctx context.Context, selector version.Selector) RecordedEvents
}

type Reader interface {
	ReadEvents(ctx context.Context, id LogID, selector version.Selector) RecordedEvents
}

type Appender interface {
	AppendEvents(ctx context.Context, id LogID, expected version.Check, events ...RawEvent) (version.Version, error)
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
