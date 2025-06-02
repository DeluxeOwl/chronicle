package person

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/DeluxeOwl/eventuallynow/event"
	"github.com/DeluxeOwl/eventuallynow/serde"
)

var _ event.GenericEvent = new(PersonEvent)

type PersonEvent struct {
	ID         PersonID    `json:"id"`
	RecordTime time.Time   `json:"recordTime"`
	Kind       personEvent `json:"kind"       exhaustruct:"optional"`
}

func (p *PersonEvent) EventName() string { return p.Kind.EventName() }

//sumtype:decl
type personEvent interface {
	event.GenericEvent
	isPersonEvent()
}

type WasBorn struct {
	BornName string `json:"bornName"`
}

func (*WasBorn) EventName() string { return "person-was-born" }
func (*WasBorn) isPersonEvent()    {}

type AgedOneYear struct{}

func (*AgedOneYear) EventName() string { return "aged-one-year" }
func (*AgedOneYear) isPersonEvent()    {}

type PersonEventSnapshot struct {
	PersonEvent
	EventName string `json:"eventName"`
}

type personEventRawSnapshot struct {
	ID         PersonID        `json:"id"`
	RecordTime time.Time       `json:"recordTime"`
	Kind       json.RawMessage `json:"kind"`
	EventName  string          `json:"eventName"` // EventName of the Kind
}

func NewPersonPayloadSerde() serde.Serde[event.GenericEvent, []byte] {
	return serde.Fuse(
		serde.SerializerFunc[event.GenericEvent, []byte](serializePersonEventPayload),
		serde.DeserializerFunc[event.GenericEvent, []byte](deserializePersonEventPayload),
	)
}

func serializePersonEventPayload(ge event.GenericEvent) ([]byte, error) {
	personEvent, ok := ge.(*PersonEvent)
	if !ok {
		return nil, fmt.Errorf("person payload serializer: expected *PersonEvent, got %T", ge)
	}

	snap := &PersonEventSnapshot{
		PersonEvent: *personEvent,
		EventName:   personEvent.Kind.EventName(),
	}
	return json.Marshal(snap)
}

func deserializePersonEventPayload(data []byte) (event.GenericEvent, error) {
	var rawSnap personEventRawSnapshot
	if err := json.Unmarshal(data, &rawSnap); err != nil {
		return nil, fmt.Errorf("person payload deserializer: unmarshal raw snapshot: %w (data: %s)", err, string(data))
	}

	deserializedCoreEvent := &PersonEvent{
		ID:         rawSnap.ID,
		RecordTime: rawSnap.RecordTime,
	}

	switch rawSnap.EventName {
	case (*WasBorn)(nil).EventName():
		var kind WasBorn
		if err := json.Unmarshal(rawSnap.Kind, &kind); err != nil {
			return nil, fmt.Errorf("person payload deserializer: unmarshal WasBorn kind: %w (json: %s)", err, string(rawSnap.Kind))
		}
		deserializedCoreEvent.Kind = &kind
	case (*AgedOneYear)(nil).EventName():
		var kind AgedOneYear
		if err := json.Unmarshal(rawSnap.Kind, &kind); err != nil {
			return nil, fmt.Errorf("person payload deserializer: unmarshal AgedOneYear kind: %w (json: %s)", err, string(rawSnap.Kind))
		}
		deserializedCoreEvent.Kind = &kind
	default:
		return nil, fmt.Errorf("person payload deserializer: unknown event name '%s' in person event payload snapshot", rawSnap.EventName)
	}
	return deserializedCoreEvent, nil
}
