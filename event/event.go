package event

import "github.com/DeluxeOwl/eventuallynow/version"

type GenericEvent interface {
	EventName() string
}

type wrappedEvent[T GenericEvent] struct {
	event    T
	metadata map[string]string
}

type Event wrappedEvent[GenericEvent]

func New(event GenericEvent) Event {
	return Event{
		event:    event,
		metadata: nil,
	}
}

func (ge *Event) Unwrap() GenericEvent {
	return ge.event
}

func ToRecorded(startingVersion version.Version, id LogID, events ...Event) []RecordedEvent {
	recordedEvents := make([]RecordedEvent, len(events))
	for i, e := range events {
		//nolint:gosec // It's not a problem in practice.
		recordedEvents[i] = NewRecorded(startingVersion+version.Version(i+1), id, e)
	}
	return recordedEvents
}
