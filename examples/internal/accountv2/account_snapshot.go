package accountv2

import (
	"context"
	"time"

	"github.com/DeluxeOwl/chronicle/aggregate"
	"github.com/DeluxeOwl/chronicle/version"
)

type Snapshot struct {
	AccountID        AccountID       `json:"id"`
	OpenedAt         time.Time       `json:"openedAt"`
	Balance          int             `json:"balance"`
	HolderName       string          `json:"holderName"`
	AggregateVersion version.Version `json:"version"`
}

func (s *Snapshot) ID() AccountID {
	return s.AccountID
}

func (s *Snapshot) Version() version.Version {
	return s.AggregateVersion
}

type Snapshotter struct{}

func (s *Snapshotter) ToSnapshot(acc *Account) (*Snapshot, error) {
	return &Snapshot{
		AccountID:        acc.ID(), // Important: save the aggregate's id
		OpenedAt:         acc.openedAt,
		Balance:          acc.balance,
		HolderName:       acc.holderName,
		AggregateVersion: acc.Version(), // Important: save the aggregate's version
	}, nil
}

func (s *Snapshotter) FromSnapshot(snap *Snapshot) (*Account, error) {
	// Recreate the aggregate from the snapshot's data
	acc := NewEmpty()
	acc.id = snap.ID()
	acc.openedAt = snap.OpenedAt
	acc.balance = snap.Balance
	acc.holderName = snap.HolderName

	// ⚠️ The repository will set the correct version on the aggregate's Base
	return acc, nil
}

func CustomSnapshot(
	ctx context.Context,
	root *Account,
	_, _ version.Version,
	_ aggregate.CommittedEvents[AccountEvent],
) bool {
	return root.balance%250 == 0
}
