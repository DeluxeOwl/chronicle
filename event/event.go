package event

import "github.com/DeluxeOwl/eventuallynow/version"

type Event interface {
	EventName() string
}

type Envelope[T Event] struct {
	event    T
	metadata map[string]string
}

type EventAny Envelope[Event]

func NewEvent(event Event) EventAny {
	return EventAny{
		event:    event,
		metadata: nil,
	}
}

func (ge *EventAny) Event() Event {
	return ge.event
}

func ToStored(startingVersion version.Version, id LogID, events ...EventAny) []RecordedEvent {
	recordedEvents := make([]RecordedEvent, len(events))
	for i, e := range events {
		//nolint:gosec // It's not a problem in practice.
		recordedEvents[i] = NewRecorded(startingVersion+version.Version(i+1), id, e)
	}
	return recordedEvents
}
