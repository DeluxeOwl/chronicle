package person_test

import (
	"testing"

	"github.com/DeluxeOwl/eventuallynow/internal/examples/person"
	"github.com/stretchr/testify/require"
)

func TestRightNumberEvents(t *testing.T) {
	require.Len(t, person.PersonEventNames, 2, "You probably forgot to add the value to the slice and update the switch statement.")
}
