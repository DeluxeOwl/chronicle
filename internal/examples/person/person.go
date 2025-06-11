package person

import (
	"errors"
	"fmt"
	"time"

	"github.com/DeluxeOwl/eventuallynow/aggregate"
	"github.com/DeluxeOwl/eventuallynow/event"
)

type PersonID string

func (p PersonID) String() string { return string(p) }

type Person struct {
	aggregate.Base

	id   PersonID
	name string
	age  int
}

type PersonSnapshot struct {
	ID   PersonID `json:"id"`
	Name string   `json:"name"`
	Age  int      `json:"age"`
}

func (p *Person) Snapshot() *PersonSnapshot {
	return &PersonSnapshot{
		ID:   p.id,
		Name: p.name,
		Age:  p.age,
	}
}

func (p *Person) ID() PersonID {
	return PersonID(p.id.String())
}

// TODO: how would I add a custom registry/dependencies??
func NewEmpty() *Person {
	return new(Person)
}

func New(id PersonID, name string) (*Person, error) {
	if name == "" {
		return nil, errors.New("empty name")
	}

	p := NewEmpty()

	if err := p.record(&PersonEvent{
		ID:         id,
		RecordTime: time.Now(),
		Kind: &WasBorn{
			BornName: name,
		},
	}); err != nil {
		return nil, fmt.Errorf("create person: %w", err)
	}

	return p, nil
}

func (p *Person) Apply(evt event.EventAny) error {
	personEvent, ok := evt.(*PersonEvent)
	if !ok {
		return fmt.Errorf("unexpected event type: %T", evt)
	}

	switch kind := personEvent.Kind.(type) {
	case *WasBorn:
		p.id = personEvent.ID
		p.age = 0
		p.name = kind.BornName
	case *AgedOneYear:
		p.age++
	default:
		return fmt.Errorf("unexpected event kind: %T", kind)
	}

	return nil
}

func (p *Person) Age() error {
	return p.record(&PersonEvent{
		ID:         p.id,
		RecordTime: time.Now(),
		Kind:       &AgedOneYear{},
	})
}

func (p *Person) RegisterEvents(r aggregate.RegisterFunc) {
	for _, name := range PersonEventNames {
		switch name {
		case PersonEventWasBorn:
			r(&PersonEvent{Kind: &WasBorn{}})
		case PersonEventAgedOneYear:
			r(&PersonEvent{Kind: &AgedOneYear{}})
		default:
			panic("unhandled event name: " + name.String())
		}
	}
}

func (p *Person) record(event *PersonEvent) error {
	return aggregate.RecordEvent(p, event)
}
