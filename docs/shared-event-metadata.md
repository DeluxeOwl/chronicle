# Shared event metadata

You can find this example in [./examples/5_event_metadata](../examples/5_event_metadata/main.go), [./examples/internal/shared](../examples/internal/shared/event.go) and [./examples/internal/accountv2](../examples/internal/accountv2/).

You might be interested in sharing some fields between events: a timestamp, an event id, correlation ids, some authorization data (like who triggered an event) etc.

This kind of data is very useful for projections: like getting the events in the past 30 days.

Technically you could add this metadata at the `event.Log` layer, but I prefer adding it at the application layer.

We want our events to have the following: a unique id and a timestamp.
For that, we're going to create a shared event that must be embedded by our `AccountEvent`(s).

```go
// in examples/internal/shared/event.go
package shared

type EventMeta interface {
	isEventMeta()
}

type EventMetadata struct {
	EventID   string    `json:"eventID"`
	OccuredAt time.Time `json:"occuredAt"`
}

func (em *EventMetadata) isEventMeta() {}
```

We're going to use the `EventMeta` sealed interface as our compile-time check to remind us to embed the metadata.

We'll create a constructor that will generate `EventMetadata` for us:
```go
func NewEventMetaGenerator(provider timeutils.TimeProvider) *EventMetaGenerator {
	return &EventMetaGenerator{
		gen: func() EventMetadata {
			now := provider.Now()

			return EventMetadata{
				// The uuidv7 contains the timestamp
				EventID: uuid.Must(uuid.NewV7AtTime(now)).String(),
				// Or just same a simple timestamp
				OccuredAt: now,
			}
		},
	}
}

type EventMetaGenerator struct {
	gen func() EventMetadata
}

func (gen *EventMetaGenerator) NewEventMeta() EventMetadata {
	return gen.gen()
}
```

Wait, what's that `timeutils.TimeProvider` type? That type is an interface which helps us mock the time for testing purposes:
```go
package timeutils

import "time"

//go:generate go run github.com/matryer/moq@latest -pkg timeutils -skip-ensure -rm -out now_mock.go . TimeProvider
type TimeProvider interface {
	Now() time.Time
}

var RealTimeProvider = sync.OnceValue(func() *realTimeProvider {
	return &realTimeProvider{}
})

type realTimeProvider struct{}

func (r *realTimeProvider) Now() time.Time {
	return time.Now()
}
```

Moving on, we're going to wire this event metadata into our account. We've created a new package called `accountv2` that we're going to extend with the metadata.

Let's add our `shared.EventMeta` interface to our `AccountEvent`
```go
//sumtype:decl
type AccountEvent interface {
	event.Any
	shared.EventMeta
	isAccountEvent()
}
```

We're gonna get some compiler errors:
```
cannot use new(accountOpened) (value of type *accountOpened) as AccountEvent value in return statement: *accountOpened does not implement AccountEvent (missing method isEventMeta)
```

These errors tell use that we're missing the method `isEventMeta()`, a method that is only satisfied by the `shared.EventMetadata` struct.
This acts as our compile-time check, telling us that we must embed the `shared.EventMetadata` struct to our events.

```go
type accountOpened struct {
	shared.EventMetadata
	ID         AccountID `json:"id"`
	OpenedAt   time.Time `json:"openedAt"`
	HolderName string    `json:"holderName"`
}
// ...
type moneyDeposited struct {
	shared.EventMetadata
	Amount int `json:"amount"`
}
// ...
type moneyWithdrawn struct {
	shared.EventMetadata
	Amount int `json:"amount"`
}
```

Now we've got the `exhaustruct` linter complaining:
```go
func Open(id AccountID, currentTime time.Time, holderName string) (*Account, error) {
	if currentTime.Weekday() == time.Sunday {
		return nil, errors.New("sorry, you can't open an account on Sunday ¯\\_(ツ)_/¯")
	}

	a := NewEmpty()

	// ⚠️ accountv2.accountOpened is missing field EventMetadata (exhaustruct)
	if err := a.recordThat(&accountOpened{
		ID:         id,
		OpenedAt:   currentTime,
		HolderName: holderName,
	}); err != nil {
		return nil, fmt.Errorf("open account: %w", err)
	}

	return a, nil
}
```

We have to modify our code a bit, we're not going to pass the `currentTime` to our `Open(...)` function, instead we'll pass a `timeutils.TimeProvider`.

Some purists might say that this pollutes our domain model, but I believe injecting some dependencies is a pragmatic approach that helps us with testing.
```go
type Account struct {
	aggregate.Base
	// ...

	// technical dependencies
	timeProvider  timeutils.TimeProvider
	metaGenerator *shared.EventMetaGenerator
}

func Open(id AccountID, timeProvider timeutils.TimeProvider, holderName string) (*Account, error) {
	// ...
}
```

We also have to change our empty constructor (the one used by the repositories) to account for these technical dependencies:
```go
// From this

func NewEmpty() *Account {
	return new(Account)
}

// To this
func NewEmptyMaker(timeProvider timeutils.TimeProvider) func() *Account {
	return func() *Account {
		return &Account{
			timeProvider:  timeProvider,
			metaGenerator: shared.NewEventMetaGenerator(timeProvider),
		}
	}
}
```

We have to return a `func() *Account` since repositories expect this type.

Adding this to our `Open(...)` function and using the generator to generate the metadata:
```go
func Open(id AccountID, timeProvider timeutils.TimeProvider, holderName string) (*Account, error) {
	makeEmptyAccount := NewEmptyMaker(timeProvider) // Create the maker with the dependencies
	a := makeEmptyAccount()

	currentTime := a.timeProvider.Now()

	if currentTime.Weekday() == time.Sunday {
		return nil, errors.New("sorry, you can't open an account on Sunday ¯\\_(ツ)_/¯")
	}

	if err := a.recordThat(&accountOpened{
		ID:            id,
		OpenedAt:      currentTime,
		HolderName:    holderName,
		EventMetadata: a.metaGenerator.NewEventMeta(), // We're using the generator to generate the metadata
	}); err != nil {
		return nil, fmt.Errorf("open account: %w", err)
	}

	return a, nil
}
```

Let's update the other places where we're generating events:
```go
err := a.recordThat(&moneyWithdrawn{
		Amount:        amount,
		EventMetadata: a.metaGenerator.NewEventMeta(),
	})
// ...
return a.recordThat(&moneyDeposited{
		Amount:        amount,
		EventMetadata: a.metaGenerator.NewEventMeta(),
	})
```

Our compiler is complaining that it doesn't find the old `NewEmpty` function in the snapshot:
```go
func (s *Snapshotter) FromSnapshot(snap *Snapshot) (*Account, error) {
	acc := NewEmpty() // ⚠️ Not found
	acc.id = snap.ID()
	acc.openedAt = snap.OpenedAt
	// ...
	return acc, nil
}
```

The way to fix this is to provide the same time provider dependency to the `Snapshotter`:
```go
type Snapshotter struct {
	TimeProvider timeutils.TimeProvider
}

func (s *Snapshotter) FromSnapshot(snap *Snapshot) (*Account, error) {
	// Recreate the aggregate from the snapshot's data
	acc := NewEmptyMaker(s.TimeProvider)() // ✅ Create the maker and call it
	acc.id = snap.ID()
	// ...
}
```

We're going to use the snapshot example but adapt it to account for our shared metadata. You can find this in [examples/5_event_metadata](../examples/5_event_metadata/main.go).

We're gonna use a mocked time, generating the current time and setting the year to 2100 (we're in 2100 baby).
```go
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
	accountMaker := accountv2.NewEmptyMaker(timeProvider) // Create the maker

	baseRepo, _ := chronicle.NewEventSourcedRepository(
		memoryEventLog,
		accountMaker, // Pass it into the repo
		nil,
	)
	// ...
}
```

It's really important to pass the same `timeProvider` to the snapshotter:
```go
	accountRepo, err := chronicle.NewEventSourcedRepositoryWithSnapshots(
			baseRepo,
			accountSnapshotStore,
			&accountv2.Snapshotter{
				TimeProvider: timeProvider, // ⚠️ The same timeProvider
			},
			aggregate.SnapPolicyFor[*accountv2.Account]().EveryNEvents(3),
		)
```

And in our `accountv2.Open`:
```go
	acc, err := accountv2.Open(accID, timeProvider, "John Smith") // ⚠️ The same timeProvider
	// ...
	// Print the events
	for ev := range memoryEventLog.ReadAllEvents(ctx, version.SelectFromBeginning) {
		fmt.Println(ev.EventName() + " " + string(ev.Data()))
	}
```

Running the example:
```bash
go run examples/5_event_metadata/main.go

Loaded account from snapshot. Version: 3
Found snapshot: true: &{AccountID:snap-123 OpenedAt:2100-08-27 11:20:56.239593 +0300 EEST Balance:200 HolderName:John Smith AggregateVersion:3}

account/opened {"eventID":"03bff837-ff2f-7d26-a69e-1a75693570ab","occuredAt":"2100-08-27T11:20:56.239656+03:00","id":"snap-123","openedAt":"2100-08-27T11:20:56.239593+03:00","holderName":"John Smith"}
account/money_deposited {"eventID":"03bff837-ff2f-7d27-a50b-a81c5f7a33eb","occuredAt":"2100-08-27T11:20:56.239665+03:00","amount":100}
account/money_deposited {"eventID":"03bff837-ff2f-7d28-8c54-9594563b48b4","occuredAt":"2100-08-27T11:20:56.239666+03:00","amount":100}
```
