package aggregate

import (
	"github.com/DeluxeOwl/eventuallynow/event"
	"github.com/DeluxeOwl/eventuallynow/version"
	"github.com/DeluxeOwl/zerrors"
)

type Base struct {
	version        version.Version
	recordedEvents []event.Event
}

func (br *Base) Version() version.Version { return br.version }

func (br *Base) FlushRecordedEvents() []event.Event {
	flushed := br.recordedEvents
	br.recordedEvents = nil

	return flushed
}

//nolint:unused // False positive.
func (br *Base) setVersion(v version.Version) {
	br.version = v
}

//nolint:unused // False positive.
func (br *Base) recordThat(aggregate Aggregate, events ...event.Event) error {
	for _, event := range events {
		if err := aggregate.Apply(event.Unwrap()); err != nil {
			return zerrors.New(ErrFailedToRecord).WithError(err)
		}

		br.recordedEvents = append(br.recordedEvents, event)
		br.version++
	}

	return nil
}
