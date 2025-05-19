package serde

type Serializer[S, D any] interface {
	Serialize(src S) (D, error)
}

type SerializerFunc[S, D any] func(src S) (D, error)

func (fn SerializerFunc[S, D]) Serialize(src S) (D, error) {
	return fn(src)
}

type Deserializer[S, D any] interface {
	Deserialize(dst D) (S, error)
}

type DeserializerFunc[S, D any] func(dst D) (S, error)

func (fn DeserializerFunc[S, D]) Deserialize(dst D) (S, error) {
	return fn(dst)
}

type Serde[S, D any] interface {
	Serializer[S, D]
	Deserializer[S, D]
}

type Fused[S, D any] struct {
	Serializer[S, D]
	Deserializer[S, D]
}

func Fuse[S, D any](serializer Serializer[S, D], deserializer Deserializer[S, D]) Fused[S, D] {
	return Fused[S, D]{
		Serializer:   serializer,
		Deserializer: deserializer,
	}
}
