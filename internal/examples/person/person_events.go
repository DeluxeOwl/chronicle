package person

import "github.com/DeluxeOwl/eventuallynow/aggregate"

//sumtype:decl
type PersonEvent interface {
	EventName() string
	isPersonEvent()
}

func (p *Person) RegisterEvents(r aggregate.RegisterFunc) {
	r(&WasBorn{})
	r(&AgedOneYear{})
}

type WasBorn struct {
	ID       PersonID `json:"id" exhaustruct:"optional"`
	BornName string   `json:"bornName" exhaustruct:"optional"`
}

func (*WasBorn) EventName() string { return "person/was-born" }
func (*WasBorn) isPersonEvent()    {}

type AgedOneYear struct{}

func (*AgedOneYear) EventName() string { return "person/aged-one-year" }
func (*AgedOneYear) isPersonEvent()    {}
