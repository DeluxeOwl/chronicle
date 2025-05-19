package person

import (
	"errors"
	"fmt"
	"time"

	"github.com/DeluxeOwl/eventuallynow/aggregate"
	"github.com/DeluxeOwl/eventuallynow/event"
	"github.com/gofrs/uuid/v5"
)

var Type = aggregate.Type[uuid.UUID, *Person]{
	Name: "Person",
	New:  func() *Person { return new(Person) },
}

type Person struct {
	aggregate.Base

	id   uuid.UUID
	name string
	age  int
}

func (p *Person) ID() uuid.UUID {
	return p.id
}

func (p *Person) Apply(evt event.Event) error {
	personEvent, ok := evt.(*PersEvent)
	if !ok {
		return fmt.Errorf("person.Apply: unexpected event type, %T", personEvent)
	}

	switch kind := personEvent.Kind.(type) {
	case *WasBorn:
		p.id = personEvent.ID
		p.age = 0
		p.name = kind.BornName
	case *AgedOneYear:
		p.age++
	}

	return nil
}

func NewPerson(id uuid.UUID, name string, now time.Time) (*Person, error) {
	if name == "" {
		return nil, errors.New("name empty")
	}

	p := new(Person)

	if err := aggregate.RecordThat(p, event.Envelope{
		Metadata: nil,
		Message: &PersEvent{
			ID:         id,
			RecordTime: now,
			Kind: &WasBorn{
				BornName: name,
			},
		},
	}); err != nil {
		return nil, fmt.Errorf("create person: %w", err)
	}

	return p, nil
}

var _ event.Event = new(PersEvent)

type PersEvent struct {
	ID         uuid.UUID
	RecordTime time.Time
	Kind       personEvent
}

func (p *PersEvent) Name() string { return p.Kind.Name() }

//sumtype:decl
type personEvent interface {
	event.Event
	isPersonEvent()
}

type WasBorn struct {
	BornName string
}

func (*WasBorn) Name() string   { return "person-was-born" }
func (*WasBorn) isPersonEvent() {}

type AgedOneYear struct{}

func (*AgedOneYear) Name() string   { return "aged-one-year" }
func (*AgedOneYear) isPersonEvent() {}
