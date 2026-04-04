# Implementing a custom `event.Log`

This framework is designed to be storage-agnostic. You can create your own to work with any database or storage system by implementing some interfaces from the `event` package.  

I recommend taking a look at the [./eventlog/postgres.go](../eventlog/postgres.go) and [./eventlog/sqlite.go](../eventlog/sqlite.go) implementations as a reference.

The core of any event log is the `event.Log` interface, which is a combination of two smaller interfaces: `event.Reader` and `event.Appender`.

```go
package event

type Log interface {
	Reader
	Appender
}
```

## Further reading

- [The `event.Reader` interface](event-reader.md)
- [The `event.Appender` interface](event-appender.md)
- [Global Event Log (the `event.GlobalReader` interface)](global-event-log.md)
- [Transactional Event Log](transactional-event-log.md)
