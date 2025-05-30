package event

import (
	"github.com/DeluxeOwl/eventuallynow/message"
	"github.com/DeluxeOwl/eventuallynow/version"
)

type Event message.Message

type Envelope message.GenericEnvelope

func ToEnvelope(event Event) Envelope {
	return Envelope{
		Message:  event,
		Metadata: nil,
	}
}

func ToEnvelopes(events ...Event) []Envelope {
	envelopes := make([]Envelope, 0, len(events))

	for _, event := range events {
		envelopes = append(envelopes, Envelope{
			Message:  event,
			Metadata: nil,
		})
	}

	return envelopes
}

func ToStored(startingVersion version.Version, id LogID, events ...Envelope) []RecordedEvent {
	recordedEvents := make([]RecordedEvent, len(events))
	for i, e := range events {
		//nolint:gosec // It's not a problem in practice.
		recordedEvents[i] = NewRecorded(startingVersion+version.Version(i+1), id, e)
	}
	return recordedEvents
}
