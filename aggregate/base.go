package aggregate

import (
	"fmt"

	"github.com/DeluxeOwl/chronicle/event"
	"github.com/DeluxeOwl/chronicle/version"
)

type Base struct {
	uncommitedEvents flushedUncommitedEvents
	version          version.Version
}

func (br *Base) Version() version.Version { return br.version }

type flushedUncommitedEvents []event.Any

func (br *Base) flushUncommitedEvents() flushedUncommitedEvents {
	flushed := br.uncommitedEvents
	br.uncommitedEvents = nil

	return flushed
}

func (br *Base) setVersion(v version.Version) {
	br.version = v
}

type anyEventApplier interface {
	Apply(event.Any) error
}

func (br *Base) recordThat(aggregate anyEventApplier, events ...event.Any) error {
	for _, anyEvent := range events {
		if err := aggregate.Apply(anyEvent); err != nil {
			return fmt.Errorf("record events: root apply: %w", err)
		}

		br.uncommitedEvents = append(br.uncommitedEvents, anyEvent)
		br.version++
	}

	return nil
}
