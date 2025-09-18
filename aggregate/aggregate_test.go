package aggregate_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"github.com/DeluxeOwl/chronicle"
	"github.com/DeluxeOwl/chronicle/aggregate"
	"github.com/DeluxeOwl/chronicle/event"
	"github.com/DeluxeOwl/chronicle/eventlog"
	"github.com/DeluxeOwl/chronicle/internal/testutils"
	"github.com/DeluxeOwl/chronicle/serde"

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
		func() PersonEvent { return new(personSnapEvent) },
		func() PersonEvent { return new(nameAndAgeSetV1) }, // For reading old data
		func() PersonEvent { return new(nameSetV2) },
		func() PersonEvent { return new(ageSetV2) },
		func() PersonEvent { return new(multipleYearsAged) },
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

type personSnapEvent struct {
	ID   PersonID `json:"id"`
	Name string   `json:"name"`
	Age  int      `json:"age"`
}

func (*personSnapEvent) EventName() string { return "person/snapshot-event" }
func (*personSnapEvent) isPersonEvent()    {}

// For the "splitting" (up-casting) test
type nameAndAgeSetV1 struct {
	Name string `json:"name"`
	Age  int    `json:"age"`
}

func (*nameAndAgeSetV1) EventName() string { return "person/name-and-age-set-v1" }
func (*nameAndAgeSetV1) isPersonEvent()    {}

type nameSetV2 struct {
	Name string `json:"name"`
}

func (*nameSetV2) EventName() string { return "person/name-set-v2" }
func (*nameSetV2) isPersonEvent()    {}

type ageSetV2 struct {
	Age int `json:"age"`
}

func (*ageSetV2) EventName() string { return "person/age-set-v2" }
func (*ageSetV2) isPersonEvent()    {}

// For the "merging" (batching) test
type multipleYearsAged struct {
	Years int `json:"years"`
}

func (*multipleYearsAged) EventName() string { return "person/multiple-years-aged" }
func (*multipleYearsAged) isPersonEvent()    {}

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

// Update Person.Apply() to handle the new events
func (p *Person) Apply(evt PersonEvent) error {
	switch event := evt.(type) {
	case *personSnapEvent:
		p.id = event.ID
		p.age = event.Age
		p.name = event.Name
	case *personWasBorn:
		p.id = event.ID
		p.age = 0
		p.name = event.BornName
	case *personAgedOneYear:
		p.age++
	case *nameSetV2:
		p.name = event.Name
	case *ageSetV2:
		p.age = event.Age
	case *multipleYearsAged:
		p.age += event.Years
	// nameAndAgeSetV1 doesn't need a handler because it should never be applied directly;
	// it will always be transformed into V2 events before Apply is called.
	case *nameAndAgeSetV1:
		return fmt.Errorf("should never be called: %T", event)
	default:
		return fmt.Errorf("unexpected event kind: %T", event)
	}

	return nil
}

func (p *Person) Age() error {
	return p.recordThat(&personAgedOneYear{})
}

func (p *Person) GenerateSnapshotEvent() error {
	return p.recordThat(&personSnapEvent{
		ID:   p.id,
		Name: p.name,
		Age:  p.age,
	})
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
		case *personWasBorn, *personSnapEvent, *ageSetV2, *multipleYearsAged, *nameAndAgeSetV1, *nameSetV2:
			continue
		default:
			continue
		}
	}

	return false
}

func createPerson(t *testing.T, id string) *Person {
	t.Helper()
	p, err := New(PersonID(id), "john")
	require.NoError(t, err)
	require.EqualValues(t, 1, p.Version())
	return p
}

func Test_Repos_With_Snapshots(t *testing.T) {
	eventLogs, closeDBs := testutils.SetupEventLogs(t)
	defer closeDBs()

	for i, el := range eventLogs {
		t.Run(el.Name, func(t *testing.T) {
			snapstores, cleanSnapstores := testutils.SetupSnapStores(t, NewSnapshot)
			defer cleanSnapstores()

			for j, ss := range snapstores {
				t.Run(ss.Name, func(t *testing.T) {
					ctx := t.Context()
					p := createPerson(t, fmt.Sprintf("person-id-%d-%d", i, j))
					registry := chronicle.NewAnyEventRegistry()

					esRepo, err := chronicle.NewEventSourcedRepository(
						el.Log,
						NewEmpty,
						nil,
						aggregate.AnyEventRegistry(registry),
					)
					require.NoError(t, err)

					// You could also do: aggregate.SnapStrategyFor[*Person]().Custom(CustomSnapshot),
					// Person is a snapshotter
					repo, err := chronicle.NewEventSourcedRepositoryWithSnapshots(
						esRepo,
						ss.Store,
						NewEmpty(),
						aggregate.SnapStrategyFor[*Person]().EveryNEvents(10),
					)
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

					_, found, err := ss.Store.GetSnapshot(ctx, p.ID())
					require.NoError(t, err)
					require.True(t, found)
				})
			}
		})
	}
}

func Test_TransactionalRepository(t *testing.T) {
	eventLogs, closeDBs := testutils.SetupSQLTransactionalLogs(t)
	defer closeDBs()

	for _, el := range eventLogs {
		t.Run(el.Name, func(t *testing.T) {
			t.Run("success case - processor commits with aggregate events", func(t *testing.T) {
				ctx := t.Context()
				personID := fmt.Sprintf("tx-person-success-%s", el.Name)
				p := createPerson(t, personID)
				require.NoError(t, p.Age()) // Version is now 2 (create + age)

				// This processor will track the events it sees.
				processor := &TransactionalAggregateProcessorMock[*sql.Tx, PersonID, PersonEvent, *Person]{
					ProcessFunc: func(ctx context.Context, tx *sql.Tx, root *Person, events aggregate.CommittedEvents[PersonEvent]) error {
						return nil
					},
				}

				repo, err := chronicle.NewTransactionalRepository(
					el.Log,
					NewEmpty,
					nil,
					processor,
				)
				require.NoError(t, err)

				// Save the aggregate. This should trigger the processor.
				_, _, err = repo.Save(ctx, p)
				require.NoError(t, err)

				// 1. Verify the processor was called and saw the right events.
				// It should see both the creation and the aging event.
				require.Len(t, processor.calls.Process, 1)
				require.Len(t, processor.calls.Process[0].Events, 2)
				require.Equal(
					t,
					"person/was-born",
					processor.calls.Process[0].Events[0].EventName(),
				)
				require.Equal(
					t,
					"person/aged-one-year",
					processor.calls.Process[0].Events[1].EventName(),
				)

				// 2. Verify the events were actually committed to the log.
				loadedPerson, err := repo.Get(ctx, PersonID(personID))
				require.NoError(t, err)
				require.Equal(t, version.Version(2), loadedPerson.Version())
				require.Equal(t, 1, loadedPerson.age)
			})

			t.Run(
				"failure case - transaction is rolled back if processor fails",
				func(t *testing.T) {
					ctx := t.Context()
					personID := fmt.Sprintf("tx-person-fail-%s", el.Name)
					p := createPerson(t, personID) // Version 1

					// This processor will intentionally fail.
					processor := &TransactionalAggregateProcessorMock[*sql.Tx, PersonID, PersonEvent, *Person]{
						ProcessFunc: func(ctx context.Context, tx *sql.Tx, root *Person, events aggregate.CommittedEvents[PersonEvent]) error {
							return errors.New("processor failed intentionally")
						},
					}

					repo, err := chronicle.NewTransactionalRepository(
						el.Log,
						NewEmpty,
						nil,
						processor,
					)
					require.NoError(t, err)

					// Save should fail because our processor returns an error.
					_, _, err = repo.Save(ctx, p)
					require.Error(t, err)
					require.ErrorContains(t, err, "processor failed intentionally")

					// Verify NO events were committed to the log due to the rollback.
					// We expect Get to fail with ErrRootNotFound because the creation event was never saved.
					_, err = repo.Get(ctx, PersonID(personID))
					require.ErrorIs(t, err, aggregate.ErrRootNotFound)
				},
			)
		})
	}
}

func Test_RecordEvent(t *testing.T) {
	t.Run("record single event", func(t *testing.T) {
		p := createPerson(t, "some-id")
		err := aggregate.RecordEvent(p, PersonEvent(&personWasBorn{}))
		require.NoError(t, err)
		require.EqualValues(t, 2, p.Version())
	})

	t.Run("record multiple events", func(t *testing.T) {
		p := createPerson(t, "some-id")
		err := aggregate.RecordEvents(
			p,
			PersonEvent(&personWasBorn{}),
			PersonEvent(&personWasBorn{}),
		)
		require.NoError(t, err)
		require.EqualValues(t, 3, p.Version())
	})

	t.Run("record nil event", func(t *testing.T) {
		p := createPerson(t, "some-id")
		err := aggregate.RecordEvent(p, PersonEvent(nil))
		require.ErrorContains(t, err, "nil event")
	})
}

func Test_FlushUncommittedEvents(t *testing.T) {
	p := createPerson(t, "some-id")
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
		p := createPerson(t, "some-id")
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
		require.ErrorContains(t, err, aggregate.ErrRootNotFound.Error())
	})

	t.Run("load", func(t *testing.T) {
		p := createPerson(t, "some-id")
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
	p := createPerson(t, "some-id")
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

func Test_Repository_GetVersion(t *testing.T) {
	ctx := t.Context()

	pg, clean := testutils.SetupPostgres(t)
	defer clean()

	pglog, err := eventlog.NewPostgres(pg)
	require.NoError(t, err)

	repo, err := chronicle.NewEventSourcedRepository(
		pglog, NewEmpty, nil,
	)
	require.NoError(t, err)

	// Setup: Create a person and give them a history of 10 events.
	personID := PersonID("get-version-id")
	p, err := New(personID, "doc") // Event 1, version 1, age 0
	require.NoError(t, err)

	for range 9 {
		p.Age() // Events 2-10, versions 2-10, age will be 9
	}
	require.Equal(t, version.Version(10), p.Version())
	require.Equal(t, 9, p.age)

	_, _, err = repo.Save(ctx, p)
	require.NoError(t, err)

	//nolint:exhaustruct // Unnecessary.
	testCases := []struct {
		name        string
		id          PersonID
		selector    version.Selector
		expectErr   bool
		errContains string
		expectedAge int
		expectedVer version.Version
	}{
		{
			name:        "get state at an intermediate version",
			id:          personID,
			selector:    version.Selector{To: 5},
			expectErr:   false,
			expectedAge: 4, // Born (age 0) + 4 aging events (v2, v3, v4, v5)
			expectedVer: 5,
		},
		{
			name:        "get state at the latest version",
			id:          personID,
			selector:    version.Selector{To: 10},
			expectErr:   false,
			expectedAge: 9, // Born (age 0) + 9 aging events
			expectedVer: 10,
		},
		{
			name: "get state with version selector beyond the latest",
			id:   personID,
			// Requesting a version that doesn't exist should load up to the latest available.
			selector:    version.Selector{To: 99},
			expectErr:   false,
			expectedAge: 9,
			expectedVer: 10,
		},
		{
			name:        "get state at version 1 (creation only)",
			id:          personID,
			selector:    version.Selector{To: 1},
			expectErr:   false,
			expectedAge: 0,
			expectedVer: 1,
		},
		{
			name:        "get state for a non-existent aggregate",
			id:          PersonID("non-existent-id"),
			selector:    version.Selector{To: 5},
			expectErr:   true,
			errContains: aggregate.ErrRootNotFound.Error(),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			loadedPerson, err := repo.GetVersion(ctx, tc.id, tc.selector)

			if tc.expectErr {
				require.Error(t, err)
				require.ErrorContains(t, err, tc.errContains)
				return
			}

			require.NoError(t, err)
			require.NotNil(t, loadedPerson)
			require.Equal(t, tc.id, loadedPerson.id)
			require.Equal(t, "doc", loadedPerson.name)
			require.Equal(
				t,
				tc.expectedAge,
				loadedPerson.age,
				"Incorrect age for the loaded version",
			)
			require.Equal(
				t,
				tc.expectedVer,
				loadedPerson.Version(),
				"Incorrect version for the loaded aggregate",
			)
		})
	}
}
