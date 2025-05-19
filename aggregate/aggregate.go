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

type Internal interface {
	FlushRecordedEvents() []event.GenericEvent
}

type Root[TypeID ID] interface {
	Aggregate
	Internal

	ID() TypeID
	Version() version.Version

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

type BaseRoot struct {
	version        version.Version
	recordedEvents []event.GenericEvent
}

func (br BaseRoot) Version() version.Version { return br.version }

func (br *BaseRoot) FlushRecordedEvents() []event.GenericEvent {
	flushed := br.recordedEvents
	br.recordedEvents = nil

	return flushed
}

func (br *BaseRoot) setVersion(v version.Version) {
	br.version = v
}

func (br *BaseRoot) recordThat(aggregate Aggregate, events ...event.GenericEvent) error {
	for _, event := range events {
		if err := aggregate.Apply(event.Payload); err != nil {
			return fmt.Errorf("aggregate.BaseRoot: failed to record event, %w", err)
		}

		br.recordedEvents = append(br.recordedEvents, event)
		br.version++
	}

	return nil
}
