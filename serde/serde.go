package serde

import "encoding/json"

type BinarySerializer interface {
	SerializeBinary(v any) ([]byte, error)
}

type BinaryDeserializer interface {
	DeserializeBinary(data []byte) (any, error)
}

type BinarySerde interface {
	SerializeBinary(v any) ([]byte, error)
	DeserializeBinary(data []byte) (any, error)
}

type (
	SerializeBinaryFunc   = func(v any) ([]byte, error)
	DeserializeBinaryFunc = func(data []byte) (any, error)
)

type GenericBinary struct {
	serializeBinary   SerializeBinaryFunc
	deserializeBinary DeserializeBinaryFunc
}

func NewGenericBinary(serialize SerializeBinaryFunc, deserialize DeserializeBinaryFunc) *GenericBinary {
	return &GenericBinary{
		serializeBinary:   serialize,
		deserializeBinary: deserialize,
	}
}

func (gb *GenericBinary) SerializeBinary(v any) ([]byte, error) {
	return gb.serializeBinary(v)
}

func (gb *GenericBinary) DeserializeBinary(data []byte) (any, error) {
	return gb.deserializeBinary(data)
}

type JSONBinary struct{}

func NewJSONBinary() *JSONBinary {
	return &JSONBinary{}
}

func (jb *JSONBinary) SerializeBinary(v any) ([]byte, error) {
	return json.Marshal(v)
}

func (jb *JSONBinary) DeserializeBinary(data []byte) (any, error) {
	var v any
	err := json.Unmarshal(data, v)
	return v, err
}
