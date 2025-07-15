package event_test

import (
	"testing"

	"github.com/DeluxeOwl/chronicle/event"
	"github.com/stretchr/testify/require"
)

func TestEvent_AnyToConcrete(t *testing.T) {
	t.Parallel()
	e := new(fooEventAlpha)
	concrete, ok := event.AnyToConcrete[FooEvent](e)
	require.True(t, ok)
	require.IsType(t, new(fooEventAlpha), concrete)
}
