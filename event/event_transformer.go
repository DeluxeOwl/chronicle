package event

import (
	"context"
	"fmt"
)

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
//	func (t *CryptoTransformer) TransformForWrite(ctx context.Context, event *MyEvent) (*MyEvent, error) {
//		encryptedData, err := encrypt(event.SensitiveData)
//		if err != nil {
//			return nil, err
//		}
//		event.SensitiveData = encryptedData
//		return event, nil
//	}
//
//	func (t *CryptoTransformer) TransformForRead(ctx context.Context, event *MyEvent) (*MyEvent, error) {
//		decryptedData, err := decrypt(event.SensitiveData)
//		if err != nil {
//			return nil, err
//		}
//		event.SensitiveData = decryptedData
//		return event, nil
//	}
//
//	// Then, pass it when creating the repository:
//	// repo, _ := chronicle.NewEventSourcedRepository(log, newAgg, []Transformer{&CryptoTransformer{...}})
type Transformer[E Any] interface {
	// TransformForWrite is called BEFORE an event is serialized and saved to the event log.
	// It receives a concrete event type and must return an event of the same type.
	// Use this to encrypt, compress, or otherwise modify the event for storage.
	//
	// Returns the transformed event and an error if the transformation fails.
	TransformForWrite(ctx context.Context, event E) (E, error)

	// TransformForRead is called AFTER an event is loaded and deserialized from the event log.
	// It should perform the inverse operation of TransformForWrite (e.g., decrypt, decompress).
	//
	// Returns the transformed event and an error if the transformation fails.
	TransformForRead(ctx context.Context, event E) (E, error)
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
// necessary type assertions to convert `E` to `event.Any` and back.
type anyTransformerAdapter[E Any] struct {
	internal AnyTransformer
}

// TransformForWrite delegates the transformation to the wrapped `AnyTransformer` and
// ensures the returned event is of the expected concrete type `E`.
//
// Returns the transformed event cast to type `E`, or an error if the transformation
// or type assertion fails.
func (a *anyTransformerAdapter[E]) TransformForWrite(ctx context.Context, event E) (E, error) {
	transformedAny, err := a.internal.TransformForWrite(ctx, event)
	if err != nil {
		var empty E
		return empty, err
	}

	finalEvent, ok := transformedAny.(E)
	if !ok {
		var empty E
		return empty, fmt.Errorf(
			"transform for write: transformer returned type %T which is not assignable to expected type %T",
			transformedAny,
			empty,
		)
	}

	return finalEvent, nil
}

// TransformForRead delegates the transformation to the wrapped `AnyTransformer` and
// ensures the returned event is of the expected concrete type `E`.
//
// Returns the transformed event cast to type `E`, or an error if the transformation
// or type assertion fails.
func (a *anyTransformerAdapter[E]) TransformForRead(ctx context.Context, event E) (E, error) {
	transformedAny, err := a.internal.TransformForRead(ctx, event)
	if err != nil {
		var empty E
		return empty, err
	}

	finalEvent, ok := transformedAny.(E)
	if !ok {
		var empty E
		return empty, fmt.Errorf("transform for read: incompatible type %T", transformedAny)
	}

	return finalEvent, nil
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
func (c TransformerChain[E]) TransformForWrite(ctx context.Context, event E) (E, error) {
	var err error
	currentEvent := event

	for _, transformer := range c.transformers {
		currentEvent, err = transformer.TransformForWrite(ctx, currentEvent)
		if err != nil {
			var empty E
			return empty, err
		}
	}

	return currentEvent, nil
}

// TransformForRead applies each transformer in the chain in reverse order.
// This ensures that write-time transformations are correctly undone (e.g., decompress then decrypt).
func (c TransformerChain[E]) TransformForRead(ctx context.Context, event E) (E, error) {
	var err error
	currentEvent := event

	for i := len(c.transformers) - 1; i >= 0; i-- {
		transformer := c.transformers[i]
		currentEvent, err = transformer.TransformForRead(ctx, currentEvent)
		if err != nil {
			var empty E
			return empty, err
		}
	}
	return currentEvent, nil
}
