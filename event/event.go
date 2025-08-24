package event

// Any is the fundamental interface that all event types in the system must implement.
// It ensures that every event has a distinct name, which is used for serialization,
// deserialization, and routing within the framework.
//
// Usage:
//
//	type moneyDeposited struct {
//	    Amount int `json:"amount"`
//	}
//
//	// This method satisfies the event.Any interface.
//	func (*moneyDeposited) EventName() string { return "account/money_deposited" }
type Any interface {
	EventName() string
}

// AnyToConcrete is a generic helper function that safely performs a type assertion
// from the abstract event.Any interface to a specific, concrete event type `E`.
// This is typically used after deserializing an event from the event store.
//
// Usage:
//
//	var anyEvent event.Any = &moneyDeposited{Amount: 100}
//	if depositedEvent, ok := event.AnyToConcrete[*moneyDeposited](anyEvent); ok {
//	    fmt.Printf("Deposited: %d\n", depositedEvent.Amount)
//	}
//
// Returns the concrete event of type `E` and `true` if the assertion is successful.
// If the assertion fails, it returns the zero value for `E` and `false`.
func AnyToConcrete[E Any](event Any) (E, bool) {
	concrete, ok := event.(E)
	if !ok {
		var empty E
		return empty, false
	}

	return concrete, true
}
