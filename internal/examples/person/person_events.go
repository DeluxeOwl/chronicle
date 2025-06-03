package person

import (
	"time"

	"github.com/DeluxeOwl/eventuallynow/event"
)

var _ event.EventAny = new(PersonEvent)

// exhaustruct:"ignore"
type PersonEvent struct {
	ID         PersonID    `json:"id"`
	RecordTime time.Time   `json:"recordTime"`
	Kind       personEvent `json:"kind"`
}

// EventName implements event.EventAny.
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

func (pen PersonEventName) String() string {
	return string(pen)
}

const (
	PersonEventWasBorn     PersonEventName = "person-was-born"
	PersonEventAgedOneYear PersonEventName = "aged-one-year"
)

var PersonEventNames = []PersonEventName{
	PersonEventWasBorn,
	PersonEventAgedOneYear,
}

func (*WasBorn) personEventName() string { return string(PersonEventWasBorn) }

type AgedOneYear struct{}

func (*AgedOneYear) personEventName() string { return string(PersonEventAgedOneYear) }
