# The `event.Reader` interface

The `Reader` is responsible for fetching an aggregate's event history. Its `ReadEvents` method takes an aggregate's `LogID` and a `version.Selector` and returns an `event.Records` iterator. 

An sql implementation is usually a db query.

```go
// A SQL-based implementation might look like this:
SELECT version, event_name, data
FROM my_events_table
WHERE log_id = ? AND version >= ?
ORDER BY version ASC;
```

`ReadEvents` returns an iterator, which plays really well with how `database/sql` expects you to read rows:
```go
for rows.Next() {
	// ...
	if err := rows.Scan(...) {
		// ...
	}
	if !yield(record, nil) {
		// ...
	}
}
```
