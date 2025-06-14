package event

import (
	"encoding/json"
)

type Serializer interface {
	MarshalEvent(v Any) (RawData, error)
	UnmarshalEvent(data RawData, v Any) error
}

type jsonSerializer struct{}

func NewJSONSerializer() *jsonSerializer {
	return &jsonSerializer{}
}

func (j *jsonSerializer) MarshalEvent(v Any) (RawData, error) {
	return json.Marshal(v)
}

func (j *jsonSerializer) UnmarshalEvent(data RawData, v Any) error {
	return json.Unmarshal(data, v)
}

type genericSerializer struct {
	marshal   func(v Any) (RawData, error)
	unmarshal func(data RawData, v Any) error
}

func NewGenericSerializer(
	marshal func(v Any) (RawData, error),
	unmarshal func(data RawData, v Any) error,
) *genericSerializer {
	return &genericSerializer{
		marshal:   marshal,
		unmarshal: unmarshal,
	}
}

func (gs *genericSerializer) MarshalEvent(v Any) (RawData, error) {
	return gs.marshal(v)
}

func (gs *genericSerializer) UnmarshalEvent(data RawData, v Any) error {
	return gs.unmarshal(data, v)
}
