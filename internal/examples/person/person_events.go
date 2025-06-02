package person

import (
	"time"

	"github.com/DeluxeOwl/eventuallynow/event"
)

var _ event.GenericEvent = new(PersonEvent)

type PersonEvent struct {
	ID         PersonID
	RecordTime time.Time
	Kind       personEvent
}

func (p *PersonEvent) EventName() string { return p.Kind.EventName() }

//sumtype:decl
type personEvent interface {
	event.GenericEvent
	isPersonEvent()
}

// Technically, you could add constructors for the events as well.
type WasBorn struct {
	BornName string
}

func (*WasBorn) EventName() string { return "person-was-born" }
func (*WasBorn) isPersonEvent()    {}

type AgedOneYear struct{}

func (*AgedOneYear) EventName() string { return "aged-one-year" }
func (*AgedOneYear) isPersonEvent()    {}
