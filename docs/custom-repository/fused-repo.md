# Using an `aggregate.FusedRepo`

What if you only need to change one part of the repository's behavior, like `Save`, while keeping the default loading logic? For this, the framework provides `aggregate.FusedRepo`. 

It is a convenience type that implements the `Repository` interface by composing its individual parts (`Getter`, `Saver`, `AggregateLoader`, etc.), making it perfect for creating decorators.

For example, you could create a custom `Saver` that adds logging, and then "fuse" it with a standard repository's loading capabilities:

```go
// A custom Saver that adds logging before saving.
type LoggingSaver[TID ID, E event.Any, R Root[TID, E]] struct {
    // The "real" saver
    saver aggregate.Saver[TID, E, R]
}

func (s *LoggingSaver[...]) Save(ctx context.Context, root R) (version.Version, aggregate.CommittedEvents[E], error) {
    log.Printf("Attempting to save aggregate %s", root.ID())
    return s.saver.Save(ctx, root)
}

baseRepo, _ := chronicle.NewEventSourcedRepository(...)

// Create a new repository that uses the standard Get but our custom Save.
loggingRepo := &aggregate.FusedRepo[...]{
    AggregateLoader: baseRepo,
    VersionedGetter: baseRepo,
    Getter:          baseRepo,
    Saver:           &LoggingSaver[...]{saver: baseRepo},
}
```
