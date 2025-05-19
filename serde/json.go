package serde

import (
	"encoding/json"
	"fmt"
)

func NewJSONSerializer[T any]() SerializerFunc[T, []byte] {
	return func(t T) ([]byte, error) {
		data, err := json.Marshal(t)
		if err != nil {
			return nil, fmt.Errorf("serde.JSON: failed to serialize data, %w", err)
		}

		return data, nil
	}
}

func NewJSONDeserializer[T any](factory func() T) DeserializerFunc[T, []byte] {
	return func(data []byte) (T, error) {
		var zeroValue T

		model := factory()
		if err := json.Unmarshal(data, &model); err != nil {
			return zeroValue, fmt.Errorf("serde.JSON: failed to deserialize data, %w", err)
		}

		return model, nil
	}
}

func NewJSON[T any](factory func() T) Fused[T, []byte] {
	return Fuse(
		NewJSONSerializer[T](),
		NewJSONDeserializer(factory),
	)
}
