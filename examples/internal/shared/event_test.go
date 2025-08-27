package shared_test

import (
	"testing"

	"github.com/DeluxeOwl/chronicle/examples/internal/shared"
	"github.com/DeluxeOwl/chronicle/pkg/timeutils"
)

func Test_NewMeta(t *testing.T) {
	gen := shared.NewEventMetaGenerator(timeutils.RealTimeProvider())
	meta := gen.NewEventMeta()

	t.Log(meta.EventID)
	t.Log(meta.Time())
}
