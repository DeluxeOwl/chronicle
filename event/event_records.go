package event

import (
	"iter"

	"github.com/DeluxeOwl/chronicle/version"
)

// RawEvents is a slice of Raw events, representing a batch of changes that are ready
// to be persisted to the event log.
//
// Usage:
//
//	rawEvents := event.RawEvents{
//	    event.NewRaw("account/opened", []byte(`{"id":"123"}`)),
//	    event.NewRaw("account/money_deposited", []byte(`{"amount":100}`)),
//	}
type RawEvents []Raw

// All returns a Go 1.22+ iterator (iter.Seq) for the RawEvents slice,
// allowing for convenient and efficient iteration.
//
// Usage:
//
//	for rawEvt := range rawEvents.All() {
//	    // process rawEvt
//	}
//
// Returns an iter.Seq[Raw].
func (re RawEvents) All() iter.Seq[Raw] {
	return func(yield func(Raw) bool) {
		for _, e := range re {
			if !yield(e) {
				return
			}
		}
	}
}

// ToRecords converts a slice of Raw events into a slice of fully-formed *Record instances.
// It assigns the given logID and calculates a sequential version number for each event,
// starting from the provided `startingVersion`.
//
// Usage:
//
//	logID := event.LogID("account-123")
//	startingVersion := version.Version(2)
//	records := rawEvents.ToRecords(logID, startingVersion)
//	// records[0] will have version 3
//	// records[1] will have version 4
//
// Returns a slice of *Record pointers.
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

// Raw represents an event that has been serialized into a byte slice. It bundles the
// event's name with its data payload. This is the format required by the Appender
// interface for writing events to the log.
type Raw struct {
	name string
	data []byte
}

// NewRaw is a constructor to create a new Raw event instance.
//
// Usage:
//
//	rawEvent := event.NewRaw("event_name", []byte(`{"key":"value"}`))
//
// Returns a new Raw struct.
func NewRaw(name string, data []byte) Raw {
	return Raw{
		name: name,
		data: data,
	}
}

// EventName returns the name of the event.
func (raw *Raw) EventName() string {
	return raw.name
}

// Data returns the serialized event payload.
func (raw *Raw) Data() []byte {
	return raw.data
}

type LogID string

// Record represents a single event that has been persisted in and read from the event log.
// It contains the event's name and data, the ID of the log (aggregate) it belongs to,
// and its unique, sequential version number within that specific log. This is the
// primary data structure returned by the Reader interface.
type Record struct {
	logID   LogID
	raw     Raw
	version version.Version
}

// NewRecord is a constructor to create a new Record instance.
//
// Usage:
//
//	record := event.NewRecord(
//	    version.Version(1),
//	    event.LogID("agg-1"),
//	    "event_name",
//	    []byte(`{"data":"...}`),
//	)
//
// Returns a pointer to a new Record.
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

// EventName returns the name of the event from the record.
func (re *Record) EventName() string {
	return re.raw.EventName()
}

// Data returns the serialized event payload from the record.
func (re *Record) Data() []byte {
	return re.raw.Data()
}

// LogID returns the identifier of the event stream this record belongs to.
func (re *Record) LogID() LogID {
	return re.logID
}

// Version returns the sequential version of this event within its log stream.
func (re *Record) Version() version.Version {
	return re.version
}

// GlobalRecord represents a persisted event read from the global event stream.
// It contains all the fields of a standard Record, plus the `globalVersion`, which
// is its unique, sequential position across all event streams in the entire store.
type GlobalRecord struct {
	logID         LogID
	raw           Raw
	version       version.Version
	globalVersion version.Version
}

// NewGlobalRecord is a constructor to create a new GlobalRecord instance.
//
// Usage:
//
//	gRecord := event.NewGlobalRecord(
//		version.Version(101), // Global version
//		version.Version(5),   // Stream-specific version
//		event.LogID("agg-2"),
//		"event_name",
//		[]byte(`{"data":"..."}`),
//	)
//
// Returns a pointer to a new GlobalRecord.
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

// GlobalVersion returns the globally unique, sequential version of this event.
func (gr *GlobalRecord) GlobalVersion() version.Version {
	return gr.globalVersion
}

// EventName returns the name of the event from the global record.
func (gr *GlobalRecord) EventName() string {
	return gr.raw.EventName()
}

// Data returns the serialized event payload from the global record.
func (gr *GlobalRecord) Data() []byte {
	return gr.raw.Data()
}

// LogID returns the identifier of the event stream this global record belongs to.
func (gr *GlobalRecord) LogID() LogID {
	return gr.logID
}

// Version returns the sequential version of this event within its own log stream.
func (gr *GlobalRecord) Version() version.Version {
	return gr.version
}
