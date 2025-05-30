package serde

import (
	"encoding/json"

	"github.com/DeluxeOwl/zerrors"
)

type JSONError string

const (
	ErrJSONSerialize   JSONError = "serialize_data"
	ErrJSONDeserialize JSONError = "deserialize_data"
)

func NewJSONSerializer[T any]() SerializerFunc[T, []byte] {
	return func(t T) ([]byte, error) {
		data, err := json.Marshal(t)
		if err != nil {
			return nil, zerrors.New(ErrJSONSerialize).WithError(err)
		}

		return data, nil
	}
}

func NewJSONDeserializer[T any](factory func() T) DeserializerFunc[T, []byte] {
	return func(data []byte) (T, error) {
		var zeroValue T

		model := factory()
		if err := json.Unmarshal(data, &model); err != nil {
			return zeroValue, zerrors.New(ErrJSONDeserialize).WithError(err)
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
