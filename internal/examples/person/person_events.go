package person

import (
	"time"

	"github.com/DeluxeOwl/eventuallynow/event"
)

var _ event.EventAny = new(PersonEvent)

type PersonEvent struct {
	ID         PersonID    `json:"id"`
	RecordTime time.Time   `json:"recordTime"`
	Kind       personEvent `json:"kind"       exhaustruct:"optional"`
}

func (p *PersonEvent) EventName() string {
	return p.Kind.personEventName()
}

//sumtype:decl
type personEvent interface {
	personEventName() string
}

type WasBorn struct {
	BornName string `json:"bornName"`
}

// For exhaustive checking.
type PersonEventName string

const (
	PersonEventWasBorn     PersonEventName = "person-was-born"
	PersonEventAgedOneYear PersonEventName = "aged-one-year"
)

func (*WasBorn) personEventName() string { return string(PersonEventWasBorn) }

type AgedOneYear struct{}

func (*AgedOneYear) personEventName() string { return string(PersonEventAgedOneYear) }
