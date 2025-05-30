package event

import "github.com/DeluxeOwl/eventuallynow/version"

type Event interface {
	EventName() string
}

type Envelope[T Event] struct {
	event    T
	metadata map[string]string
}

type AnyEvent Envelope[Event]

func NewEvent(event Event) AnyEvent {
	return AnyEvent{
		event:    event,
		metadata: nil,
	}
}

func (ge *AnyEvent) Event() Event {
	return ge.event
}

func ToEnvelope(event Event) AnyEvent {
	return NewEvent(event)
}

func ToStored(startingVersion version.Version, id LogID, events ...AnyEvent) []RecordedEvent {
	recordedEvents := make([]RecordedEvent, len(events))
	for i, e := range events {
		//nolint:gosec // It's not a problem in practice.
		recordedEvents[i] = NewRecorded(startingVersion+version.Version(i+1), id, e)
	}
	return recordedEvents
}
