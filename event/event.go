package event

import (
	"fmt"
)

type Any interface {
	EventName() string
}

type Event struct {
	wrappedEvent Any
}

func New(event Any) Event {
	return Event{
		wrappedEvent: event,
	}
}

func (ge *Event) Unwrap() Any {
	return ge.wrappedEvent
}

func (ge *Event) EventName() string {
	return ge.wrappedEvent.EventName()
}

func (ge *Event) ToRaw(serde Serializer) (Raw, error) {
	bytes, err := serde.MarshalEvent(ge.Unwrap())
	if err != nil {
		return Raw{}, fmt.Errorf("convert event to raw event: marshal event: %w", err)
	}

	return *NewRaw(ge.EventName(), bytes), nil
}

type (
	UncommitedEvents []Event
	CommitedEvents   []Event
)

func (events UncommitedEvents) ToRaw(serializer Serializer) ([]Raw, error) {
	rawEvents := make([]Raw, len(events))
	for i := range events {
		raw, err := events[i].ToRaw(serializer)
		if err != nil {
			return nil, fmt.Errorf("convert events: %w", err)
		}

		rawEvents[i] = raw
	}

	return rawEvents, nil
}
