# Event deletion

> [!WARNING] 
> Event deletion breaks the purity of the event log and pollutes your domain events. 
> Event deletion can cause data loss or inconsistent state and should be carefully considered and tested.

In an ideal world, an event log is append only and keeps the entire history of all events across all aggregates.

**Why would you want to delete events?** Purely for storage reduction when the intermediate history is not considered valuable for auditing or debugging.

For example, in an IoT an application, you might have millions of `TemperatureChanged` events for a sensor, but you only care about the state at the end of each day.

Event deletion is lossy and dangerous, because if you don't do it properly, it can cause data loss.

**The critical difference from Snapshots**: Regular snapshots are a performance cache. The full event log is still the source of truth. With compaction, the original events are gone forever. You can no longer "time travel" to a state before the compaction. 

You can take a look at the `Test_EventDeletion` test in [aggregate_deletion_test.go](../aggregate/aggregate_deletion_test.go).

A pattern for deleting events safely is generating a snapshot event (**not related to the snapshot store**):
```go
func (p *Person) Apply(evt PersonEvent) error {
	switch event := evt.(type) {
	case *personSnapEvent:
		p.id = event.ID
		p.age = event.Age
		p.name = event.Name
		// ...
}

func (p *Person) GenerateSnapshotEvent() error {
	return p.recordThat(&personSnapEvent{
		ID:   p.id,
		Name: p.name,
		Age:  p.age,
	})
}
```

And remembering the version **before** that snapshot event.
```go
	for range 3999 {
		p.Age()
	}

	// Version is 4000, 1 is from wasBorn
	versionBeforeSnapshotEvent := 4000
	// same as below
	// versionBeforeSnapshotEvent := p.Version()

	// Generate a "snapshot event", version is 4001
	err = p.GenerateSnapshotEvent()

	_, _, err = esRepo.Save(ctx, p)

	// It's up to the user to remember this version somehow.
	err = deleterLog.DangerouslyDeleteEventsUpTo(
		ctx,
		event.LogID(p.ID()),
		version.Version(versionBeforeSnapshotEvent),
	)
```

Getting the version before a snapshot event is up to you, currently there's no transactionally safe way to `Save(...)` the event in the repository and delete all events up to that version.

You can periodically scan the backing store (e.g. a `SELECT` statement in postgres) based on a naming convention for example to look for the version.

The following event logs implement the `DeleterLog` interface: postgres, sqlite, memory
```go
type DeleterLog interface {
	DangerouslyDeleteEventsUpTo(
		ctx context.Context,
		id LogID,
		version version.Version,
	) error
}
```

## Further reading

- [Event archival](event-archival.md)
