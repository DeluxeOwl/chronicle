package serde

import "encoding/json"

type BinarySerializer interface {
	SerializeBinary(v any) ([]byte, error)
}

type BinaryDeserializer interface {
	DeserializeBinary(data []byte, v any) error
}

type BinarySerde interface {
	BinarySerializer
	BinaryDeserializer
}

type (
	SerializeBinaryFunc   = func(v any) ([]byte, error)
	DeserializeBinaryFunc = func(data []byte, v any) error
)

var (
	_ BinarySerde = (*GenericBinary)(nil)
	_ BinarySerde = (*JSONBinary)(nil)
)

type GenericBinary struct {
	serializeBinary   SerializeBinaryFunc
	deserializeBinary DeserializeBinaryFunc
}

func NewGenericBinary(
	serialize SerializeBinaryFunc,
	deserialize DeserializeBinaryFunc,
) *GenericBinary {
	return &GenericBinary{
		serializeBinary:   serialize,
		deserializeBinary: deserialize,
	}
}

func (gb *GenericBinary) SerializeBinary(v any) ([]byte, error) {
	return gb.serializeBinary(v)
}

func (gb *GenericBinary) DeserializeBinary(data []byte, v any) error {
	return gb.deserializeBinary(data, v)
}

type JSONBinary struct{}

func NewJSONBinary() *JSONBinary {
	return &JSONBinary{}
}

func (jb *JSONBinary) SerializeBinary(v any) ([]byte, error) {
	return json.Marshal(v)
}

func (jb *JSONBinary) DeserializeBinary(data []byte, v any) error {
	return json.Unmarshal(data, v)
}
