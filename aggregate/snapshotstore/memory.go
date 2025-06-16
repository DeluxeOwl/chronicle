package snapshotstore

import (
	"context"
	"fmt"
	"sync"

	"github.com/DeluxeOwl/chronicle/aggregate"
)

var _ aggregate.SnapshotStore[aggregate.ID, aggregate.Snapshot[aggregate.ID]] = (*MemoryStore[aggregate.ID, aggregate.Snapshot[aggregate.ID]])(nil)

type MemoryStore[TID aggregate.ID, TS aggregate.Snapshot[TID]] struct {
	mu             sync.RWMutex
	snapshots      map[string][]byte
	createSnapshot func() TS
	serde          aggregate.SnapshotSerializer
}

type MemoryStoreOption[TID aggregate.ID, TS aggregate.Snapshot[TID]] func(*MemoryStore[TID, TS])

func WithSerializer[TID aggregate.ID, TS aggregate.Snapshot[TID]](s aggregate.SnapshotSerializer) MemoryStoreOption[TID, TS] {
	return func(store *MemoryStore[TID, TS]) {
		store.serde = s
	}
}

func NewMemoryStore[TID aggregate.ID, TS aggregate.Snapshot[TID]](createSnapshot func() TS, opts ...MemoryStoreOption[TID, TS]) *MemoryStore[TID, TS] {
	store := &MemoryStore[TID, TS]{
		mu:             sync.RWMutex{},
		snapshots:      make(map[string][]byte),
		createSnapshot: createSnapshot,
		serde:          aggregate.NewJSONSnapshotSerializer(),
	}

	for _, opt := range opts {
		opt(store)
	}

	return store
}

// SaveSnapshot serializes the provided snapshot using the configured serde and saves it.
func (s *MemoryStore[TID, TS]) SaveSnapshot(ctx context.Context, snapshot TS) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	id := snapshot.ID().String()

	data, err := s.serde.MarshalSnapshot(snapshot)
	if err != nil {
		return fmt.Errorf("save snapshot: marshal: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.snapshots[id] = data

	return nil
}

// GetSnapshot retrieves a snapshot and deserializes it using the configured serde.
func (s *MemoryStore[TID, TS]) GetSnapshot(ctx context.Context, aggregateID TID) (TS, bool, error) {
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
	if err := s.serde.UnmarshalSnapshot(data, snapshot); err != nil {
		return empty, false, fmt.Errorf("get snapshot: unmarshal: %w", err)
	}

	return snapshot, true, nil
}
