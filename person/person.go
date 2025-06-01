package person

import (
	"time"

	"github.com/DeluxeOwl/eventuallynow/aggregate"
	"github.com/DeluxeOwl/eventuallynow/event"
	"github.com/DeluxeOwl/zerrors"
)

type PersonError string

const (
	ErrUnexpectedEventType PersonError = "unexpected_event_type"
	ErrUnexpectedEventKIND PersonError = "unexpected_event_kind"
	ErrEmptyName           PersonError = "empty_name"
	ErrCreate              PersonError = "create"
	ErrNilPersonSerialize  PersonError = "serialize_person_nil"
)

type PersonID string

func (p PersonID) String() string { return string(p) }

type Person struct {
	aggregate.Base

	id   PersonID
	name string
	age  int
}

func NewEmpty() *Person {
	return new(Person)
}

func (p *Person) ID() PersonID {
	return PersonID(p.id.String())
}

func (p *Person) Apply(evt event.GenericEvent) error {
	personEvent, ok := evt.(*PersEvent)
	if !ok {
		return zerrors.New(ErrUnexpectedEventType).Errorf("type: %T", personEvent)
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

func New(id string, name string, now time.Time) (*Person, error) {
	if name == "" {
		return nil, zerrors.New(ErrEmptyName)
	}

	p := NewEmpty()

	if err := aggregate.RecordEvent(p, &PersEvent{
		ID:         PersonID(id),
		RecordTime: now,
		Kind: &WasBorn{
			BornName: name,
		},
	}); err != nil {
		return nil, zerrors.New(ErrCreate).WithError(err)
	}

	return p, nil
}

func (p *Person) Age() error {
	return aggregate.RecordEvent(p, &PersEvent{
		ID:         p.id,
		RecordTime: time.Now(),
		Kind:       &AgedOneYear{},
	})
}

var _ event.GenericEvent = new(PersEvent)

type PersEvent struct {
	ID         PersonID
	RecordTime time.Time
	Kind       personEvent
}

func (p *PersEvent) EventName() string { return p.Kind.EventName() }

//sumtype:decl
type personEvent interface {
	event.GenericEvent
	isPersonEvent()
}

type WasBorn struct {
	BornName string
}

func (*WasBorn) EventName() string { return "person-was-born" }
func (*WasBorn) isPersonEvent()    {}

type AgedOneYear struct{}

func (*AgedOneYear) EventName() string { return "aged-one-year" }
func (*AgedOneYear) isPersonEvent()    {}
