package event

type EventAny interface {
	EventName() string
}

type wrapper[T EventAny] struct {
	event T
}

type Event wrapper[EventAny]

func New(event EventAny) Event {
	return Event{
		event: event,
	}
}

func (ge *Event) Unwrap() EventAny {
	return ge.event
}
