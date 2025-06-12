package encoding

import (
	"encoding/json"

	"github.com/DeluxeOwl/chronicle/event"
)

// Custom marshaler and unmarshaler.
type Unmarshaler interface {
	UnmarshalEvent(data []byte) error
}

type Marshaler interface {
	MarshalEvent() ([]byte, error)
}

func Unmarshal(data []byte, v event.Any) error {
	if customUnmarshal, ok := v.(Unmarshaler); ok {
		return customUnmarshal.UnmarshalEvent(data)
	}

	return json.Unmarshal(data, v)
}

func Marshal(v event.Any) ([]byte, error) {
	if customMarshal, ok := v.(Marshaler); ok {
		return customMarshal.MarshalEvent()
	}

	return json.Marshal(v)
}
