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

// TODO: probably need to ensure that it needs an ID
// Maybe I should focus more on the store itself and loading the aggregate from a snapshot and using that
// Because there's plenty of ways to make a snapshot.
//
// what im saying is that maybe its not my responsability as the library author to implement this method below or to dictate how people snapshot things
// but maybe they need a sample implementation or something to get started, it's nice to have sensible defaults
func (p *Person) WithSnapshot() aggregate.WithSnapshot {
	return aggregate.FromSnapshot(func(snap *PersonSnapshot) *Person {
		return &Person{
			id:   snap.ID,
			name: snap.Name,
			age:  snap.Age,
		}
	}).To(func() *PersonSnapshot {
		return &PersonSnapshot{
			ID:   p.id,
			Name: p.name,
			Age:  p.age,
		}
	})
}

func (p *Person) GetSnapshot() *PersonSnapshot {
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
