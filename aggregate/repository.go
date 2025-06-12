package aggregate

import (
	"errors"
	"fmt"

	"github.com/DeluxeOwl/chronicle/event"
)

func LoadFromRecordedEvents[TypeID ID, TEvent event.Any](root Root[TypeID, TEvent], registry event.Registry, events event.Records) error {
	for e, err := range events {
		if err != nil {
			return fmt.Errorf("load from events: %w", err)
		}

		fact, ok := registry.NewEventFactory(e.EventName())
		if !ok {
			return errors.New("factory not registered for" + e.EventName())
		}

		ev := fact()
		if err := event.Unmarshal(e.Data(), ev); err != nil {
			return fmt.Errorf("internal unmarshal record data: %w", err)
		}

		anyEvt, ok := ev.(TEvent)
		if !ok {
			return errors.New("internal: this isn't supposed to happen (todo)")
		}

		if err := root.Apply(anyEvt); err != nil {
			return fmt.Errorf("load from events: root apply: %w", err)
		}

		root.setVersion(e.Version())
	}

	return nil
}
