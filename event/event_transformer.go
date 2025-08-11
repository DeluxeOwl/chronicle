package event

import (
	"context"
	"fmt"
)

type Transformer[E Any] interface {
	TransformForWrite(ctx context.Context, event E) (E, error)
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
