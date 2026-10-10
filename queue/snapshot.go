package queue

import (
	"context"
	"crypto/sha256"
	"errors"
	"slices"
	"time"

	"github.com/DeluxeOwl/chronicle/aggregate"
	"github.com/DeluxeOwl/chronicle/version"
)

// ID identifies the queue's event stream, not its user-facing name.
type ID string

func (id ID) String() string { return string(id) }

// Snapshot is the encoded queue state used by Chronicle snapshot stores.
// Treat snapshots as opaque data; changing them can corrupt queue state.
type Snapshot struct {
	QueueID       ID                         `json:"queueID"`
	StreamVersion version.Version            `json:"streamVersion"`
	Pending       []SnapshotMessage          `json:"pending"`
	Seen          map[string]SnapshotEnqueue `json:"seen"`
}

// SnapshotMessage records pending work and its current delivery attempt.
type SnapshotMessage struct {
	ID          string    `json:"id"`
	Payload     []byte    `json:"payload"`
	AvailableAt time.Time `json:"availableAt"`
	Receipt     Receipt   `json:"receipt"`
	LeaseUntil  time.Time `json:"leaseUntil"`
	Attempt     uint64    `json:"attempt"`
}

// SnapshotEnqueue preserves the original enqueue identity after acknowledgment.
type SnapshotEnqueue struct {
	PayloadHash [sha256.Size]byte `json:"payloadHash"`
	AvailableAt time.Time         `json:"availableAt"`
}

func (s *Snapshot) ID() ID                   { return s.QueueID }
func (s *Snapshot) Version() version.Version { return s.StreamVersion }

type stateSnapshotter struct{}

func (stateSnapshotter) ToSnapshot(root *state) (*Snapshot, error) {
	snap := &Snapshot{
		QueueID: root.id, StreamVersion: root.Version(),
		Pending: make([]SnapshotMessage, 0, len(root.order)),
		Seen:    make(map[string]SnapshotEnqueue, len(root.seen)),
	}
	for _, id := range root.order {
		msg := root.messages[id]
		snap.Pending = append(snap.Pending, SnapshotMessage{
			ID: id, Payload: slices.Clone(msg.payload), AvailableAt: msg.availableAt,
			Receipt: msg.receipt, LeaseUntil: msg.leaseUntil, Attempt: msg.attempt,
		})
	}
	for id, fingerprint := range root.seen {
		snap.Seen[id] = SnapshotEnqueue{
			PayloadHash: fingerprint.payloadHash,
			AvailableAt: fingerprint.availableAt,
		}
	}
	return snap, nil
}

func (stateSnapshotter) FromSnapshot(snap *Snapshot) (*state, error) {
	root := newState(snap.QueueID)
	for id, fingerprint := range snap.Seen {
		root.seen[id] = enqueueFingerprint{
			payloadHash: fingerprint.PayloadHash,
			availableAt: fingerprint.AvailableAt,
		}
	}
	for _, msg := range snap.Pending {
		root.order = append(root.order, msg.ID)
		root.messages[msg.ID] = &pendingMessage{
			id: msg.ID, payload: slices.Clone(msg.Payload), availableAt: msg.AvailableAt,
			receipt: msg.Receipt, leaseUntil: msg.LeaseUntil, attempt: msg.Attempt,
		}
		if msg.Receipt != "" {
			root.receipts[msg.Receipt] = msg.ID
		}
	}
	return root, nil
}

func configureSnapshots(
	repo aggregate.Repository[ID, queueEvent, *state],
	config options,
) (aggregate.Repository[ID, queueEvent, *state], error) {
	if !config.snapshotsEnabled {
		return repo, nil
	}
	if config.snapshotStore == nil {
		return nil, errors.New("queue snapshot store cannot be nil")
	}
	if config.snapshotEvery == 0 {
		return nil, errors.New("queue snapshot interval must be positive")
	}
	return aggregate.NewESRepoWithSnapshots(
		repo, config.snapshotStore, stateSnapshotter{},
		aggregate.SnapPolicyFor[*state]().EveryNEvents(config.snapshotEvery),
		aggregate.OnSnapshotError(func(context.Context, error) error { return nil }),
	)
}

var _ aggregate.Snapshotter[ID, queueEvent, *state, *Snapshot] = stateSnapshotter{}
