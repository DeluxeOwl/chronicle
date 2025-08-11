package aggregate_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/DeluxeOwl/chronicle"
	"github.com/DeluxeOwl/chronicle/aggregate"
	"github.com/DeluxeOwl/chronicle/event"
	"github.com/DeluxeOwl/chronicle/eventlog"
	"github.com/DeluxeOwl/chronicle/serde"
	"github.com/DeluxeOwl/chronicle/snapshotstore"

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
	committedEvents aggregate.CommittedEvents[PersonEvent],
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

func createPerson(t *testing.T) *Person {
	t.Helper()
	p, err := New(PersonID("some-id"), "john")
	require.NoError(t, err)
	require.EqualValues(t, 1, p.Version())
	return p
}

func Test_Person(t *testing.T) {
	ctx := t.Context()

	p := createPerson(t)

	memlog := eventlog.NewMemory()

	_, err := chronicle.NewTransactionalRepository(
		memlog,
		NewEmpty,
		nil,
		&TransactionalAggregateProcessorMock[eventlog.MemTx, PersonID, PersonEvent, *Person]{
			ProcessFunc: func(ctx context.Context, tx eventlog.MemTx, root *Person, events aggregate.CommittedEvents[PersonEvent]) error {
				return nil
			},
		},
	)
	require.NoError(t, err)

	snapstore := snapshotstore.NewMemoryStore(NewSnapshot)
	registry := chronicle.NewAnyEventRegistry()

	esRepo, err := chronicle.NewEventSourcedRepository(
		memlog,
		NewEmpty,
		nil,
		aggregate.AnyEventRegistry(registry),
	)
	require.NoError(t, err)

	repo, err := chronicle.NewEventSourcedRepositoryWithSnapshots(
		esRepo,
		snapstore,
		NewEmpty(),
		aggregate.SnapStrategyFor[*Person]().EveryNEvents(10),
	)
	// You could also do: aggregate.SnapStrategyFor[*Person]().Custom(CustomSnapshot),
	// Person is a snapshotter

	require.NoError(t, err)

	for range 44 {
		p.Age()
	}

	_, _, err = repo.Save(ctx, p)
	require.NoError(t, err)

	newp, err := repo.Get(ctx, p.ID())
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

	_, found, err := snapstore.GetSnapshot(ctx, p.ID())
	require.NoError(t, err)
	require.True(t, found)
}

func Test_RecordEvent(t *testing.T) {
	t.Run("record single event", func(t *testing.T) {
		p := createPerson(t)
		err := aggregate.RecordEvent(p, PersonEvent(&personWasBorn{}))
		require.NoError(t, err)
		require.EqualValues(t, 2, p.Version())
	})

	t.Run("record multiple events", func(t *testing.T) {
		p := createPerson(t)
		err := aggregate.RecordEvents(
			p,
			PersonEvent(&personWasBorn{}),
			PersonEvent(&personWasBorn{}),
		)
		require.NoError(t, err)
		require.EqualValues(t, 3, p.Version())
	})

	t.Run("record nil event", func(t *testing.T) {
		p := createPerson(t)
		err := aggregate.RecordEvent(p, PersonEvent(nil))
		require.ErrorContains(t, err, "nil event")
	})
}

func Test_FlushUncommittedEvents(t *testing.T) {
	p := createPerson(t)
	p.Age()

	uncommitted := aggregate.FlushUncommittedEvents(p)
	require.Len(t, uncommitted, 2)

	personWasBornName := new(personWasBorn).EventName()
	personAgedName := new(personAgedOneYear).EventName()

	require.Equal(t, uncommitted[0].EventName(), personWasBornName)
	require.Equal(t, uncommitted[1].EventName(), personAgedName)

	raw, err := aggregate.RawEventsFromUncommitted(
		t.Context(),
		serde.NewJSONBinary(),
		nil,
		uncommitted,
	)
	require.NoError(t, err)
	require.Equal(t, raw[0].EventName(), personWasBornName)
	require.Equal(t, raw[1].EventName(), personAgedName)
}

func Test_CommitEvents(t *testing.T) {
	serializer := serde.NewJSONBinary()
	t.Run("without events", func(t *testing.T) {
		memstore := eventlog.NewMemory()
		p := NewEmpty()

		v, events, err := aggregate.CommitEvents(t.Context(), memstore, serializer, nil, p)
		require.EqualValues(t, 0, v)
		require.Nil(t, events)
		require.NoError(t, err)
	})

	t.Run("with events", func(t *testing.T) {
		memstore := eventlog.NewMemory()
		p := createPerson(t)
		p.Age()

		personWasBornName := new(personWasBorn).EventName()
		personAgedName := new(personAgedOneYear).EventName()

		v, events, err := aggregate.CommitEvents(t.Context(), memstore, serializer, nil, p)
		require.EqualValues(t, 2, v)
		require.Len(t, events, 2)
		require.NoError(t, err)

		records, err := memstore.
			ReadEvents(t.Context(), event.LogID(p.ID()), version.SelectFromBeginning).
			Collect()

		require.NoError(t, err)
		require.Len(t, records, 2)

		require.Equal(t, personWasBornName, records[0].EventName())
		require.Equal(t, personAgedName, records[1].EventName())

		require.EqualValues(t, p.ID(), records[0].LogID())
		require.EqualValues(t, p.ID(), records[1].LogID())
	})
}

func Test_ReadAndLoadFromStore(t *testing.T) {
	serializer := serde.NewJSONBinary()
	t.Run("not found", func(t *testing.T) {
		memlog := eventlog.NewMemory()
		registry := chronicle.NewEventRegistry[PersonEvent]()

		err := aggregate.ReadAndLoadFromStore(
			t.Context(),
			NewEmpty(),
			event.Log(memlog),
			registry,
			serde.BinaryDeserializer(serializer),
			nil,
			PersonID("john"),
			version.SelectFromBeginning,
		)
		require.ErrorContains(t, err, "root not found")
	})

	t.Run("load", func(t *testing.T) {
		p := createPerson(t)
		p.Age()

		memstore := eventlog.NewMemory()
		registry := chronicle.NewEventRegistry[PersonEvent]()
		err := registry.RegisterEvents(p)
		require.NoError(t, err)

		_, _, err = aggregate.CommitEvents(t.Context(), memstore, serializer, nil, p)
		require.NoError(t, err)

		emptyRoot := NewEmpty()
		err = aggregate.ReadAndLoadFromStore(
			t.Context(),
			emptyRoot,
			event.Log(memstore),
			registry,
			serde.BinaryDeserializer(serializer),
			nil,
			p.ID(),
			version.SelectFromBeginning,
		)
		require.NoError(t, err)
		require.EqualValues(t, 2, emptyRoot.Version())
		require.Equal(t, 1, emptyRoot.age)
	})
}

func Test_LoadFromRecords(t *testing.T) {
	p := createPerson(t)
	p.Age()

	serializer := serde.NewJSONBinary()
	memstore := eventlog.NewMemory()
	registry := chronicle.NewEventRegistry[PersonEvent]()

	err := registry.RegisterEvents(p)
	require.NoError(t, err)

	v, events, err := aggregate.CommitEvents(t.Context(), memstore, serializer, nil, p)
	require.EqualValues(t, 2, v)
	require.Len(t, events, 2)
	require.NoError(t, err)

	records := memstore.
		ReadEvents(t.Context(), event.LogID(p.ID()), version.SelectFromBeginning)

	emptyPerson := NewEmpty()
	err = aggregate.LoadFromRecords(t.Context(), emptyPerson, registry, serializer, nil, records)
	require.NoError(t, err)

	require.Equal(t, emptyPerson, p)
}

func Test_SnapshotRepo(t *testing.T) {
	t.Run("should snapshot every 10 events", func(t *testing.T) {
		ctx := t.Context()
		p := createPerson(t)

		memlog := eventlog.NewMemory()

		var lastSnapshot *PersonSnapshot
		snapstore := &SnapshotStoreMock[PersonID, *PersonSnapshot]{
			SaveSnapshotFunc: func(ctx context.Context, snapshot *PersonSnapshot) error {
				lastSnapshot = snapshot
				return nil
			},
		}

		registry := chronicle.NewAnyEventRegistry()

		esRepo, err := chronicle.NewEventSourcedRepository(
			memlog,
			NewEmpty,
			nil,
			aggregate.AnyEventRegistry(registry),
		)
		require.NoError(t, err)

		repo, err := chronicle.NewEventSourcedRepositoryWithSnapshots(
			esRepo,
			snapstore,
			NewEmpty(),
			aggregate.SnapStrategyFor[*Person]().EveryNEvents(10),
		)
		require.NoError(t, err)

		p.Age()

		// Shouldn't be called
		_, _, err = repo.Save(ctx, p)
		require.NoError(t, err)
		require.Empty(t, snapstore.calls.SaveSnapshot)
		require.Nil(t, lastSnapshot)

		for range 10 {
			p.Age()
		}
		_, _, err = repo.Save(ctx, p)
		require.NoError(t, err)

		p.Age()
		_, _, err = repo.Save(ctx, p)
		require.NoError(t, err)

		// Should be called once
		require.Len(t, snapstore.calls.SaveSnapshot, 1)

		// Now, we add 9 more, it should be called once again
		for range 9 {
			p.Age()
		}
		_, _, err = repo.Save(ctx, p)
		require.NoError(t, err)
		require.Len(t, snapstore.calls.SaveSnapshot, 2)

		// And now it should be called 3 times
		for range 119 {
			p.Age()
		}
		_, _, err = repo.Save(ctx, p)
		require.NoError(t, err)
		require.Len(t, snapstore.calls.SaveSnapshot, 3)
		require.Equal(t, 140, lastSnapshot.Age)
	})

	t.Run("should ignore snapshot error", func(t *testing.T) {
		ctx := t.Context()
		p := createPerson(t)

		memlog := eventlog.NewMemory()

		snapstore := &SnapshotStoreMock[PersonID, *PersonSnapshot]{
			SaveSnapshotFunc: func(ctx context.Context, snapshot *PersonSnapshot) error {
				return errors.New("snapshot error")
			},
		}

		registry := chronicle.NewAnyEventRegistry()
		esRepo, err := chronicle.NewEventSourcedRepository(
			memlog,
			NewEmpty,
			nil,
			aggregate.AnyEventRegistry(registry),
		)
		require.NoError(t, err)
		repo, err := chronicle.NewEventSourcedRepositoryWithSnapshots(
			esRepo,
			snapstore,
			NewEmpty(),
			aggregate.SnapStrategyFor[*Person]().AfterCommit(),
		)
		require.NoError(t, err)

		for range 44 {
			p.Age()
		}
		_, _, err = repo.Save(ctx, p)
		require.NoError(t, err)
		require.Len(t, snapstore.calls.SaveSnapshot, 1)
	})

	t.Run("should ignore snapshot error when user returns nil", func(t *testing.T) {
		ctx := t.Context()
		p := createPerson(t)

		memlog := eventlog.NewMemory()

		snapstore := &SnapshotStoreMock[PersonID, *PersonSnapshot]{
			SaveSnapshotFunc: func(ctx context.Context, snapshot *PersonSnapshot) error {
				return errors.New("snapshot error")
			},
		}

		registry := chronicle.NewAnyEventRegistry()

		esRepo, err := chronicle.NewEventSourcedRepository(
			memlog,
			NewEmpty,
			nil,
			aggregate.AnyEventRegistry(registry),
		)
		require.NoError(t, err)

		repo, err := chronicle.NewEventSourcedRepositoryWithSnapshots(
			esRepo,
			snapstore,
			NewEmpty(),
			aggregate.SnapStrategyFor[*Person]().AfterCommit(),
			aggregate.ReturnErrorFunc(func(err error) error {
				// Received a non-nil error, but return nil anyway.
				require.ErrorContains(t, err, "snapshot error")
				require.Error(t, err)
				return nil
			}),
		)
		require.NoError(t, err)

		for range 44 {
			p.Age()
		}
		_, _, err = repo.Save(ctx, p)
		require.NoError(t, err)
		require.Len(t, snapstore.calls.SaveSnapshot, 1)
	})

	t.Run("should return error on snapshot error", func(t *testing.T) {
		ctx := t.Context()
		p := createPerson(t)

		memlog := eventlog.NewMemory()

		snapstore := &SnapshotStoreMock[PersonID, *PersonSnapshot]{
			SaveSnapshotFunc: func(ctx context.Context, snapshot *PersonSnapshot) error {
				return errors.New("snapshot error")
			},
		}

		registry := chronicle.NewAnyEventRegistry()

		esRepo, err := chronicle.NewEventSourcedRepository(
			memlog,
			NewEmpty,
			nil,
			aggregate.AnyEventRegistry(registry),
		)
		require.NoError(t, err)

		repo, err := chronicle.NewEventSourcedRepositoryWithSnapshots(
			esRepo,
			snapstore,
			NewEmpty(),
			aggregate.SnapStrategyFor[*Person]().AfterCommit(),
			aggregate.ReturnErrorFunc(func(err error) error {
				return fmt.Errorf("user customized message %w", err)
			}),
		)
		require.NoError(t, err)

		for range 44 {
			p.Age()
		}
		_, _, err = repo.Save(ctx, p)
		require.Error(t, err)
		require.ErrorContains(t, err, "user customized message")
		require.Len(t, snapstore.calls.SaveSnapshot, 1)
	})
}
