package aggregate

import (
	"fmt"

	"github.com/DeluxeOwl/eventuallynow/event"
	"github.com/DeluxeOwl/eventuallynow/version"
	"github.com/DeluxeOwl/zerrors"
)

type AggregateError string

const (
	ErrFailedToRecord AggregateError = "failed_to_record_event"
)

type ID interface {
	fmt.Stringer
}

type Aggregate interface {
	Apply(event.GenericEvent) error
}

type RecordedEventsFlusher interface {
	FlushRecordedEvents() []event.Event
}

type Root[TypeID ID] interface {
	Aggregate
	RecordedEventsFlusher

	ID() TypeID
	Version() version.Version

	// EventRecorder implements these, so you *have* to embed EventRecorder.
	setVersion(version.Version)
	recordThat(Aggregate, ...event.Event) error
}

func RecordEventsBatch[TypeID ID](root Root[TypeID], events []event.GenericEvent) error {
	ee := make([]event.Event, len(events))
	for i := range events {
		ee[i] = event.New(events[i])
	}
	return root.recordThat(root, ee...)
}

func RecordEvent[TypeID ID](root Root[TypeID], e event.GenericEvent) error {
	return root.recordThat(root, event.New(e))
}

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
