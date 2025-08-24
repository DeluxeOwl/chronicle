package serde

import "encoding/json"

// BinarySerializer is an interface for types that can convert a Go value
// (like an event struct) into a byte slice for storage or transmission.
//
// Usage:
//
//	var serializer BinarySerializer = NewJSONBinary()
//	data, err := serializer.SerializeBinary(myEvent)
//
// Returns a byte slice representing the serialized value and an error if serialization fails.
type BinarySerializer interface {
	SerializeBinary(v any) ([]byte, error)
}

// BinaryDeserializer is an interface for types that can convert a byte slice
// back into a Go value (like an event struct).
//
// Usage:
//
//	var deserializer BinaryDeserializer = NewJSONBinary()
//	eventBytes := []byte(`{"amount": 100}`)
//	var moneyDepositedEvent MoneyDeposited
//	err := deserializer.DeserializeBinary(eventBytes, &moneyDepositedEvent)
//
// Returns an error if deserialization fails. The target `v` must be a pointer.
type BinaryDeserializer interface {
	DeserializeBinary(data []byte, v any) error
}

// BinarySerde is a convenience interface that combines both serialization and
// deserialization capabilities. The name "Serde" is a common shorthand for
// Serializer/Deserializer.
type BinarySerde interface {
	BinarySerializer
	BinaryDeserializer
}

// SerializeBinaryFunc is a function type that matches the signature of the
// BinarySerializer.SerializeBinary method.
type SerializeBinaryFunc = func(v any) ([]byte, error)

// DeserializeBinaryFunc is a function type that matches the signature of the
// BinaryDeserializer.DeserializeBinary method.
type DeserializeBinaryFunc = func(data []byte, v any) error

var (
	_ BinarySerde = (*GenericBinary)(nil)
	_ BinarySerde = (*JSONBinary)(nil)
)

// GenericBinary provides a flexible, function-based implementation of the BinarySerde interface.
// It allows you to create a custom serde by simply providing the serialization and
// deserialization functions, without needing to define a new struct.
//
// Usage:
//
//	// Create a serde using the standard library's JSON functions.
//	customSerde := NewGenericBinary(json.Marshal, json.Unmarshal)
type GenericBinary struct {
	serializeBinary   SerializeBinaryFunc
	deserializeBinary DeserializeBinaryFunc
}

// NewGenericBinary creates a new GenericBinary serde from the provided
// serialize and deserialize functions.
func NewGenericBinary(
	serialize SerializeBinaryFunc,
	deserialize DeserializeBinaryFunc,
) *GenericBinary {
	return &GenericBinary{
		serializeBinary:   serialize,
		deserializeBinary: deserialize,
	}
}

// SerializeBinary calls the underlying serialize function.
func (gb *GenericBinary) SerializeBinary(v any) ([]byte, error) {
	return gb.serializeBinary(v)
}

// DeserializeBinary calls the underlying deserialize function.
func (gb *GenericBinary) DeserializeBinary(data []byte, v any) error {
	return gb.deserializeBinary(data, v)
}

// JSONBinary is a concrete implementation of BinarySerde that uses the standard
// library's `encoding/json` package. This is the default serializer used
// throughout the framework.
type JSONBinary struct{}

// NewJSONBinary creates a new JSONBinary serde.
func NewJSONBinary() *JSONBinary {
	return &JSONBinary{}
}

// SerializeBinary uses json.Marshal to serialize the value.
func (jb *JSONBinary) SerializeBinary(v any) ([]byte, error) {
	return json.Marshal(v)
}

// DeserializeBinary uses json.Unmarshal to deserialize the data into the value.
func (jb *JSONBinary) DeserializeBinary(data []byte, v any) error {
	return json.Unmarshal(data, v)
}
