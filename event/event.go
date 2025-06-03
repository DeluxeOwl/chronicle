package event

import "encoding/json"

type EventAny interface {
	EventName() string
}

type wrapper[T EventAny] struct {
	event T
}

type Event wrapper[EventAny]

var Empty = Event{event: nil}

func New(event EventAny) Event {
	return Event{
		event: event,
	}
}

func (ge *Event) Unwrap() EventAny {
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
func Unmarshal(data []byte, v EventAny) error {
	if customUnmarshal, ok := v.(Unmarshaler); ok {
		return customUnmarshal.UnmarshalEvent(data)
	}

	return json.Unmarshal(data, v)
}

func Marshal(v EventAny) ([]byte, error) {
	if customMarshal, ok := v.(Marshaler); ok {
		return customMarshal.MarshalEvent()
	}

	return json.Marshal(v)
}
