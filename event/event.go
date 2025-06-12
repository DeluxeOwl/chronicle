package event

import "encoding/json"

type Any interface {
	EventName() string
}

type wrapper[T Any] struct {
	event T
}

type Event wrapper[Any]

var Empty = Event{event: nil}

func New(event Any) Event {
	return Event{
		event: event,
	}
}

func (ge *Event) Unwrap() Any {
	return ge.event
}

func (ge *Event) EventName() string {
	return ge.event.EventName()
}

type Unmarshaler interface {
	UnmarshalEvent(data []byte) error
}

type Marshaler interface {
	MarshalEvent() ([]byte, error)
}

// TODO: allow global encoder/decoder, or constructor
func Unmarshal(data []byte, v Any) error {
	if customUnmarshal, ok := v.(Unmarshaler); ok {
		return customUnmarshal.UnmarshalEvent(data)
	}

	return json.Unmarshal(data, v)
}

func Marshal(v Any) ([]byte, error) {
	if customMarshal, ok := v.(Marshaler); ok {
		return customMarshal.MarshalEvent()
	}

	return json.Marshal(v)
}
