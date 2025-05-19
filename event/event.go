package event

import (
	"github.com/DeluxeOwl/eventuallynow/message"
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
