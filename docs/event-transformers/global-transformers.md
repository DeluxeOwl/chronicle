# Global Transformers with `AnyTransformerToTyped`

While the `CryptoTransformer` is specific to `account.AccountEvent`, you might want to create a generic transformer that can operate on events from any aggregate. For example, a global logging mechanism.

This is where `event.AnyTransformer` is useful. It is a type alias for `event.Transformer[event.Any]`, allowing it to process any event in the system as long as it satisfies the base `event.Any` interface.

Let's create a simple transformer that logs every event being written to or read from the event log.
```go
// in examples/4_transformers/main.go

type LoggingTransformer struct{}

// This transformer works with any event type (`event.Any`).
func (t *LoggingTransformer) TransformForWrite(
	_ context.Context,
	events []event.Any,
) ([]event.Any, error) {
	for _, event := range events {
		fmt.Printf("[LOG] Writing event: %s\n", event.EventName())
	}

	return events, nil
}

func (t *LoggingTransformer) TransformForRead(
	_ context.Context,
	events []event.Any,
) ([]event.Any, error) {
	for _, event := range events {
		fmt.Printf("[LOG] Reading event: %s\n", event.EventName())
	}
	return events, nil
}
```

However, a repository for a specific aggregate, like our `accountRepo`, expects a `[]event.Transformer[account.AccountEvent]`, not a `[]event.Transformer[event.Any]`. A direct assignment will fail due to Go's type system.

`AnyTransformerToTyped` is a helper function that solves this. It's an adapter that takes your generic `AnyTransformer` and makes it compatible with a specific aggregate's repository.

Here is how you would use both our specific `CryptoTransformer` and our global `LoggingTransformer` for the account repository.

```go
	cryptoTransformer = account.NewCryptoTransformer(deletedKey)

	loggingTransformer := &LoggingTransformer{}

	forgottenRepo, _ := chronicle.NewEventSourcedRepository(
		memoryEventLog,
		account.NewEmpty,
		[]event.Transformer[account.AccountEvent]{
			cryptoTransformer,
			event.AnyTransformerToTyped[account.AccountEvent](loggingTransformer),
		},
	)
```
