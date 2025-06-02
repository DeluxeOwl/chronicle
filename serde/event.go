package serde

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/DeluxeOwl/eventuallynow/event"
)

type SerializedEventWrapper struct {
	EventName string            `json:"eventName"`
	Metadata  map[string]string `json:"metadata,omitempty"`
	Payload   json.RawMessage   `json:"payload"`
}

type GenericPayloadSerdeProvider interface {
	GetPayloadSerializer(ge event.GenericEvent) (Serializer[event.GenericEvent, []byte], error)
	GetPayloadDeserializer(eventName string) (Deserializer[event.GenericEvent, []byte], error)
}

type DefaultEventSerde struct {
	provider GenericPayloadSerdeProvider
}

func NewDefaultEventSerde(provider GenericPayloadSerdeProvider) Bytes[event.Event] {
	return &DefaultEventSerde{provider: provider}
}

func (s *DefaultEventSerde) Serialize(ev event.Event) ([]byte, error) {
	genericEv := ev.Unwrap()
	if genericEv == nil {
		return nil, errors.New("cannot serialize nil generic event")
	}
	eventName := genericEv.EventName()

	payloadSerializer, err := s.provider.GetPayloadSerializer(genericEv)
	if err != nil {
		return nil, fmt.Errorf("getting payload serializer for event '%s': %w", eventName, err)
	}

	payloadBytes, err := payloadSerializer.Serialize(genericEv)
	if err != nil {
		return nil, fmt.Errorf("serializing payload for event '%s': %w", eventName, err)
	}

	wrapper := SerializedEventWrapper{
		EventName: eventName,
		Metadata:  ev.GetMeta(), // Get metadata from the event.Event wrapper
		Payload:   json.RawMessage(payloadBytes),
	}

	return json.Marshal(wrapper)
}

// Deserialize implements Bytes[event.Event].
func (s *DefaultEventSerde) Deserialize(data []byte) (event.Event, error) {
	var wrapper SerializedEventWrapper
	if err := json.Unmarshal(data, &wrapper); err != nil {
		return event.Event{}, fmt.Errorf("unmarshaling event wrapper: %w", err)
	}

	payloadDeserializer, err := s.provider.GetPayloadDeserializer(wrapper.EventName)
	if err != nil {
		return event.Event{}, fmt.Errorf("getting payload deserializer for event '%s': %w", wrapper.EventName, err)
	}

	genericEv, err := payloadDeserializer.Deserialize(wrapper.Payload)
	if err != nil {
		return event.Event{}, fmt.Errorf("deserializing payload for event '%s': %w (payload: %s)", wrapper.EventName, err, string(wrapper.Payload))
	}

	opts := []event.Option{}
	if len(wrapper.Metadata) > 0 { // Apply metadata if present
		opts = append(opts, event.WithMetadata(wrapper.Metadata))
	}

	return event.New(genericEv, opts...), nil
}
