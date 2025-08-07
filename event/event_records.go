package event

import (
	"iter"

	"github.com/DeluxeOwl/chronicle/version"
)

// The raw event that's common to any event, has the name and a bytes payload.
// Implementations of the log should handle the Raw event.

type RawEvents []Raw

func (re RawEvents) All() iter.Seq[Raw] {
	return func(yield func(Raw) bool) {
		for _, e := range re {
			if !yield(e) {
				return
			}
		}
	}
}

func (re RawEvents) ToRecords(logID LogID, startingVersion version.Version) []*Record {
	records := make([]*Record, len(re))

	for i, rawEvt := range re {
		//nolint:gosec // It's not a problem in practice.
		records[i] = NewRecord(
			startingVersion+version.Version(i+1),
			logID,
			rawEvt.EventName(),
			rawEvt.Data())
	}

	return records
}

type Raw struct {
	name string
	data []byte
}

func NewRaw(name string, data []byte) Raw {
	return Raw{
		name: name,
		data: data,
	}
}

func (raw *Raw) EventName() string {
	return raw.name
}

func (raw *Raw) Data() []byte {
	return raw.data
}

// The event record which contains the raw event, its version and the logID it belongs to.
// Implementations of the log should store the data from the event record.

type LogID string

type Record struct {
	logID   LogID
	raw     Raw
	version version.Version
}

func NewRecord(version version.Version, logID LogID, name string, data []byte) *Record {
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

func (re *Record) Data() []byte {
	return re.raw.Data()
}

func (re *Record) LogID() LogID {
	return re.logID
}

func (re *Record) Version() version.Version {
	return re.version
}

type GlobalRecord struct {
	logID         LogID
	raw           Raw
	version       version.Version
	globalVersion version.Version
}

func NewGlobalRecord(
	globalVersion version.Version,
	version version.Version,
	logID LogID,
	name string,
	data []byte,
) *GlobalRecord {
	return &GlobalRecord{
		version: version,
		logID:   logID,
		raw: Raw{
			data: data,
			name: name,
		},
		globalVersion: globalVersion,
	}
}

func (gr *GlobalRecord) GlobalVersion() version.Version {
	return gr.globalVersion
}

func (gr *GlobalRecord) EventName() string {
	return gr.raw.EventName()
}

func (gr *GlobalRecord) Data() []byte {
	return gr.raw.Data()
}

func (gr *GlobalRecord) LogID() LogID {
	return gr.logID
}

func (gr *GlobalRecord) Version() version.Version {
	return gr.version
}
