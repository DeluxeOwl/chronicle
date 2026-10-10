package queue

import (
	"errors"
	"fmt"
	"reflect"

	"github.com/DeluxeOwl/chronicle/encoding"
)

func payloadFactoryFor[T any](config options) (func() T, error) {
	if config.payloadFactory == nil {
		if _, protobuf := config.codec.(*encoding.Protobuf); protobuf {
			return nil, errors.New("protobuf queue payload requires WithPayloadFactory")
		}
		//nolint:nilnil // No factory uses the codec's ordinary *T decoding contract.
		return nil, nil
	}
	factory, ok := config.payloadFactory.(func() T)
	if !ok || factory == nil {
		return nil, errors.New("queue payload factory must return the queue payload type")
	}
	if reflect.TypeFor[T]().Kind() != reflect.Pointer {
		return nil, errors.New("queue payload factory requires a pointer payload type")
	}
	return factory, nil
}

func decodePayload[T any](codec encoding.Codec, factory func() T, data []byte) (T, error) {
	var payload T
	target := any(&payload)
	if factory != nil {
		payload = factory()
		if reflect.ValueOf(payload).IsNil() {
			return payload, errors.New("queue payload factory returned nil")
		}
		// Factory-created values are already pointers, including descriptor-aware
		// Protobuf messages. Pass that pointer directly rather than **Message.
		target = payload
	}
	if err := codec.Decode(data, target); err != nil {
		var zero T
		return zero, fmt.Errorf("decode payload: %w", err)
	}
	return payload, nil
}
