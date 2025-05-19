package event

import "maps"

type Payload interface {
	Name() string
}

type Event[T Payload] struct {
	Payload  T
	Metadata Metadata
}

type GenericEvent Event[Payload]

func (e Event[T]) ToGenericEvent() GenericEvent {
	return GenericEvent{
		Payload:  e.Payload,
		Metadata: e.Metadata,
	}
}

type Metadata map[string]string

func (m Metadata) With(key, value string) Metadata {
	if m == nil {
		m = make(Metadata)
	}

	m[key] = value

	return m
}

// Merge merges the other Metadata provided in input with the current map.
// Returns a pointer to the extended metadata map.
func (m Metadata) Merge(other Metadata) Metadata {
	if m == nil {
		return other
	}
	maps.Copy(m, other)
	return m
}
