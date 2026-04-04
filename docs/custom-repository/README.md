# Implementing a custom `aggregate.Repository`

While `chronicle` provides ready to use repository implementations (`NewEventSourcedRepository`, `NewTransactionalRepository`, etc.), you might encounter scenarios where you need to build your own. 

This could be to integrate with a different kind of storage, add custom caching, or implement specific transactional behavior not covered by the defaults.

`chronicle` provides a set of reusable helper functions that handle the most complex parts of the event sourcing workflow. Implementing a repository is often just a matter of wiring these components together.

First, you'll define your repository struct. It needs dependencies to function: an `event.Log` for storage, a factory function `createRoot` to instantiate your aggregate, an `event.Registry` to map event names to types, and a `codec.Codec` for encoding and decoding.

> [!WARNING] 
> By default, all repositories use a shared global registry. To override this, you need to pass the registry manually.

```go
import (
	"github.com/DeluxeOwl/chronicle/aggregate"
	"github.com/DeluxeOwl/chronicle/event"
	"github.com/DeluxeOwl/chronicle/codec"
	// ...
)

type CustomRepository[TID ID, E event.Any, R Root[TID, E]] struct {
	eventlog     event.Log
	createRoot   func() R
	registry     event.Registry[E]
	encoder      codec.Codec

	// Optional: for encrypting, compressing, or upcasting events
	transformers []event.Transformer[E]
}
```

Next, you need a constructor. An optional step here is to **register the aggregate's events**. The repository must know how to decode event data from the log back into concrete Go types. 

You can also tell your users to ensure that events are registered beforehand, but that is error-prone.

This is done by calling `registry.RegisterEvents()`, which populates the registry using the `EventFuncs()` method on your aggregate.

```go
func NewCustomRepository[...](
	eventLog event.Log,
	createRoot func() R,
	// ...
) (*CustomRepository[...], error) {
	repo := &CustomRepository[...]{
		eventlog:   eventLog,
		createRoot: createRoot,
		registry:   event.NewConcreteRegistryFromAny[E](event.GlobalRegistry),
		encoder:    codec.NewJSONB(), // Default to JSON, you can also change this.
	}

	err := repo.registry.RegisterEvents(createRoot())
	if err != nil {
		return nil, fmt.Errorf("new custom repository: %w", err)
	}

	return repo, nil
}
```

To implement the `Get` method for loading an aggregate, you can use the `aggregate.ReadAndLoadFromStore` helper. This function orchestrates the entire loading process: it queries the `event.Log`, then uses the registry and the encoder to decode and apply each event to a fresh aggregate instance.

```go
func (r *CustomRepository[...]) Get(ctx context.Context, id TID) (R, error) {
	root := r.createRoot() // Create a new, empty aggregate instance.

	// This helper does all the heavy lifting of loading.
	err := aggregate.ReadAndLoadFromStore(
		ctx,
		root,
		r.eventlog,
		r.registry,
		r.encoder,
		r.transformers,
		id,
		version.SelectFromBeginning, // Load all events
	)
	if err != nil {
		var empty R // return zero value for the root
		return empty, fmt.Errorf("custom repo get: %w", err)
	}

	return root, nil
}
```

If you need more fine grained control over the loading process, you can look at the implementation of `ReadAndLoadFromStore`, which in turn uses `aggregate.LoadFromRecords`. 

This lower level helper takes an iterator of `event.Record` instances and handles the core loop of decoding event data, applying read-side transformers, and calling the aggregate's `Apply` method for each event.

For the `Save` method, the framework provides the `aggregate.CommitEvents` helper. It handles flushing uncommitted events from the aggregate, applying write-side transformers, encoding the events, and appending them to the event log with the correct optimistic concurrency check.

```go
func (r *CustomRepository[...]) Save(
	ctx context.Context,
	root R,
) (version.Version, aggregate.CommittedEvents[E], error) {
	
	// This helper handles flushing, transforming, encoding, and committing.
	newVersion, committedEvents, err := aggregate.CommitEvents(
		ctx,
		r.eventlog,
		r.encoder,
		r.transformers,
		root,
	)
	if err != nil {
		return version.Zero, nil, fmt.Errorf("custom repo save: %w", err)
	}

	return newVersion, committedEvents, nil
}
```

## Further reading

- [Using an `aggregate.FusedRepo`](fused-repo.md)
