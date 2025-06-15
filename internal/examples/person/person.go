package person

import (
	"errors"
	"fmt"

	"github.com/DeluxeOwl/chronicle/aggregate"
)

type PersonID string

func (p PersonID) String() string { return string(p) }

type Person struct {
	aggregate.Base `exhaustruct:"optional"`

	id   PersonID
	name string
	age  int
}

type PersonSnapshot struct {
	ID   PersonID `json:"id"`
	Name string   `json:"name"`
	Age  int      `json:"age"`
}

func (p *Person) ToSnapshot() *PersonSnapshot {
	return &PersonSnapshot{
		ID:   p.id,
		Name: p.name,
		Age:  p.age,
	}
}

func (p *Person) ID() PersonID {
	return PersonID(p.id.String())
}

// Note: you'd add custom dependencies by returning a non-empty
// instance, or creating a closure.
func NewEmpty() *Person {
	return new(Person)
}

func New(id PersonID, name string) (*Person, error) {
	if name == "" {
		return nil, errors.New("empty name")
	}

	p := NewEmpty()

	if err := p.recordThat(&personWasBorn{
		ID:       id,
		BornName: name,
	}); err != nil {
		return nil, fmt.Errorf("create person: %w", err)
	}

	return p, nil
}

func (p *Person) Apply(evt PersonEvent) error {
	switch event := evt.(type) {
	case *personWasBorn:
		p.id = event.ID
		p.age = 0
		p.name = event.BornName
	case *personAgedOneYear:
		p.age++
	default:
		return fmt.Errorf("unexpected event kind: %T", event)
	}

	return nil
}

func (p *Person) Age() error {
	return p.recordThat(&personAgedOneYear{})
}

func (p *Person) recordThat(event PersonEvent) error {
	return aggregate.RecordEvent(p, event)
}
