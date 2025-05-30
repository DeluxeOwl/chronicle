package event

import "github.com/DeluxeOwl/eventuallynow/version"

type Event interface {
	Name() string
}

type Envelope[T Event] struct {
	event    T
	metadata map[string]string
}

type Option[T Event] func(*Envelope[T])

func NewEnvelope[T Event](event T, opts ...Option[T]) Envelope[T] {
	e := Envelope[T]{
		event:    event,
		metadata: map[string]string{},
	}
	for _, o := range opts {
		o(&e)
	}
	return e
}

type GenericEnvelope Envelope[Event]

func NewGenericEnvelope(event Event) GenericEnvelope {
	return GenericEnvelope{
		event:    event,
		metadata: nil,
	}
}

func (ge *GenericEnvelope) Event() Event {
	return ge.event
}

func ToEnvelope(event Event) GenericEnvelope {
	return NewGenericEnvelope(event)
}

func ToEnvelopes(events ...Event) []GenericEnvelope {
	envelopes := make([]GenericEnvelope, 0, len(events))

	for _, event := range events {
		envelopes = append(envelopes, NewGenericEnvelope(event))
	}

	return envelopes
}

func ToStored(startingVersion version.Version, id LogID, events ...GenericEnvelope) []RecordedEvent {
	recordedEvents := make([]RecordedEvent, len(events))
	for i, e := range events {
		//nolint:gosec // It's not a problem in practice.
		recordedEvents[i] = NewRecorded(startingVersion+version.Version(i+1), id, e)
	}
	return recordedEvents
}
