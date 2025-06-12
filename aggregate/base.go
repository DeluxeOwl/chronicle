package aggregate

import (
	"fmt"

	"github.com/DeluxeOwl/chronicle/event"
	"github.com/DeluxeOwl/chronicle/version"
)

type Base struct {
	version          version.Version
	uncommitedEvents []event.Event
}

func (br *Base) Version() version.Version { return br.version }

func (br *Base) FlushUncommitedEvents() []event.Event {
	flushed := br.uncommitedEvents
	br.uncommitedEvents = nil

	return flushed
}

//nolint:unused // False positive.
func (br *Base) setVersion(v version.Version) {
	br.version = v
}

//nolint:unused // False positive.
func (br *Base) recordThat(aggregate Aggregate[event.Any], events ...event.Event) error {
	for _, event := range events {
		anyEvent := event.Unwrap()

		if err := aggregate.Apply(anyEvent); err != nil {
			return fmt.Errorf("record that: aggregate apply: %w", err)
		}

		br.uncommitedEvents = append(br.uncommitedEvents, event)
		br.version++
	}

	return nil
}
