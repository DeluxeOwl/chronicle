package event

import (
	"fmt"

	"github.com/DeluxeOwl/eventuallynow/version"
)

type LogID string

type RecordedEvent struct {
	version version.Version
	logID   LogID
	ev      RawEvent
}

func NewRecorded(version version.Version, logID LogID, name string, data []byte) *RecordedEvent {
	return &RecordedEvent{
		version: version,
		logID:   logID,
		ev: RawEvent{
			data: data,
			name: name,
		},
	}
}

func (re *RecordedEvent) Version() version.Version {
	return re.version
}

func (re *RecordedEvent) LogID() LogID {
	return re.logID
}

func (re *RecordedEvent) EventName() string {
	return re.ev.EventName()
}

func (re *RecordedEvent) Bytes() []byte {
	return re.ev.Bytes()
}

func RawToRecorded(startingVersion version.Version, id LogID, events []RawEvent) []*RecordedEvent {
	recordedEvents := make([]*RecordedEvent, len(events))
	for i, e := range events {
		//nolint:gosec // It's not a problem in practice.
		recordedEvents[i] = NewRecorded(startingVersion+version.Version(i+1), id, e.EventName(), e.Bytes())
	}
	return recordedEvents
}

func ToRaw(event Event) (RawEvent, error) {
	bytes, err := Marshal(event.Unwrap())
	if err != nil {
		return RawEvent{}, fmt.Errorf("marshal event: %w", err)
	}

	return RawEvent{
		data: bytes,
		name: event.EventName(),
	}, nil
}

func ToRawBatch(events []Event) ([]RawEvent, error) {
	rawEvents := make([]RawEvent, len(events))
	for i := range events {
		raw, err := ToRaw(events[i])
		if err != nil {
			return nil, fmt.Errorf("to raw batch: %w", err)
		}

		rawEvents[i] = raw
	}

	return rawEvents, nil
}
