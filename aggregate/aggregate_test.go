package aggregate_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/DeluxeOwl/chronicle"
	"github.com/DeluxeOwl/chronicle/aggregate"
	"github.com/DeluxeOwl/chronicle/event"

	"github.com/DeluxeOwl/chronicle/version"
	"github.com/stretchr/testify/require"
)

type PersonID string

func (p PersonID) String() string { return string(p) }

type Person struct {
	aggregate.Base `exhaustruct:"optional"`

	id   PersonID
	name string
	age  int
}

func (p *Person) ID() PersonID {
	return p.id
}

//sumtype:decl
type PersonEvent interface {
	event.Any
	isPersonEvent()
}

func (p *Person) EventFuncs() event.FuncsFor[PersonEvent] {
	return event.FuncsFor[PersonEvent]{
		func() PersonEvent { return new(personWasBorn) },
		func() PersonEvent { return new(personAgedOneYear) },
	}
}

// Note: events are unexported so people outside the package can't
// use Apply with random events

type personWasBorn struct {
	ID       PersonID `json:"id" exhaustruct:"optional"`
	BornName string   `json:"bornName" exhaustruct:"optional"`
}

func (*personWasBorn) EventName() string { return "person/was-born" }
func (*personWasBorn) isPersonEvent()    {}

type personAgedOneYear struct{}

func (*personAgedOneYear) EventName() string { return "person/aged-one-year" }
func (*personAgedOneYear) isPersonEvent()    {}

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

var _ aggregate.Snapshotter[PersonID, PersonEvent, *Person, *PersonSnapshot] = (*Person)(nil)

type PersonSnapshot struct {
	SnapshotVersion version.Version `json:"snapshotVersion"`
	SnapshotID      PersonID        `json:"snapshotID"`
	Name            string          `json:"name"`
	Age             int             `json:"age"`
}

func NewSnapshot() *PersonSnapshot {
	return new(PersonSnapshot)
}

func (ps *PersonSnapshot) Version() version.Version {
	return ps.SnapshotVersion
}

func (ps *PersonSnapshot) ID() PersonID {
	return ps.SnapshotID
}

func (p *Person) ToSnapshot(person *Person) (*PersonSnapshot, error) {
	return &PersonSnapshot{
		// Important: store the version
		SnapshotVersion: person.Version(),
		SnapshotID:      person.id,
		Name:            person.name,
		Age:             person.age,
	}, nil
}

func (p *Person) FromSnapshot(snapshot *PersonSnapshot) (*Person, error) {
	return &Person{
		id:   snapshot.SnapshotID,
		name: snapshot.Name,
		age:  snapshot.Age,
	}, nil
}

func CustomSnapshot(
	ctx context.Context,
	root *Person,
	previousVersion, newVersion version.Version,
	committedEvents aggregate.CommitedEvents[PersonEvent],
) bool {
	for evt := range committedEvents.All() {
		// This is exhaustive.
		switch evt.(type) {
		case *personAgedOneYear:
			return true
		case *personWasBorn:
			continue
		default:
			continue
		}
	}

	return false
}

func Test_PersonE2E(t *testing.T) {
	ctx := t.Context()

	johnID := PersonID("some-id")
	p, err := New(johnID, "john")

	require.NoError(t, err)

	memlog := chronicle.NewEventLogMemory()
	snapstore := chronicle.NewSnapshotStoreMemory(NewSnapshot)
	registry := chronicle.NewAnyEventRegistry()

	repo, err := chronicle.NewEventSourcedRepositoryWithSnapshots(
		memlog,
		snapstore,
		NewEmpty,
		NewEmpty(),
		aggregate.SnapStrategyFor[*Person]().EveryNEvents(10),
		aggregate.SnapAnyEventRegistry(registry),
	)
	// You could also do: aggregate.SnapStrategyFor[*Person]().Custom(CustomSnapshot),
	// Person is a snapshotter

	require.NoError(t, err)

	for range 44 {
		p.Age()
	}

	_, _, err = repo.Save(ctx, p)
	require.NoError(t, err)

	newp, err := repo.Get(ctx, johnID)
	require.NoError(t, err)

	ps, err := newp.ToSnapshot(newp)
	require.NoError(t, err)
	require.Equal(t, "john", ps.Name)
	require.Equal(t, 44, ps.Age)

	agedOneFactory, ok := registry.GetFunc("person/aged-one-year")
	require.True(t, ok)
	event1 := agedOneFactory()
	event2 := agedOneFactory()

	// This is because of the zero sized struct
	require.Same(t, event1, event2)

	wasBornFactory, ok := registry.GetFunc("person/was-born")
	require.True(t, ok)
	event3 := wasBornFactory()
	event4 := wasBornFactory()
	require.NotSame(t, event3, event4)

	_, found, err := snapstore.GetSnapshot(ctx, johnID)
	require.NoError(t, err)
	require.True(t, found)
}
