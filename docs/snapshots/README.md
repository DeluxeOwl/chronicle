# Snapshots

You can find this example in [./examples/3_snapshots/main.go](../examples/3_snapshots/main.go) and [./examples/internal/account/account_snapshot.go](../examples/internal/account/account_snapshot.go).

During the lifecycle of an application, some aggregates might accumulate a very long list of events.

For example, an account could exist for decades and accumulate thousands or even tens of thousands of transactions.

Loading such an aggregate would require fetching and replaying its entire history from the beginning, which could become a performance bottleneck. (As a rule of thumb, always measure first-loading even a few thousand events is often perfectly acceptable performance-wise).

Snapshots are a performance optimization that solves this problem. 

A snapshot is an encoded copy of an aggregate's state at a specific version. Instead of replaying the entire event history, the system can load the *latest snapshot* and then replay only the events that have occurred _since_ that snapshot was taken.

Let's continue our `Account` example from the quickstart and add snapshot functionality. 

First we need to create our snapshot struct which needs to satisfy the interface `aggregate.Snapshot[TID]` (by implementing `ID() AccountID` and `Version() version.Version`). 

This means it must store the aggregate's ID and version, which are then exposed via the required `ID()` and `Version()` methods.

```go
package account

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
```

It's a "snapshot" (a point in time picture) of the `Account`'s state at a given point that can be encoded (JSON by default).

Next, we need a way to convert an `Account` aggregate to an `AccountSnapshot` and back. This is the job of a `Snapshotter`. It acts as a bridge between your live aggregate and its encoded snapshot representation.

We create a type that implements the `aggregate.Snapshotter` interface.

```go
package account

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
```

The `ToSnapshot` method captures the current state, and `FromSnapshot` restores it. 

Note that `FromSnapshot` doesn't need to set the version on the aggregate's embedded `Base`; the framework handles this automatically when loading from a snapshot.



Let's wire everything up in `main`:
```go
package main

import (
	"context"
	"fmt"
	"time"

	"github.com/DeluxeOwl/chronicle"
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

	accountSnapshotStore := snapshotstore.NewMemory(
		func() *account.Snapshot { return new(account.Snapshot) },
	)
	// ...
}
```

We create a snapshot store for our snapshots. It needs a constructor for an empty snapshot, used for decoding.

While the library provides an in-memory snapshot store out of the box, the `aggregate.SnapshotStore` interface makes it straightforward to implement your own persistent store (e.g., using a database table, Redis, or a file-based store).

Then, we wrap the base repository with the snapshotting functionality. 

Now, we need to decide _when_ to take a snapshot. You probably don't want to create one on every single change, as that would be inefficient. The framework provides a flexible `SnapshotPolicy` to define this policy and a builder for various policies:
```go
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
```

In our example, we chose to snapshot every 3 events.

Let's issue some commands:
```go
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
```

The repository loads the snapshot at version 3. Then, it will ask the event log for events for "snap-123" starting from version 4. Since there are none, loading is complete, and very fast.

```go
	reloadedAcc, err := accountRepo.Get(ctx, accID)
	if err != nil {
		panic(err)
	}

	fmt.Printf("Loaded account from snapshot. Version: %d\n", reloadedAcc.Version())
	// Loaded account from snapshot. Version: 3
```

We can also get the snapshot from the store to check:
```go
	snap, found, err := accountSnapshotStore.GetSnapshot(ctx, accID)
	if err != nil {
		panic(err)
	}
	fmt.Printf("Found snapshot: %t: %+v\n", found, snap)
	// Found snapshot: true: &{AccountID:snap-123 OpenedAt:2025-08-25 10:53:57.970965 +0300 EEST Balance:200 HolderName:John Smith AggregateVersion:3}
```

## Further reading

- [Snapshot policies](snapshot-policies.md)
