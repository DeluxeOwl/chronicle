package event

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
