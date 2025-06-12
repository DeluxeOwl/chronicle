package chronicle

import "encoding/json"

type ChronicleConfig struct {
	Unmarshaler func(data []byte, v any) error
	Marshaler   func(v any) ([]byte, error)
}

var Config = ChronicleConfig{
	Marshaler:   json.Marshal,
	Unmarshaler: json.Unmarshal,
}
