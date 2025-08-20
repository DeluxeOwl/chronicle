package event

import (
	"context"
	"fmt"
)

type Transformer[E Any] interface {
	// TransformForWrite is called BEFORE an event is serialized and saved to the event log.
	// Use this to encrypt, compress, or otherwise modify the event for storage.
	TransformForWrite(ctx context.Context, event E) (E, error)

	// TransformForRead is called AFTER an event is loaded and deserialized from the event log.
	// This should be the inverse operation of TransformForWrite (e.g., decrypt, decompress).
	TransformForRead(ctx context.Context, event E) (E, error)
}

type AnyTransformer Transformer[Any]

func AnyTransformerToTyped[E Any](anyTransformer AnyTransformer) Transformer[E] {
	return &anyTransformerAdapter[E]{
		internal: anyTransformer,
	}
}

type anyTransformerAdapter[E Any] struct {
	internal AnyTransformer
}

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
