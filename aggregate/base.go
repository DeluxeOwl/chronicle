package aggregate

import (
	"fmt"

	"github.com/DeluxeOwl/chronicle/event"
	"github.com/DeluxeOwl/chronicle/version"
)

type Base struct {
	version version.Version

	//nolint:unused // False positive.
	uncommitedEvents flushedUncommitedEvents
}

func (br *Base) Version() version.Version { return br.version }

type flushedUncommitedEvents []eventWrapper[event.Any]

//nolint:unused // False positive.
func (br *Base) flushUncommitedEvents() flushedUncommitedEvents {
	flushed := br.uncommitedEvents
	br.uncommitedEvents = nil

	return flushed
}

//nolint:unused // False positive.
func (br *Base) setVersion(v version.Version) {
	br.version = v
}

type anyEventApplier interface {
	Apply(event.Any) error
}

//nolint:unused // False positive.
func (br *Base) recordThat(aggregate anyEventApplier, events ...eventWrapper[event.Any]) error {
	for _, event := range events {
		anyEvent := event.Unwrap()

		if err := aggregate.Apply(anyEvent); err != nil {
			return fmt.Errorf("record events: root apply: %w", err)
		}

		br.uncommitedEvents = append(br.uncommitedEvents, event)
		br.version++
	}

	return nil
}
