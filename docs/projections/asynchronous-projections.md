# Asynchronous Projections (`event.AsyncProjection`)

An `AsyncProjection` is the primitive for building eventually-consistent read models. It works by processing events from the global, system-wide event stream *after* they have been successfully committed. This is the most common type of projection, ideal for analytics, reporting, search indexes, or notifying external systems where a slight delay is acceptable.

It operates on generic `event.GlobalRecord`s and is designed for resilience.

```go
// AsyncProjection processes events asynchronously from the global event stream.
// It processes one event at a time with explicit checkpoint management for
// resumability and fault tolerance.
//
// Use cases:
//   - Eventually consistent projections
//   - Cross-service integration
//   - Analytics and reporting
//   - Email notifications, etc.
type AsyncProjection interface {
	MatchesEvent(eventName string) bool
	// Handle processes a single event from the global stream.
	// Checkpoint is saved based on the configured CheckpointPolicy.
	Handle(ctx context.Context, rec *GlobalRecord) error
}
```

-   `MatchesEvent(eventName string) bool`: Filters the global event stream, allowing you to process only the events relevant to your projection.
-   `Handle(ctx context.Context, rec *GlobalRecord) error`: Called for each individual event that passes the filter. Your logic here is responsible for updating the read model.

An `AsyncProjection` is driven by the `event.AsyncProjectionRunner`. This runner is a configurable component that manages the entire lifecycle:
-   It polls or "tails" the global event log for new events (for event logs that support tailing)
-   It passes matching events to your projection's `Handle` method.
-   It manages checkpoints by saving the `globalVersion` of the last successfully processed event. This ensures that if the projection restarts, it can resume from exactly where it left off, guaranteeing no events are missed.
