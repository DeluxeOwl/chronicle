package aggregate_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/DeluxeOwl/chronicle"
	"github.com/DeluxeOwl/chronicle/aggregate"
	"github.com/DeluxeOwl/chronicle/eventlog"
	"github.com/stretchr/testify/require"
)

func Test_SnapshotRepo(t *testing.T) {
	t.Run("should snapshot every 10 events", func(t *testing.T) {
		ctx := t.Context()
		p := createPerson(t, "some-id")

		memlog := eventlog.NewMemory()

		var lastSnapshot *PersonSnapshot
		snapstore := &SnapshotStoreMock[PersonID, *PersonSnapshot]{
			SaveSnapshotFunc: func(ctx context.Context, snapshot *PersonSnapshot) error {
				lastSnapshot = snapshot
				return nil
			},
		}

		registry := chronicle.NewAnyEventRegistry()

		esRepo, err := chronicle.NewEventSourcedRepository(
			memlog,
			NewEmpty,
			nil,
			aggregate.AnyEventRegistry(registry),
		)
		require.NoError(t, err)

		repo, err := chronicle.NewEventSourcedRepositoryWithSnapshots(
			esRepo,
			snapstore,
			NewEmpty(),
			aggregate.SnapStrategyFor[*Person]().EveryNEvents(10),
		)
		require.NoError(t, err)

		p.Age()

		// Shouldn't be called
		_, _, err = repo.Save(ctx, p)
		require.NoError(t, err)
		require.Empty(t, snapstore.calls.SaveSnapshot)
		require.Nil(t, lastSnapshot)

		for range 10 {
			p.Age()
		}
		_, _, err = repo.Save(ctx, p)
		require.NoError(t, err)

		p.Age()
		_, _, err = repo.Save(ctx, p)
		require.NoError(t, err)

		// Should be called once
		require.Len(t, snapstore.calls.SaveSnapshot, 1)

		// Now, we add 9 more, it should be called once again
		for range 9 {
			p.Age()
		}
		_, _, err = repo.Save(ctx, p)
		require.NoError(t, err)
		require.Len(t, snapstore.calls.SaveSnapshot, 2)

		// And now it should be called 3 times
		for range 119 {
			p.Age()
		}
		_, _, err = repo.Save(ctx, p)
		require.NoError(t, err)
		require.Len(t, snapstore.calls.SaveSnapshot, 3)
		require.Equal(t, 140, lastSnapshot.Age)
	})

	t.Run("should ignore snapshot error", func(t *testing.T) {
		ctx := t.Context()
		p := createPerson(t, "some-id")

		memlog := eventlog.NewMemory()

		snapstore := &SnapshotStoreMock[PersonID, *PersonSnapshot]{
			SaveSnapshotFunc: func(ctx context.Context, snapshot *PersonSnapshot) error {
				return errors.New("snapshot error")
			},
		}

		registry := chronicle.NewAnyEventRegistry()
		esRepo, err := chronicle.NewEventSourcedRepository(
			memlog,
			NewEmpty,
			nil,
			aggregate.AnyEventRegistry(registry),
		)
		require.NoError(t, err)
		repo, err := chronicle.NewEventSourcedRepositoryWithSnapshots(
			esRepo,
			snapstore,
			NewEmpty(),
			aggregate.SnapStrategyFor[*Person]().AfterCommit(),
			aggregate.OnSnapshotError(func(ctx context.Context, err error) error {
				return nil
			}),
		)
		require.NoError(t, err)

		for range 44 {
			p.Age()
		}
		_, _, err = repo.Save(ctx, p)
		require.NoError(t, err)
		require.Len(t, snapstore.calls.SaveSnapshot, 1)
	})

	t.Run("should ignore snapshot error when user returns nil", func(t *testing.T) {
		ctx := t.Context()
		p := createPerson(t, "some-id")

		memlog := eventlog.NewMemory()

		snapstore := &SnapshotStoreMock[PersonID, *PersonSnapshot]{
			SaveSnapshotFunc: func(ctx context.Context, snapshot *PersonSnapshot) error {
				return errors.New("snapshot error")
			},
		}

		registry := chronicle.NewAnyEventRegistry()

		esRepo, err := chronicle.NewEventSourcedRepository(
			memlog,
			NewEmpty,
			nil,
			aggregate.AnyEventRegistry(registry),
		)
		require.NoError(t, err)

		repo, err := chronicle.NewEventSourcedRepositoryWithSnapshots(
			esRepo,
			snapstore,
			NewEmpty(),
			aggregate.SnapStrategyFor[*Person]().AfterCommit(),
			aggregate.OnSnapshotError(func(_ context.Context, err error) error {
				// Received a non-nil error, but return nil anyway.
				require.ErrorContains(t, err, "snapshot error")
				require.Error(t, err)
				return nil
			}),
		)
		require.NoError(t, err)

		for range 44 {
			p.Age()
		}
		_, _, err = repo.Save(ctx, p)
		require.NoError(t, err)
		require.Len(t, snapstore.calls.SaveSnapshot, 1)
	})

	t.Run("should return error on snapshot error", func(t *testing.T) {
		ctx := t.Context()
		p := createPerson(t, "some-id")

		memlog := eventlog.NewMemory()

		snapstore := &SnapshotStoreMock[PersonID, *PersonSnapshot]{
			SaveSnapshotFunc: func(ctx context.Context, snapshot *PersonSnapshot) error {
				return errors.New("snapshot error")
			},
		}

		registry := chronicle.NewAnyEventRegistry()

		esRepo, err := chronicle.NewEventSourcedRepository(
			memlog,
			NewEmpty,
			nil,
			aggregate.AnyEventRegistry(registry),
		)
		require.NoError(t, err)

		repo, err := chronicle.NewEventSourcedRepositoryWithSnapshots(
			esRepo,
			snapstore,
			NewEmpty(),
			aggregate.SnapStrategyFor[*Person]().AfterCommit(),
			aggregate.OnSnapshotError(func(_ context.Context, err error) error {
				return fmt.Errorf("user customized message %w", err)
			}),
		)
		require.NoError(t, err)

		for range 44 {
			p.Age()
		}
		_, _, err = repo.Save(ctx, p)
		require.Error(t, err)
		require.ErrorContains(t, err, "user customized message")
		require.Len(t, snapstore.calls.SaveSnapshot, 1)
	})
}
