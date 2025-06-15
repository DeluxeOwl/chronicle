package aggregate

import (
	"encoding/json"
	"fmt"
)

type SnapshotSerializer interface {
	MarshalSnapshot(v any) ([]byte, error)
	UnmarshalSnapshot(data []byte, v any) error
}

type jsonSnapshotSerializer struct{}

func NewJSONSnapshotSerializer() SnapshotSerializer {
	return &jsonSnapshotSerializer{}
}

func (j *jsonSnapshotSerializer) MarshalSnapshot(v any) ([]byte, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("json marshal snapshot: %w", err)
	}
	return data, nil
}

func (j *jsonSnapshotSerializer) UnmarshalSnapshot(data []byte, v any) error {
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("json unmarshal snapshot: %w", err)
	}
	return nil
}

type genericSerializer struct {
	marshal   func(v any) ([]byte, error)
	unmarshal func(data []byte, v any) error
}

func NewGenericSerializer(
	marshal func(v any) ([]byte, error),
	unmarshal func(data []byte, v any) error,
) *genericSerializer {
	return &genericSerializer{
		marshal:   marshal,
		unmarshal: unmarshal,
	}
}

func (gs *genericSerializer) MarshalSnapshot(v any) ([]byte, error) {
	return gs.marshal(v)
}

func (gs *genericSerializer) UnmarshalSnapshot(data []byte, v any) error {
	return gs.unmarshal(data, v)
}
