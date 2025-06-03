package event

import (
	"github.com/DeluxeOwl/eventuallynow/version"
)

type LogID string

type RecordedEvent struct {
	version version.Version
	logID   LogID
	ev      Event
}

func NewRecorded(version version.Version, logID LogID, event EventAny) *RecordedEvent {
	return &RecordedEvent{
		version: version,
		logID:   logID,
		ev:      New(event),
	}
}

func (re *RecordedEvent) Version() version.Version {
	return re.version
}

func (re *RecordedEvent) LogID() LogID {
	return re.logID
}

func (re *RecordedEvent) Event() Event {
	return re.ev
}

func (re *RecordedEvent) EventName() string {
	return re.ev.event.EventName()
}

func (re *RecordedEvent) EventAny() EventAny {
	return re.ev.Unwrap()
}

type RecordedEventSerializableFields struct {
	Version   version.Version `json:"version"`
	LogID     LogID           `json:"logID"`
	EventName string          `json:"eventName"`
}

type RecordedEventSnapshot struct {
	RecordedEventSerializableFields
	Event EventAny `json:"event"`
}

func (re *RecordedEvent) Snapshot() *RecordedEventSnapshot {
	return &RecordedEventSnapshot{
		RecordedEventSerializableFields: RecordedEventSerializableFields{
			Version:   re.version,
			LogID:     re.logID,
			EventName: re.EventName(),
		},
		Event: re.ev.event,
	}
}

func ToRecorded(startingVersion version.Version, id LogID, events ...Event) []*RecordedEvent {
	recordedEvents := make([]*RecordedEvent, len(events))
	for i, e := range events {
		//nolint:gosec // It's not a problem in practice.
		recordedEvents[i] = NewRecorded(startingVersion+version.Version(i+1), id, e.Unwrap())
	}
	return recordedEvents
}
