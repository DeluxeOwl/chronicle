package snapshotstore

import (
	"context"
	"fmt"
	"sync"

	"github.com/DeluxeOwl/chronicle/aggregate"
	"github.com/DeluxeOwl/chronicle/encoding"
)

var _ aggregate.SnapshotStore[aggregate.ID, aggregate.Snapshot[aggregate.ID]] = (*Memory[aggregate.ID, aggregate.Snapshot[aggregate.ID]])(
	nil,
)

// Memory provides a thread-safe, in-memory implementation of the aggregate.SnapshotStore interface.
// It is useful for testing, development, or applications where snapshot persistence is not required.
// Snapshots are stored as encoded byte slices in a map, keyed by the aggregate ID.
type Memory[TID aggregate.ID, TS aggregate.Snapshot[TID]] struct {
	encoder        encoding.Codec
	snapshots      map[string][]byte
	createSnapshot func() TS
	mu             sync.RWMutex
}

type MemoryOption[TID aggregate.ID, TS aggregate.Snapshot[TID]] func(*Memory[TID, TS])

func WithCodec[TID aggregate.ID, TS aggregate.Snapshot[TID]](
	s encoding.Codec,
) MemoryOption[TID, TS] {
	return func(store *Memory[TID, TS]) {
		store.encoder = s
	}
}

// NewMemory creates and returns a new in-memory snapshot store.
// It requires a constructor function for the specific snapshot type, which is used
// to create new instances during decoding. By default, it uses JSON for encoding.
//
// Usage:
//
//	// Assuming account.Snapshot is your snapshot type
//	accountSnapshotStore := snapshotstore.NewMemory(
//		func() *account.Snapshot { return new(account.Snapshot) },
//	)
//
// Returns a pointer to a fully initialized Memory store.
func NewMemory[TID aggregate.ID, TS aggregate.Snapshot[TID]](
	createSnapshot func() TS,
	opts ...MemoryOption[TID, TS],
) *Memory[TID, TS] {
	store := &Memory[TID, TS]{
		mu:             sync.RWMutex{},
		snapshots:      make(map[string][]byte),
		createSnapshot: createSnapshot,
		encoder:        encoding.DefaultJSONB,
	}

	for _, opt := range opts {
		opt(store)
	}

	return store
}

// SaveSnapshot encodes the provided snapshot using the configured encoder and saves
// it to the in-memory map. The operation is thread-safe.
//
// Usage:
//
//	// snapshot is a valid snapshot instance
//	err := store.SaveSnapshot(ctx, snapshot)
//	if err != nil {
//	    // handle error
//	}
//
// Returns an error if the encoding fails or if the context is cancelled.
func (s *Memory[TID, TS]) SaveSnapshot(ctx context.Context, snapshot TS) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	id := snapshot.ID().String()

	data, err := s.encoder.Encode(snapshot)
	if err != nil {
		return fmt.Errorf("save snapshot: marshal: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.snapshots[id] = data

	return nil
}

// GetSnapshot retrieves a snapshot by its aggregate ID, decodes it, and returns it.
// The operation is thread-safe.
//
// Usage:
//
//	// assuming 'id' is a valid aggregate.ID
//	snapshot, found, err := store.GetSnapshot(ctx, id)
//	if err != nil {
//	    // handle storage or decoding error
//	}
//	if !found {
//	    // handle case where no snapshot exists
//	}
//	// use snapshot
//
// Returns the decoded snapshot, a boolean indicating if a snapshot was found for the
// given ID, and an error if one occurred during retrieval or decoding.
func (s *Memory[TID, TS]) GetSnapshot(ctx context.Context, aggregateID TID) (TS, bool, error) {
	var empty TS

	if err := ctx.Err(); err != nil {
		return empty, false, err
	}

	id := aggregateID.String()

	s.mu.RLock()
	data, ok := s.snapshots[id]
	s.mu.RUnlock()

	if !ok {
		return empty, false, nil
	}

	snapshot := s.createSnapshot()
	if err := s.encoder.Decode(data, snapshot); err != nil {
		return empty, false, fmt.Errorf("get snapshot: unmarshal: %w", err)
	}

	return snapshot, true, nil
}
