package person

import (
	"github.com/DeluxeOwl/chronicle/event"
)

//sumtype:decl
type PersonEvent interface {
	EventName() string
	isPersonEvent()
}

func (p *Person) EventFuncs() event.FuncsFor[PersonEvent] {
	return event.FuncsFor[PersonEvent]{
		func() PersonEvent { return new(personWasBorn) },
		func() PersonEvent { return new(personAgedOneYear) },
	}
}

// Note: events are unexported so people outside the package can't
// use person.Apply with random events

type personWasBorn struct {
	ID       PersonID `json:"id" exhaustruct:"optional"`
	BornName string   `json:"bornName" exhaustruct:"optional"`
}

func (*personWasBorn) EventName() string { return "person/was-born" }
func (*personWasBorn) isPersonEvent()    {}

type personAgedOneYear struct{}

func (*personAgedOneYear) EventName() string { return "person/aged-one-year" }
func (*personAgedOneYear) isPersonEvent()    {}
