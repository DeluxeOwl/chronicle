package main

import (
	"context"
	"fmt"
	"time"

	"github.com/DeluxeOwl/chronicle"
	"github.com/DeluxeOwl/chronicle/aggregate"
	"github.com/DeluxeOwl/chronicle/eventlog"
	"github.com/DeluxeOwl/chronicle/examples/internal/account"
	"github.com/DeluxeOwl/chronicle/snapshotstore"
)

func main() {
	memoryEventLog := eventlog.NewMemory()
	baseRepo, _ := chronicle.NewEventSourcedRepository(
		memoryEventLog,
		account.NewEmpty,
		nil,
	)

	// 1. Create a snapshot store for our AccountSnapshot
	// It needs a constructor for an empty snapshot, used for decoding.
	accountSnapshotStore := snapshotstore.NewMemory(
		func() *account.Snapshot { return new(account.Snapshot) },
	)

	accountRepo, err := chronicle.NewEventSourcedRepositoryWithSnapshots(
		baseRepo,
		accountSnapshotStore,
		&account.Snapshotter{},
		aggregate.SnapPolicyFor[*account.Account]().EveryNEvents(3),
	)
	if err != nil {
		panic(err)
	}

	ctx := context.Background()
	accID := account.AccountID("snap-123")

	acc, err := account.Open(accID, time.Now(), "John Smith") // version 1
	if err != nil {
		panic(err)
	}
	_ = acc.DepositMoney(100) // version 2
	_ = acc.DepositMoney(100) // version 3

	// Saving the aggregate with 3 uncommitted events.
	// The new version will be 3.
	// Since 3 >= 3 (our N), the policy will trigger a snapshot.
	_, _, err = accountRepo.Save(ctx, acc)
	if err != nil {
		panic(err)
	}

	// Now, let's load the aggregate again.
	// The repository will first load the snapshot at version 3 from the snapshot store.
	// Then, it will ask the event log for events for "snap-123" starting from version 4.
	// Since there are none, loading is complete, and very fast.
	reloadedAcc, err := accountRepo.Get(ctx, accID)
	if err != nil {
		panic(err)
	}

	fmt.Printf("Loaded account from snapshot. Version: %d\n", reloadedAcc.Version())

	snap, found, err := accountSnapshotStore.GetSnapshot(ctx, accID)
	if err != nil {
		panic(err)
	}
	fmt.Printf("Found snapshot: %t: %+v\n", found, snap)
}
