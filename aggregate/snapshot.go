package aggregate

import (
	"context"
	"fmt"

	"github.com/DeluxeOwl/chronicle/event"
)

//go:generate go run github.com/matryer/moq@latest -pkg aggregate_test -skip-ensure -rm -out snapshot_mock_test.go . SnapshotStore Snapshotter Snapshot

// Snapshot represents the contract for a serializable, point-in-time representation
// of an aggregate's state. Implementations of this interface hold the necessary
// data to reconstruct an aggregate, along with its ID and the version at which
// the snapshot was taken.
//
// Usage:
//
//	type AccountSnapshot struct {
//	    AccountID        account.AccountID
//	    AggregateVersion version.Version
//	    Balance          int
//	}
//
//	func (s *AccountSnapshot) ID() account.AccountID { return s.AccountID }
//	func (s *AccountSnapshot) Version() version.Version { return s.AggregateVersion }
type Snapshot[TID ID] interface {
	IDer[TID]
	Versioner
}

// Snapshotter defines the mechanism for converting a live aggregate root into its
// snapshot representation and back. It acts as a bridge between the domain object
// and its persisted state.
//
// Usage:
//
//	type AccountSnapshotter struct{}
//
//	func (s *AccountSnapshotter) ToSnapshot(acc *account.Account) (*account.Snapshot, error) {
//	    return &account.Snapshot{
//	        AccountID:        acc.ID(),
//	        AggregateVersion: acc.Version(),
//	        Balance:          acc.Balance(),
//	    }, nil
//	}
//
//	func (s *AccountSnapshotter) FromSnapshot(snap *account.Snapshot) (*account.Account, error) {
//	    // Recreate the aggregate from snapshot data
//	    acc := account.NewEmpty()
//	    // ... set fields from snap ...
//	    return acc, nil
//	}
type Snapshotter[TID ID, E event.Any, R Root[TID, E], TS Snapshot[TID]] interface {
	ToSnapshot(R) (TS, error)
	FromSnapshot(TS) (R, error)
}

// SnapshotStore defines the contract for a storage mechanism for snapshots.
// Implementations are responsible for persisting and retrieving snapshot data, for example,
// in memory, in a database table, or in a key-value store.
//
// Usage:
//
//	// snapStore is an implementation of SnapshotStore, e.g., snapshotstore.NewMemoryStore()
//	err := snapStore.SaveSnapshot(ctx, mySnapshot)
//	if err != nil {
//	    // handle error
//	}
//
//	snapshot, found, err := snapStore.GetSnapshot(ctx, aggregateID)
//	if err != nil {
//	    // handle error
//	}
//	if found {
//	    // use the snapshot
//	}
type SnapshotStore[TID ID, TS Snapshot[TID]] interface {
	SaveSnapshot(ctx context.Context, snapshot TS) error

	// GetSnapshot retrieves the latest snapshot for a given aggregate ID.
	//
	// Returns the snapshot, a boolean indicating if it was found, and an error if one occurred.
	GetSnapshot(ctx context.Context, aggregateID TID) (TS, bool, error)
}

// LoadFromSnapshot orchestrates the retrieval and rehydration of an aggregate from a snapshot.
// It fetches the latest snapshot from the store and uses the snapshotter to convert it
// back into a live aggregate root instance, with its version correctly set.
//
// Usage:
//
//	// Typically used within a repository's Get method.
//	root, found, err := aggregate.LoadFromSnapshot(
//	    ctx,
//	    snapshotStore,
//	    snapshotter,
//	    aggregateID,
//	)
//	if err != nil {
//	    return nil, err
//	}
//	if found {
//	    // The aggregate is partially loaded from the snapshot.
//	    // Now, load subsequent events from the event log.
//	} else {
//	    // No snapshot found, load from the beginning of the event log.
//	}
//
// Returns the rehydrated aggregate root, a boolean indicating if a snapshot was found,
// and any error that occurred during the process.
func LoadFromSnapshot[TID ID, E event.Any, R Root[TID, E], TS Snapshot[TID]](
	ctx context.Context,
	store SnapshotStore[TID, TS],
	snapshotter Snapshotter[TID, E, R, TS],
	aggregateID TID,
) (R, bool, error) {
	var empty R

	snap, found, err := store.GetSnapshot(ctx, aggregateID)
	if err != nil {
		return empty, found, fmt.Errorf("load from snapshot: get snapshot: %w", err)
	}

	if !found {
		return empty, false, nil
	}

	root, err := snapshotter.FromSnapshot(snap)
	if err != nil {
		return empty, found, fmt.Errorf("load from snapshot: convert from snapshot: %w", err)
	}
	root.setVersion(snap.Version())

	return root, true, nil
}
