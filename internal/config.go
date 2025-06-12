package internal

import "encoding/json"

type ChronicleConfig struct {
	Unmarshal func(data []byte, v any) error
	Marshal   func(v any) ([]byte, error)
}

var Config = ChronicleConfig{
	Marshal:   json.Marshal,
	Unmarshal: json.Unmarshal,
}
