package person_test

import (
	"testing"

	"github.com/DeluxeOwl/chronicle"
	"github.com/DeluxeOwl/chronicle/aggregate"
	"github.com/DeluxeOwl/chronicle/aggregate/snapshotstore"
	"github.com/DeluxeOwl/chronicle/internal/examples/person"
	"github.com/stretchr/testify/require"
)

func TestPlayground(t *testing.T) {
	ctx := t.Context()

	johnID := person.PersonID("some-id")
	p, err := person.New(johnID, "john")

	require.NoError(t, err)

	mem := chronicle.NewEventLogMemory()

	snapshotStore := snapshotstore.NewMemoryStore(person.NewSnapshot)

	repo, err := chronicle.NewEventSourcedRepositoryWithSnapshots(
		mem,
		person.NewEmpty,
		snapshotStore,
		person.NewEmpty(), // Person is a snapshotter.
		aggregate.SnapshotStrategyFor[*person.Person]().EveryNEvents(10),
	)

	require.NoError(t, err)

	for range 44 {
		p.Age()
	}

	_, _, err = repo.Save(ctx, p)
	require.NoError(t, err)

	newp, err := repo.Get(ctx, johnID)
	require.NoError(t, err)

	ps := newp.ToSnapshot(newp)
	require.Equal(t, "john", ps.Name)
	require.Equal(t, 44, ps.Age)

	// agedOneFactory, ok := registry.NewEventFactory("person/aged-one-year")
	// require.True(t, ok)
	// event1 := agedOneFactory()
	// event2 := agedOneFactory()

	// // This is because of the zero sized struct
	// require.Same(t, event1, event2)

	// wasBornFactory, ok := registry.NewEventFactory("person/was-born")
	// require.True(t, ok)
	// event3 := wasBornFactory()
	// event4 := wasBornFactory()
	// require.NotSame(t, event3, event4)

	_, found, err := snapshotStore.GetSnapshot(ctx, johnID)
	require.NoError(t, err)
	require.True(t, found)
}
