package serde

import (
	"github.com/DeluxeOwl/zerrors"
)

type SerdeError string

const (
	ErrFirstStageSerializer  SerdeError = "first_stage_serializer"
	ErrSecondStageSerializer SerdeError = "second_stage_serializer"

	ErrFirstStageDeserializer  SerdeError = "first_stage_deserializer"
	ErrSecondStageDeserializer SerdeError = "second_stage_deserializer"
)

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

type Chained[S, M, D any] struct {
	first  Serde[S, M]
	second Serde[M, D]
}

// Serialize implements the serde.Serializer interface.
func (s Chained[Src, Mid, Dst]) Serialize(src Src) (Dst, error) {
	var zeroValue Dst

	mid, err := s.first.Serialize(src)
	if err != nil {
		return zeroValue, zerrors.New(ErrFirstStageSerializer).WithError(err)
	}

	dst, err := s.second.Serialize(mid)
	if err != nil {
		return zeroValue, zerrors.New(ErrSecondStageDeserializer).WithError(err)
	}

	return dst, nil
}

// Deserialize implements the serde.Deserializer interface.
func (s Chained[Src, Mid, Dst]) Deserialize(dst Dst) (Src, error) {
	var zeroValue Src

	mid, err := s.second.Deserialize(dst)
	if err != nil {
		return zeroValue, zerrors.New(ErrFirstStageDeserializer).WithError(err)
	}

	src, err := s.first.Deserialize(mid)
	if err != nil {
		return zeroValue, zerrors.New(ErrSecondStageDeserializer).WithError(err)
	}

	return src, nil
}

// Chain chains together two serdes to build a new serde instance to map from Src to Dst types.
func Chain[S, M, D any](first Serde[S, M], second Serde[M, D]) Chained[S, M, D] {
	return Chained[S, M, D]{
		first:  first,
		second: second,
	}
}
