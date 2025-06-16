package aggregate

import (
	"fmt"

	"github.com/DeluxeOwl/chronicle/event"
	"github.com/DeluxeOwl/chronicle/version"
)

type Base struct {
	version version.Version

	//nolint:unused // False positive.
	uncommitedEvents event.FlushedUncommitedEvents
}

func (br *Base) Version() version.Version { return br.version }

//nolint:unused // False positive.
func (br *Base) flushUncommitedEvents() event.FlushedUncommitedEvents {
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
func (br *Base) recordThat(aggregate anyEventApplier, events ...event.Event) error {
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
