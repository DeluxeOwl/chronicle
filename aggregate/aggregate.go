package aggregate

import (
	"fmt"

	"github.com/DeluxeOwl/eventuallynow/event"
	"github.com/DeluxeOwl/eventuallynow/version"
)

type ID interface {
	fmt.Stringer
}

type Aggregate interface {
	Apply(event.Payload) error
}

type RecordedEventsFlusher interface {
	FlushRecordedEvents() []event.GenericEvent
}

type Root[TypeID ID] interface {
	Aggregate
	RecordedEventsFlusher

	ID() TypeID
	Version() version.Version

	// EventRecorder implements these, so you *have* to embed EventRecorder.
	setVersion(version.Version)
	recordThat(Aggregate, ...event.GenericEvent) error
}

type Type[TypeID ID, T Root[TypeID]] struct {
	Name    string
	Factory func() T
}

func RecordThat[TypeID ID](root Root[TypeID], events ...event.GenericEvent) error {
	return root.recordThat(root, events...)
}

type EventRecorder struct {
	version        version.Version
	recordedEvents []event.GenericEvent
}

func (br EventRecorder) Version() version.Version { return br.version }

func (br *EventRecorder) FlushRecordedEvents() []event.GenericEvent {
	flushed := br.recordedEvents
	br.recordedEvents = nil

	return flushed
}

func (br *EventRecorder) setVersion(v version.Version) {
	br.version = v
}

func (br *EventRecorder) recordThat(aggregate Aggregate, events ...event.GenericEvent) error {
	for _, event := range events {
		if err := aggregate.Apply(event.Payload); err != nil {
			return fmt.Errorf("aggregate.EventRecorder: failed to record event, %w", err)
		}

		br.recordedEvents = append(br.recordedEvents, event)
		br.version++
	}

	return nil
}
