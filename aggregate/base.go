package aggregate

import (
	"fmt"

	"github.com/DeluxeOwl/chronicle/event"
	"github.com/DeluxeOwl/chronicle/version"
)

type Base struct {
	uncommittedEvents flushedUncommittedEvents
	version           version.Version
}

func (br *Base) Version() version.Version { return br.version }

type flushedUncommittedEvents []event.Any

func (br *Base) flushUncommittedEvents() flushedUncommittedEvents {
	flushed := br.uncommittedEvents
	br.uncommittedEvents = nil

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

		br.uncommittedEvents = append(br.uncommittedEvents, anyEvent)
		br.version++
	}

	return nil
}
