package event

import (
	"maps"

	"github.com/DeluxeOwl/eventuallynow/version"
)

type GenericEvent interface {
	EventName() string
}

// TODO: should metadata be removed?
type wrappedEvent[T GenericEvent] struct {
	event    T
	metadata map[string]string
}

type Event wrappedEvent[GenericEvent]

func (e *Event) GetMeta() map[string]string {
	return e.metadata
}

type Option func(*Event)

func WithMetadata(meta map[string]string) Option {
	return func(e *Event) {
		maps.Copy(e.metadata, meta)
	}
}

func New(event GenericEvent, opts ...Option) Event {
	e := Event{
		event:    event,
		metadata: map[string]string{},
	}

	for _, opt := range opts {
		opt(&e)
	}

	return e
}

func (ge *Event) Unwrap() GenericEvent {
	return ge.event
}

func ToRecorded(startingVersion version.Version, id LogID, events ...Event) []*RecordedEvent {
	recordedEvents := make([]*RecordedEvent, len(events))
	for i, e := range events {
		//nolint:gosec // It's not a problem in practice.
		recordedEvents[i] = NewRecorded(startingVersion+version.Version(i+1), id, e)
	}
	return recordedEvents
}
