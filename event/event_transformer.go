package event

import (
	"context"
	"fmt"
)

// TODO: update docs to reflect slice change, add upcasting example + tests + efficiency
// Transformer defines a contract for transforming events before they are written to
// the event log and after they are read from it. This is a useful mechanism for
// implementing cross-cutting concerns like encryption, compression, or event data up-casting.
//
// Transformers are applied in the order they are provided for writing, and in reverse
// order for reading.
//
// Usage:
//
//	// A simple transformer that encrypts event data.
//	type CryptoTransformer struct {
//		// dependencies like an encryption service
//	}
//
//	func (t *CryptoTransformer) TransformForWrite(ctx context.Context, events []*MyEvent) ([]*MyEvent, error) {
//		for _, event := range events {
//			encryptedData, err := encrypt(event.SensitiveData)
//			if err != nil {
//				return nil, err
//			}
//			event.SensitiveData = encryptedData
//		}
//		return events, nil
//	}
//
//	func (t *CryptoTransformer) TransformForRead(ctx context.Context, events []*MyEvent) ([]*MyEvent, error) {
//		for _, event := range events {
//			decryptedData, err := decrypt(event.SensitiveData)
//			if err != nil {
//				return nil, err
//			}
//			event.SensitiveData = decryptedData
//		}
//		return events, nil
//	}
//
//	// Then, pass it when creating the repository:
//	// repo, _ := chronicle.NewEventSourcedRepository(log, newAgg, []Transformer{&CryptoTransformer{...}})
type Transformer[E Any] interface {
	// TransformForWrite is called BEFORE events are serialized and saved to the event log.
	// It receives a concrete event types and must returns events of the same type.
	// Use this to encrypt, compress, or otherwise modify the events for storage.
	//
	// Returns the transformed events and an error if the transformation fails.
	TransformForWrite(ctx context.Context, event []E) ([]E, error)

	// TransformForRead is called AFTER events are loaded and deserialized from the event log.
	// It should perform the inverse operation of TransformForWrite (e.g., decrypt, decompress).
	//
	// Returns the transformed events and an error if the transformation fails.
	TransformForRead(ctx context.Context, event []E) ([]E, error)
}

// AnyTransformer is an alias for a Transformer that operates on the generic `event.Any`
// interface. This is useful for creating transformers that are not tied to a specific
// aggregate's event type, such as a global encryption or logging transformer.
type AnyTransformer Transformer[Any]

// AnyTransformerToTyped adapts a non-specific `AnyTransformer` to a strongly-typed
// `Transformer[E]`. This is a helper function used internally by the framework
// to allow a single generic transformer to be used with multiple repositories that
// have different event types.
//
// Returns a type-safe `Transformer[E]` that wraps the generic `AnyTransformer`.
func AnyTransformerToTyped[E Any](anyTransformer AnyTransformer) Transformer[E] {
	return &anyTransformerAdapter[E]{
		internal: anyTransformer,
	}
}

// anyTransformerAdapter is the internal implementation that wraps an `AnyTransformer`
// to make it conform to the strongly-typed `Transformer[E]` interface. It handles the
// necessary type assertions to convert `[]E` to `[]event.Any` and back.
type anyTransformerAdapter[E Any] struct {
	internal AnyTransformer
}

// TransformForWrite delegates the transformation to the wrapped `AnyTransformer` and
// ensures the returned events are of the expected concrete type `[]E`.
//
// Returns the transformed event cast to type `[]E`, or an error if the transformation
// or type assertion fails.
func (a *anyTransformerAdapter[E]) TransformForWrite(ctx context.Context, events []E) ([]E, error) {
	anyEvents := convertSliceToAny(events)

	transformedAnys, err := a.internal.TransformForWrite(ctx, anyEvents)
	if err != nil {
		return nil, err
	}

	return convertSliceFromAny[E](transformedAnys)
}

// TransformForRead delegates the transformation to the wrapped `AnyTransformer` and
// ensures the returned events are of the expected concrete type `[]E`.
//
// Returns the transformed events cast to type `[]E`, or an error if the transformation
// or type assertion fails.
func (a *anyTransformerAdapter[E]) TransformForRead(ctx context.Context, events []E) ([]E, error) {
	anyEvents := convertSliceToAny(events)

	transformedAnys, err := a.internal.TransformForRead(ctx, anyEvents)
	if err != nil {
		return nil, err
	}

	return convertSliceFromAny[E](transformedAnys)
}

func convertSliceFromAny[E Any](anyEvents []Any) ([]E, error) {
	events := make([]E, len(anyEvents))
	for i, anyEvent := range anyEvents {
		typedEvent, ok := anyEvent.(E)
		if !ok {
			var empty E
			return nil, fmt.Errorf(
				"transformer returned incompatible type %T at index %d, expected type assignable to %T",
				anyEvent,
				i,
				empty,
			)
		}
		events[i] = typedEvent
	}
	return events, nil
}

func convertSliceToAny[E Any](events []E) []Any {
	anyEvents := make([]Any, len(events))
	for i, e := range events {
		anyEvents[i] = e
	}
	return anyEvents
}

// TransformerChain is a composite Transformer that applies a sequence of transformers.
// It implements the Transformer interface, allowing multiple transformers to be treated
// as a single one.
//
// On write, it applies transformers in the order they are provided.
// On read, it applies them in the reverse order to correctly unwind the transformations.
type TransformerChain[E Any] struct {
	transformers []Transformer[E]
}

// NewTransformerChain creates a new TransformerChain from the given transformers.
func NewTransformerChain[E Any](transformers ...Transformer[E]) TransformerChain[E] {
	return TransformerChain[E]{
		transformers: transformers,
	}
}

// TransformForWrite applies each transformer in the chain sequentially.
// The output of one transformer becomes the input for the next.
func (c TransformerChain[E]) TransformForWrite(ctx context.Context, events []E) ([]E, error) {
	var err error
	currentEvents := events

	for _, transformer := range c.transformers {
		currentEvents, err = transformer.TransformForWrite(ctx, currentEvents)
		if err != nil {
			return nil, err
		}
	}

	return currentEvents, nil
}

// TransformForRead applies each transformer in the chain in reverse order.
// This ensures that write-time transformations are correctly undone (e.g., decompress then decrypt).
func (c TransformerChain[E]) TransformForRead(ctx context.Context, events []E) ([]E, error) {
	var err error
	currentEvents := events

	for i := len(c.transformers) - 1; i >= 0; i-- {
		transformer := c.transformers[i]
		currentEvents, err = transformer.TransformForRead(ctx, currentEvents)
		if err != nil {
			return nil, err
		}
	}

	return currentEvents, nil
}
