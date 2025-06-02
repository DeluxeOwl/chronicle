package person

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/DeluxeOwl/eventuallynow/event"
	"github.com/DeluxeOwl/eventuallynow/serde"
	"github.com/DeluxeOwl/zerrors"
)

var _ event.GenericEvent = new(PersonEvent)

type PersonEvent struct {
	ID         PersonID    `json:"id"`
	RecordTime time.Time   `json:"recordTime"`
	Kind       personEvent `json:"kind" exhaustruct:"optional"`
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
	EventName string            `json:"eventName"`
	Metadata  map[string]string `json:"metadata,omitempty"`
}

type personEventRawSnapshot struct {
	ID         PersonID          `json:"id"`
	RecordTime time.Time         `json:"recordTime"`
	Kind       json.RawMessage   `json:"kind"`
	EventName  string            `json:"eventName"`
	Metadata   map[string]string `json:"metadata,omitempty"`
}

type PersonEventSerde struct{}

var _ serde.Bytes[event.Event] = PersonEventSerde{}

func (p PersonEventSerde) Deserialize(dst []byte) (event.Event, error) {
	var rawSnap personEventRawSnapshot
	if err := json.Unmarshal(dst, &rawSnap); err != nil {
		return event.Event{}, fmt.Errorf("unmarshal event data into raw snapshot: %w", err)
	}

	deserializedCoreEvent := &PersonEvent{
		ID:         rawSnap.ID,
		RecordTime: rawSnap.RecordTime,
	}

	switch rawSnap.EventName {
	case (*WasBorn)(nil).EventName():
		var kind WasBorn
		if err := json.Unmarshal(rawSnap.Kind, &kind); err != nil {
			return event.Event{}, fmt.Errorf("unmarshal WasBorn kind: %w (json: %s)", err, string(rawSnap.Kind))
		}
		deserializedCoreEvent.Kind = &kind
	case (*AgedOneYear)(nil).EventName():
		var kind AgedOneYear
		if err := json.Unmarshal(rawSnap.Kind, &kind); err != nil {
			return event.Event{}, fmt.Errorf("unmarshal AgedOneYear kind: %w (json: %s)", err, string(rawSnap.Kind))
		}
		deserializedCoreEvent.Kind = &kind
	default:
		return event.Event{}, fmt.Errorf("unknown event name '%s' found in event data", rawSnap.EventName)
	}

	return event.New(deserializedCoreEvent, event.WithMetadata(rawSnap.Metadata)), nil
}

func (p PersonEventSerde) Serialize(src event.Event) ([]byte, error) {
	genericEvt := src.Unwrap()
	personEvent, ok := genericEvt.(*PersonEvent)
	if !ok {
		return nil, zerrors.New(ErrUnexpectedEventType).Errorf("expected *PersonEvent, got type: %T", genericEvt)
	}

	snap := &PersonEventSnapshot{
		PersonEvent: *personEvent,
		EventName:   personEvent.EventName(),
		Metadata:    src.GetMeta(),
	}

	b, err := json.Marshal(snap)
	if err != nil {
		return nil, fmt.Errorf("marshal PersonEventSnapshot: %w", err)
	}

	return b, nil
}
