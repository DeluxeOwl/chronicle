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

func NewRecorded(version version.Version, logID LogID, event Event) *RecordedEvent {
	return &RecordedEvent{
		version: version,
		logID:   logID,
		ev:      event,
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

func (re *RecordedEvent) EventAny() EventAny {
	return re.ev.Unwrap()
}

type RecordedEventSnapshot struct {
	Version version.Version `json:"version"`
	LogID   LogID           `json:"logID"`
	Event   EventAny        `json:"event"`
}

func (re *RecordedEvent) Snapshot() *RecordedEventSnapshot {
	return &RecordedEventSnapshot{
		Version: re.version,
		LogID:   re.logID,
		Event:   re.ev.event,
	}
}

func ToRecorded(startingVersion version.Version, id LogID, events ...Event) []*RecordedEvent {
	recordedEvents := make([]*RecordedEvent, len(events))
	for i, e := range events {
		//nolint:gosec // It's not a problem in practice.
		recordedEvents[i] = NewRecorded(startingVersion+version.Version(i+1), id, e)
	}
	return recordedEvents
}
