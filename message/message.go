package message

import "maps"

type Message interface {
	Name() string
}

type Envelope[T Message] struct {
	Message  T
	Metadata Metadata
}

type GenericEnvelope Envelope[Message]

type Metadata map[string]string

func (m Metadata) With(key, value string) Metadata {
	if m == nil {
		m = make(Metadata)
	}

	m[key] = value

	return m
}

func (m Metadata) Merge(other Metadata) Metadata {
	if m == nil {
		return other
	}
	maps.Copy(m, other)
	return m
}
