package event

import "github.com/DeluxeOwl/chronicle/internal"

type Unmarshaler interface {
	UnmarshalEvent(data []byte) error
}

type Marshaler interface {
	MarshalEvent() ([]byte, error)
}

func Unmarshal(data []byte, v Any) error {
	if customUnmarshal, ok := v.(Unmarshaler); ok {
		return customUnmarshal.UnmarshalEvent(data)
	}

	return internal.Config.Unmarshal(data, v)
}

func Marshal(v Any) ([]byte, error) {
	if customMarshal, ok := v.(Marshaler); ok {
		return customMarshal.MarshalEvent()
	}

	return internal.Config.Marshal(v)
}
