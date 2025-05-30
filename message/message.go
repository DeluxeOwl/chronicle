package message

type Message interface {
	Name() string
}

type Envelope[T Message] struct {
	Message  T
	Metadata map[string]string
}

type GenericEnvelope Envelope[Message]
