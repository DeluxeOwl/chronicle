package event

import (
	"fmt"

	"github.com/DeluxeOwl/eventuallynow/version"
)

func ConvertRawToRecorded(startingVersion version.Version, id LogID, events []Raw) []*Record {
	recordedEvents := make([]*Record, len(events))
	for i, e := range events {
		//nolint:gosec // It's not a problem in practice.
		recordedEvents[i] = NewRecord(startingVersion+version.Version(i+1), id, e.EventName(), e.Data())
	}
	return recordedEvents
}

func ConvertToRaw(event Event) (Raw, error) {
	bytes, err := Marshal(event.Unwrap())
	if err != nil {
		return Raw{}, fmt.Errorf("marshal event: %w", err)
	}

	return Raw{
		data: bytes,
		name: event.EventName(),
	}, nil
}

func ConvertEventsToRaw(events []Event) ([]Raw, error) {
	rawEvents := make([]Raw, len(events))
	for i := range events {
		raw, err := ConvertToRaw(events[i])
		if err != nil {
			return nil, fmt.Errorf("to raw batch: %w", err)
		}

		rawEvents[i] = raw
	}

	return rawEvents, nil
}
