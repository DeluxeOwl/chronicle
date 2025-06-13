package convert

import (
	"fmt"

	"github.com/DeluxeOwl/chronicle/encoding"
	"github.com/DeluxeOwl/chronicle/event"
	"github.com/DeluxeOwl/chronicle/version"
)

func RawToRecorded(startingVersion version.Version, id event.LogID, events []event.Raw) []*event.Record {
	recordedEvents := make([]*event.Record, len(events))
	for i, e := range events {
		//nolint:gosec // It's not a problem in practice.
		recordedEvents[i] = event.NewRecord(startingVersion+version.Version(i+1), id, e.EventName(), e.Data())
	}
	return recordedEvents
}

func EventToRaw(ev event.Event) (event.Raw, error) {
	bytes, err := encoding.Marshal(ev.Unwrap())
	if err != nil {
		return event.Raw{}, fmt.Errorf("convert event to raw event: marshal event: %w", err)
	}

	return *event.NewRaw(ev.EventName(), bytes), nil
}

func EventsToRaw(events []event.Event) ([]event.Raw, error) {
	rawEvents := make([]event.Raw, len(events))
	for i := range events {
		raw, err := EventToRaw(events[i])
		if err != nil {
			return nil, fmt.Errorf("convert events: %w", err)
		}

		rawEvents[i] = raw
	}

	return rawEvents, nil
}
