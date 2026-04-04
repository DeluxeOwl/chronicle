# Global Event Log (the `event.GlobalReader` interface)

For building system-wide projections, you can implement the `event.GlobalLog` interface, which adds `GlobalReader`.

```go
type GlobalReader interface {
	ReadAllEvents(ctx context.Context, globalSelector version.Selector) GlobalRecords
}
```

To support this, your backing store needs to be able to assign a globally monotonic id (like `global_version BIGINT GENERATED ALWAYS AS IDENTITY` in postgres). 

This is cumbersome and not recommended for stores like kafka which don't have a globally ordered log. This is a tradeoff you must be aware of.

You save this global ID along with the other event data. The `ReadAllEvents` implementation can then simply query the table ordered by this key. 

The returned `GlobalRecords` is the same as normal `Records` but each event has an additional `func (gr *GlobalRecord) GlobalVersion() version.Version` method.
