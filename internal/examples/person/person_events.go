package person

import "github.com/DeluxeOwl/chronicle/event"

//sumtype:decl
type PersonEvent interface {
	EventName() string
	isPersonEvent()
}

func (p *Person) ListEvents() []event.EventAny {
	return []event.EventAny{
		&PersonWasBorn{},
		&PersonAgedOneYear{},
	}
}

type PersonWasBorn struct {
	ID       PersonID `json:"id" exhaustruct:"optional"`
	BornName string   `json:"bornName" exhaustruct:"optional"`
}

func (*PersonWasBorn) EventName() string { return "person/was-born" }
func (*PersonWasBorn) isPersonEvent()    {}

type PersonAgedOneYear struct{}

func (*PersonAgedOneYear) EventName() string { return "person/aged-one-year" }
func (*PersonAgedOneYear) isPersonEvent()    {}
