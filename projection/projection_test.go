package projection_test

import (
	"context"
	"testing"

	"github.com/DeluxeOwl/chronicle/event"
	"github.com/DeluxeOwl/chronicle/eventlog"
	"github.com/DeluxeOwl/chronicle/projection"
	"github.com/DeluxeOwl/chronicle/version"
)

func TestProjection(t *testing.T) {
	ctx := t.Context()

	log := eventlog.NewMemory()
	checkpointer := &CheckpointerMock{
		GetFunc: func(ctx context.Context, projectionType string) (version.Version, error) {
			return version.Zero, nil
		},
		SetFunc: func(ctx context.Context, projectionType string, v version.Version) error {
			return nil
		},
	}

	pmock := &ProjectionMock{
		EventNamesFunc: func() []string {
			return []string{""}
		},
		HandleFunc: func(ctx context.Context, rec event.GlobalRecord) error {
			return nil
		},
	}

	_ = projection.NewGroup(log, checkpointer, []projection.Projection{pmock})
	log.AppendEvents(ctx, event.LogID("account-123"), version.CheckExact(0), event.RawEvents{
		event.NewRaw("account/opened", []byte(`{"status": "opened"}`)),
	})
}
