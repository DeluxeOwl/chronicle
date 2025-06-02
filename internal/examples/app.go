package examples

import (
	"fmt"

	"github.com/DeluxeOwl/eventuallynow/event"

	"github.com/DeluxeOwl/eventuallynow/internal/examples/person"
	"github.com/DeluxeOwl/eventuallynow/serde"
)

type AppPayloadSerdeProvider struct {
	personSerde serde.Serde[event.GenericEvent, []byte] `exhaustruct:"optional"`
}

func NewAppPayloadSerdeProvider() *AppPayloadSerdeProvider {
	return &AppPayloadSerdeProvider{
		personSerde: person.NewPersonPayloadSerde(),
	}
}

func (p *AppPayloadSerdeProvider) GetPayloadSerializer(ge event.GenericEvent) (serde.Serializer[event.GenericEvent, []byte], error) {
	switch ge.(type) {
	case *person.PersonEvent:
		return p.personSerde, nil

	default:
		return nil, fmt.Errorf("AppPayloadSerdeProvider: no serializer registered for event type %T", ge)
	}
}

func (p *AppPayloadSerdeProvider) GetPayloadDeserializer(eventName string) (serde.Deserializer[event.GenericEvent, []byte], error) {
	// Map eventName (string) to the correct payload deserializer.
	// These are the event names like "person-was-born", "aged-one-year".
	if eventName == (*person.WasBorn)(nil).EventName() || eventName == (*person.AgedOneYear)(nil).EventName() {
		return p.personSerde, nil
	}
	// Add cases for other event names from other modules
	return nil, fmt.Errorf("AppPayloadSerdeProvider: no deserializer registered for event name '%s'", eventName)
}

var _ serde.GenericPayloadSerdeProvider = &AppPayloadSerdeProvider{}
