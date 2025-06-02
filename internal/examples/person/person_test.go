package person_test

import (
	"testing"

	"github.com/DeluxeOwl/eventuallynow/aggregate"
	"github.com/DeluxeOwl/eventuallynow/event"
	memoryadapter "github.com/DeluxeOwl/eventuallynow/event/adapter"
	"github.com/DeluxeOwl/eventuallynow/internal/examples/person"
	"github.com/DeluxeOwl/eventuallynow/version"
	"github.com/sanity-io/litter"
	"github.com/stretchr/testify/require"
)

func TestSomething(t *testing.T) {
	ctx := t.Context()

	memoryStore := memoryadapter.NewMemoryStore()

	// Typically you'd pass the serde config to the event sourced repository
	repo := aggregate.NewEventSourcedRepository(memoryStore, person.NewEmpty)

	id := person.PersonID("some-generated-id")
	p, err := person.New(id, "Johnny")
	require.NoError(t, err)

	ageIncrease := 2

	for range ageIncrease {
		err = p.Age()
		require.NoError(t, err)
	}

	snap, err := person.Serde.Serialize(p)
	require.NoError(t, err)
	require.Equal(t, ageIncrease, snap.Age)

	err = repo.Save(ctx, p)
	require.NoError(t, err)

	p, err = repo.Get(ctx, id)
	require.NoError(t, err)

	snap, err = person.Serde.Serialize(p)
	require.NoError(t, err)

	require.Equal(t, ageIncrease, snap.Age)
	require.Equal(t, "Johnny", snap.Name)
	require.EqualValues(t, "some-generated-id", snap.ID)

	litter.Config.HidePrivateFields = false

	recordedEvents := memoryStore.ReadEvents(ctx, event.LogID(id.String()), version.SelectFromBeginning)

	events, err := recordedEvents.AsSlice()
	require.NoError(t, err)
	require.Len(t, events, 3)
}
