package event

import "github.com/DeluxeOwl/chronicle/internal"

type Unmarshaler interface {
	UnmarshalEvent(data RawData) error
}

type Marshaler interface {
	MarshalEvent() (RawData, error)
}

func Unmarshal(data RawData, v Any) error {
	if customUnmarshal, ok := v.(Unmarshaler); ok {
		return customUnmarshal.UnmarshalEvent(data)
	}

	return internal.Config.Unmarshal(data, v)
}

func Marshal(v Any) (RawData, error) {
	if customMarshal, ok := v.(Marshaler); ok {
		return customMarshal.MarshalEvent()
	}

	return internal.Config.Marshal(v)
}
