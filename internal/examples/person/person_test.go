package person_test

import (
	"testing"

	"github.com/DeluxeOwl/chronicle"
	"github.com/DeluxeOwl/chronicle/aggregate"
	"github.com/DeluxeOwl/chronicle/internal/examples/person"
	"github.com/stretchr/testify/require"
)

func TestPlayground(t *testing.T) {
	ctx := t.Context()

	johnID := person.PersonID("some-id")
	p, err := person.New(johnID, "john")

	require.NoError(t, err)

	memlog := chronicle.NewEventLogMemory()
	snapstore := chronicle.NewSnapshotStoreMemory(person.NewSnapshot)
	registry := chronicle.NewAnyEventRegistry()

	repo, err := chronicle.NewEventSourcedRepositoryWithSnapshots(
		memlog,
		snapstore,
		person.NewEmpty,
		person.NewEmpty(),
		aggregate.SnapStrategyFor[*person.Person]().EveryNEvents(10),
		aggregate.SnapAnyEventRegistry(registry),
	)
	// You could also do: aggregate.SnapStrategyFor[*person.Person]().Custom(person.CustomSnapshot),
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
