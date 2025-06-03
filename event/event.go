package event

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
