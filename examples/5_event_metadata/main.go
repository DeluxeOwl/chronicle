package main

import (
	"context"
	"fmt"
	"time"

	"github.com/DeluxeOwl/chronicle"
	"github.com/DeluxeOwl/chronicle/aggregate"
	"github.com/DeluxeOwl/chronicle/eventlog"
	"github.com/DeluxeOwl/chronicle/examples/internal/accountv2"
	"github.com/DeluxeOwl/chronicle/pkg/timeutils"
	"github.com/DeluxeOwl/chronicle/snapshotstore"
	"github.com/DeluxeOwl/chronicle/version"
)

func main() {
	memoryEventLog := eventlog.NewMemory()

	// We're using a mock time provider, generating the current time but setting the year to 2100
	timeProvider := &timeutils.TimeProviderMock{
		NowFunc: func() time.Time {
			now := time.Now()

			futureTime := now.AddDate(2100-now.Year(), 0, 0)

			return futureTime
		},
	}
	accountMaker := accountv2.NewEmptyMaker(timeProvider)

	baseRepo, _ := chronicle.NewEventSourcedRepository(
		memoryEventLog,
		accountMaker,
		nil,
	)

	accountSnapshotStore := snapshotstore.NewMemoryStore(
		func() *accountv2.Snapshot { return new(accountv2.Snapshot) },
	)

	accountRepo, err := chronicle.NewEventSourcedRepositoryWithSnapshots(
		baseRepo,
		accountSnapshotStore,
		&accountv2.Snapshotter{
			TimeProvider: timeProvider, // ⚠️ The same timeProvider
		},
		aggregate.SnapStrategyFor[*accountv2.Account]().EveryNEvents(3),
	)
	if err != nil {
		panic(err)
	}

	ctx := context.Background()
	accID := accountv2.AccountID("snap-123")

	acc, err := accountv2.Open(accID, timeProvider, "John Smith") // ⚠️ The same timeProvider
	if err != nil {
		panic(err)
	}
	_ = acc.DepositMoney(100)
	_ = acc.DepositMoney(100)

	_, _, err = accountRepo.Save(ctx, acc)
	if err != nil {
		panic(err)
	}

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

	for ev := range memoryEventLog.ReadAllEvents(ctx, version.SelectFromBeginning) {
		fmt.Println(string(ev.Data()))
	}
}
