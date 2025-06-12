package person_test

import (
	"testing"

	"github.com/DeluxeOwl/chronicle"
	"github.com/DeluxeOwl/chronicle/event"
	"github.com/DeluxeOwl/chronicle/internal/examples/person"
	"github.com/stretchr/testify/require"
)

func TestSTh(t *testing.T) {
	ctx := t.Context()

	johnID := person.PersonID("some-id")
	p, err := person.New(johnID, "john")

	require.NoError(t, err)

	mem := chronicle.NewEventLogMemory()
	repo := chronicle.NewAggregateRepository(mem, person.NewEmpty)

	for range 2 {
		p.Age()
	}

	err = repo.Save(ctx, p)
	require.NoError(t, err)

	newp, err := repo.Get(ctx, johnID)
	require.NoError(t, err)

	ps := newp.Snapshot()
	require.Equal(t, "john", ps.Name)
	require.Equal(t, 2, ps.Age)

	agedOneFactory, ok := event.GlobalRegistry.NewEventFactory("person/aged-one-year")
	require.True(t, ok)
	event1 := agedOneFactory()
	event2 := agedOneFactory()

	require.NotSame(t, event1, event2)
}
