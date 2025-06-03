package person

import (
	"time"

	"github.com/DeluxeOwl/eventuallynow/aggregate"
	"github.com/DeluxeOwl/eventuallynow/event"
	"github.com/DeluxeOwl/zerrors"
)

type PersonID string

func (p PersonID) String() string { return string(p) }

type PersonError string

const (
	ErrUnexpectedEventType PersonError = "unexpected_event_type"
	ErrUnexpectedEventKIND PersonError = "unexpected_event_kind"
	ErrEmptyName           PersonError = "empty_name"
	ErrCreate              PersonError = "create"
	ErrNilPersonSerialize  PersonError = "serialize_person_nil"
)

type Person struct {
	aggregate.Base

	id   PersonID
	name string
	age  int
}

func (p *Person) GetAge() int {
	return p.age
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
		return nil, zerrors.New(ErrEmptyName)
	}

	p := NewEmpty()

	if err := p.record(&PersonEvent{
		ID:         id,
		RecordTime: time.Now(),
		Kind: &WasBorn{
			BornName: name,
		},
	}); err != nil {
		return nil, zerrors.New(ErrCreate).WithError(err)
	}

	return p, nil
}

func (p *Person) Apply(evt event.EventAny) error {
	personEvent, ok := evt.(*PersonEvent)
	if !ok {
		return zerrors.New(ErrUnexpectedEventType).Errorf("type: %T", evt)
	}

	switch kind := personEvent.Kind.(type) {
	case *WasBorn:
		p.id = personEvent.ID
		p.age = 0
		p.name = kind.BornName
	case *AgedOneYear:
		p.age++
	default:
		return zerrors.New(ErrUnexpectedEventType).Errorf("kind: %T", personEvent.Kind)
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
