package person_test

import (
	"testing"

	"github.com/DeluxeOwl/eventuallynow/internal/examples/person"
	"github.com/stretchr/testify/require"
)

func TestSTh(t *testing.T) {
	// ctx := t.Context()

	johnID := person.PersonID("some-id")
	p, err := person.New(johnID, "john")
	require.NoError(t, err)

	// mem := eventstore.NewMemory()
	// repo := aggregate.NewEventSourcedRepository(mem, person.NewEmpty)

	for range 2 {
		p.Age()
	}

	// err = repo.Save(ctx, p)
	// require.NoError(t, err)

	// newp, err := repo.Get(ctx, johnID)
	// require.NoError(t, err)
	// require.Equal(t, 2, newp.GetAge())
}
