package event

import (
	"github.com/DeluxeOwl/eventuallynow/version"
)

// The raw event that's common to any event, has the name and a bytes payload.
// Implementations of the log should handle the Raw event.

type RawData = []byte

type Raw struct {
	data RawData
	name string
}

func NewRaw(name string, data RawData) *Raw {
	return &Raw{
		name: name,
		data: data,
	}
}

func (raw *Raw) EventName() string {
	return raw.name
}

func (raw *Raw) Data() RawData {
	return raw.data
}

// The event record which contains the raw event, its version and the logID it belongs to.
// Implementations of the log should store the data from the event record.

type LogID string

type Record struct {
	raw     Raw
	logID   LogID
	version version.Version
}

func NewRecord(version version.Version, logID LogID, name string, data RawData) *Record {
	return &Record{
		version: version,
		logID:   logID,
		raw: Raw{
			data: data,
			name: name,
		},
	}
}

func (re *Record) EventName() string {
	return re.raw.EventName()
}

func (re *Record) Data() RawData {
	return re.raw.Data()
}

func (re *Record) LogID() LogID {
	return re.logID
}

func (re *Record) Version() version.Version {
	return re.version
}
