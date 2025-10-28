package aggregate_test

import (
	"testing"

	"github.com/DeluxeOwl/chronicle"
	"github.com/DeluxeOwl/chronicle/aggregate"
	"github.com/DeluxeOwl/chronicle/event"
	"github.com/DeluxeOwl/chronicle/internal/testutils"
	"github.com/DeluxeOwl/chronicle/version"
	"github.com/stretchr/testify/require"
)

func Test_EventDeletion(t *testing.T) {
	eventLogs, closeDBs := testutils.SetupEventLogs(t)
	defer closeDBs()

	for _, el := range eventLogs {
		t.Run(el.Name, func(t *testing.T) {
			deleterLog, ok := el.Log.(event.DeleterLog)
			if !ok {
				t.Skipf("%s does not support deletion of events, skipping", el.Name)
			}

			ctx := t.Context()
			p := createPerson(t, "some-id")

			esRepo, err := chronicle.NewEventSourcedRepository(
				el.Log,
				NewEmpty,
				nil,
				aggregate.AnyEventRegistry(event.NewRegistry[event.Any]()),
			)
			require.NoError(t, err)

			for range 3999 {
				p.Age()
			}

			// Version is 4000, 1 is from wasBorn
			versionBeforeSnapshotEvent := 4000
			// same as below
			// versionBeforeSnapshotEvent := p.Version()

			// Generate a "snapshot event", version is 4001
			err = p.GenerateSnapshotEvent()
			require.NoError(t, err)

			// Age is 6000 now.
			for range 2001 {
				p.Age()
			}

			_, _, err = esRepo.Save(ctx, p)
			require.NoError(t, err)

			// It's up to the user to remember this version somehow.
			err = deleterLog.DangerouslyDeleteEventsUpTo(
				ctx,
				event.LogID(p.ID()),
				version.Version(versionBeforeSnapshotEvent),
			)
			require.NoError(t, err)

			personAfterDelete, err := esRepo.Get(ctx, p.ID())
			require.NoError(t, err)
			require.Equal(t, 6000, personAfterDelete.age)
		})
	}
}
