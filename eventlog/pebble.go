package eventlog

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/DeluxeOwl/chronicle/event"
	"github.com/DeluxeOwl/chronicle/version"
	"github.com/cockroachdb/pebble"
)

var (
	_ event.GlobalReader                         = new(Pebble)
	_ event.Log                                  = new(Pebble)
	_ event.TransactionalEventLog[*pebble.Batch] = new(Pebble)
)

var (
	eventKeyPrefix       = []byte("e/")
	versionKeyPrefix     = []byte("v/")
	globalEventKeyPrefix = []byte("ge/")
	globalVersionKey     = []byte("g_version")
)

// We don't need to store LogID and Version in the value, as they are already in the key.
type pebbleEventData struct {
	Data      []byte `json:"data"`
	EventName string `json:"eventName"`
}

// pebbleGlobalEventData stores the data for the global event index.
// We don't store the global version as it's part of the key.
type pebbleGlobalEventData struct {
	LogID     event.LogID     `json:"logID"`
	Version   version.Version `json:"version"`
	Data      []byte          `json:"data"`
	EventName string          `json:"eventName"`
}

type Pebble struct {
	db *pebble.DB
	mu sync.Mutex
}

func NewPebble(db *pebble.DB) *Pebble {
	return &Pebble{
		db: db,
		mu: sync.Mutex{},
	}
}

func (p *Pebble) AppendEvents(
	ctx context.Context,
	id event.LogID,
	expected version.Check,
	events event.RawEvents,
) (version.Version, error) {
	var newVersion version.Version

	err := p.WithinTx(ctx, func(ctx context.Context, batch *pebble.Batch) error {
		v, _, err := p.AppendInTx(ctx, batch, id, expected, events)
		if err != nil {
			return err
		}
		newVersion = v
		return nil
	})
	if err != nil {
		return version.Zero, fmt.Errorf("append events: %w", err)
	}
	return newVersion, nil
}

func (p *Pebble) AppendInTx(
	ctx context.Context,
	batch *pebble.Batch,
	id event.LogID,
	expected version.Check,
	events event.RawEvents,
) (version.Version, []*event.Record, error) {
	if err := ctx.Err(); err != nil {
		return version.Zero, nil, fmt.Errorf("append in tx: %w", err)
	}

	if len(events) == 0 {
		return version.Zero, nil, fmt.Errorf("append in tx: %w", ErrNoEvents)
	}

	// Get current versions.
	logIDVersionKey := versionKeyFor(id)
	actualLogVersion, err := p.getLogVersion(logIDVersionKey)
	if err != nil {
		return version.Zero, nil, fmt.Errorf("append in tx: %w", err)
	}
	globalVersion, err := p.getGlobalVersion()
	if err != nil {
		return version.Zero, nil, fmt.Errorf("append in tx: could not get global version: %w", err)
	}

	exp, ok := expected.(version.CheckExact)
	if !ok {
		return version.Zero, nil, fmt.Errorf("append in tx: %w", ErrUnsupportedCheck)
	}

	if err := exp.CheckExact(actualLogVersion); err != nil {
		return version.Zero, nil, fmt.Errorf("append in tx: %w", err)
	}

	records := events.ToRecords(id, actualLogVersion)
	for i, record := range records {
		// Set event data.
		key := eventKeyFor(record.LogID(), record.Version())
		value, err := json.Marshal(pebbleEventData{
			Data:      record.Data(),
			EventName: record.EventName(),
		})
		if err != nil {
			return version.Zero, nil, fmt.Errorf(
				"append in tx: could not marshal event data: %w",
				err,
			)
		}
		if err := batch.Set(key, value, pebble.NoSync); err != nil {
			return version.Zero, nil, fmt.Errorf(
				"append in tx: could not add event to batch: %w",
				err,
			)
		}

		//nolint:gosec // not a problem.
		currentGlobalVersion := globalVersion + version.Version(i) + 1
		globalKey := globalEventKeyFor(currentGlobalVersion)
		globalValue, err := json.Marshal(pebbleGlobalEventData{
			LogID:     record.LogID(),
			Version:   record.Version(),
			Data:      record.Data(),
			EventName: record.EventName(),
		})
		if err != nil {
			return version.Zero, nil, fmt.Errorf(
				"append in tx: could not marshal global event data: %w",
				err,
			)
		}
		if err := batch.Set(globalKey, globalValue, pebble.NoSync); err != nil {
			return version.Zero, nil, fmt.Errorf(
				"append in tx: could not add global event to batch: %w",
				err,
			)
		}
	}

	// Update stream version.
	newStreamVersion := actualLogVersion + version.Version(len(events))
	versionValue := make([]byte, uint64sizeBytes)
	binary.BigEndian.PutUint64(versionValue, uint64(newStreamVersion))

	if err := batch.Set(logIDVersionKey, versionValue, pebble.NoSync); err != nil {
		return version.Zero, nil, fmt.Errorf(
			"append in tx: could not add version to batch: %w",
			err,
		)
	}

	// Update global version.
	newGlobalVersion := globalVersion + version.Version(len(events))
	globalVersionValue := make([]byte, uint64sizeBytes)
	binary.BigEndian.PutUint64(globalVersionValue, uint64(newGlobalVersion))
	if err := batch.Set(globalVersionKey, globalVersionValue, pebble.NoSync); err != nil {
		return version.Zero, nil, fmt.Errorf(
			"append in tx: could not add global version to batch: %w",
			err,
		)
	}

	return newStreamVersion, records, nil
}

func (p *Pebble) WithinTx(
	ctx context.Context,
	fn func(ctx context.Context, batch *pebble.Batch) error,
) error {
	// The lock ensures that the read-then-write logic of AppendInTx is atomic.
	p.mu.Lock()
	defer p.mu.Unlock()

	batch := p.db.NewBatch()
	defer batch.Close()

	if err := fn(ctx, batch); err != nil {
		return err
	}

	if err := batch.Commit(pebble.Sync); err != nil {
		return fmt.Errorf("within tx: commit batch: %w", err)
	}

	return nil
}

func (p *Pebble) ReadEvents(
	ctx context.Context,
	id event.LogID,
	selector version.Selector,
) event.Records {
	return func(yield func(*event.Record, error) bool) {
		startKey := eventKeyFor(id, selector.From)
		prefix := eventKeyPrefixFor(id)

		// Use an iterator with an upper bound to only scan keys for the given log ID.
		//nolint:exhaustruct // Unnecessary.
		iter, err := p.db.NewIter(&pebble.IterOptions{
			LowerBound: startKey,
			UpperBound: prefixEndKey(prefix),
		})
		if err != nil {
			yield(nil, fmt.Errorf("read events: create iterator: %w", err))
			return
		}
		defer iter.Close()

		for iter.First(); iter.Valid(); iter.Next() {
			if err := ctx.Err(); err != nil {
				yield(nil, err)
				return
			}

			// Key contains logID and version
			_, eventVersion, err := parseEventKey(iter.Key())
			if err != nil {
				yield(nil, fmt.Errorf("read events: could not parse event key: %w", err))
				return
			}

			var data pebbleEventData
			if err := json.Unmarshal(iter.Value(), &data); err != nil {
				yield(nil, fmt.Errorf("read events: could not unmarshal event data: %w", err))
				return
			}

			record := event.NewRecord(eventVersion, id, data.EventName, data.Data)

			if !yield(record, nil) {
				return
			}
		}
		if err := iter.Error(); err != nil {
			yield(nil, fmt.Errorf("read events: iterator error: %w", err))
		}
	}
}

func (p *Pebble) ReadAllEvents(
	ctx context.Context,
	globalSelector version.Selector,
) event.GlobalRecords {
	return func(yield func(*event.GlobalRecord, error) bool) {
		startKey := globalEventKeyFor(globalSelector.From)

		//nolint:exhaustruct // Unnecessary.
		iter, err := p.db.NewIter(&pebble.IterOptions{
			LowerBound: startKey,
			UpperBound: prefixEndKey(globalEventKeyPrefix),
		})
		if err != nil {
			yield(nil, fmt.Errorf("read all events: create iterator: %w", err))
			return
		}
		defer iter.Close()

		for iter.First(); iter.Valid(); iter.Next() {
			if err := ctx.Err(); err != nil {
				yield(nil, err)
				return
			}

			globalVersion, err := parseGlobalEventKey(iter.Key())
			if err != nil {
				yield(nil, fmt.Errorf("read all events: could not parse global event key: %w", err))
				return
			}

			var data pebbleGlobalEventData
			if err := json.Unmarshal(iter.Value(), &data); err != nil {
				yield(
					nil,
					fmt.Errorf("read all events: could not unmarshal global event data: %w", err),
				)
				return
			}

			record := event.NewGlobalRecord(
				globalVersion,
				data.Version,
				data.LogID,
				data.EventName,
				data.Data,
			)

			if !yield(record, nil) {
				return
			}
		}
		if err := iter.Error(); err != nil {
			yield(nil, fmt.Errorf("read all events: iterator error: %w", err))
		}
	}
}

// parseEventKey extracts the log ID and version from an event key.
// This version is robust against Log IDs that contain '/'.
func parseEventKey(key []byte) (event.LogID, version.Version, error) {
	// Key structure: e/{logID}/[8-byte-version]
	if !bytes.HasPrefix(key, eventKeyPrefix) {
		return "", 0, fmt.Errorf("invalid event key prefix: %q", key)
	}
	if len(key) < len(eventKeyPrefix)+1+8 { // prefix + min 1 char ID + / + 8 byte version
		return "", 0, fmt.Errorf("invalid event key length: %q", key)
	}

	// Version is the last 8 bytes
	versionBytes := key[len(key)-8:]
	eventVersion := version.Version(binary.BigEndian.Uint64(versionBytes))

	// LogID is between the prefix and the final slash before the version
	logIDBytes := key[len(eventKeyPrefix) : len(key)-9] // -9 = -8 for version, -1 for slash
	logID := event.LogID(logIDBytes)

	return logID, eventVersion, nil
}

// prefixEndKey returns the key that immediately follows all keys with the given prefix.
func prefixEndKey(prefix []byte) []byte {
	end := make([]byte, len(prefix))
	copy(end, prefix)
	for i := len(end) - 1; i >= 0; i-- {
		end[i]++
		if end[i] != 0 {
			return end[:i+1]
		}
	}
	return nil
}

func parseGlobalEventKey(key []byte) (version.Version, error) {
	if !bytes.HasPrefix(key, globalEventKeyPrefix) {
		return 0, fmt.Errorf("invalid global event key prefix: %q", key)
	}
	if len(key) != len(globalEventKeyPrefix)+uint64sizeBytes {
		return 0, fmt.Errorf("invalid global event key length: %q", key)
	}
	versionBytes := key[len(globalEventKeyPrefix):]
	return version.Version(binary.BigEndian.Uint64(versionBytes)), nil
}

func (p *Pebble) getGlobalVersion() (version.Version, error) {
	value, closer, err := p.db.Get(globalVersionKey)
	if err != nil {
		if errors.Is(err, pebble.ErrNotFound) {
			return version.Zero, nil
		}
		return version.Zero, fmt.Errorf("get global version: %w", err)
	}
	defer closer.Close()

	return version.Version(binary.BigEndian.Uint64(value)), nil
}

func (p *Pebble) getLogVersion(key []byte) (version.Version, error) {
	value, closer, err := p.db.Get(key)
	if err != nil {
		if errors.Is(err, pebble.ErrNotFound) {
			return version.Zero, nil
		}
		return version.Zero, fmt.Errorf("get log version: %w", err)
	}
	defer closer.Close()

	return version.Version(binary.BigEndian.Uint64(value)), nil
}

func versionKeyFor(id event.LogID) []byte {
	return append(versionKeyPrefix, []byte(id)...)
}

func eventKeyPrefixFor(id event.LogID) []byte {
	return append(eventKeyPrefix, []byte(id+"/")...)
}

const (
	uint64sizeBytes = 8
	slashSizeBytes  = 1
)

func eventKeyFor(id event.LogID, version version.Version) []byte {
	idBytes := []byte(id)
	key := make([]byte, 0, len(eventKeyPrefix)+len(idBytes)+slashSizeBytes+uint64sizeBytes)
	key = append(key, eventKeyPrefix...)
	key = append(key, idBytes...)
	key = append(key, '/')
	key = binary.BigEndian.AppendUint64(key, uint64(version))
	return key
}

func globalEventKeyFor(globalVersion version.Version) []byte {
	key := make([]byte, 0, len(globalEventKeyPrefix)+uint64sizeBytes)
	key = append(key, globalEventKeyPrefix...)
	key = binary.BigEndian.AppendUint64(key, uint64(globalVersion))
	return key
}
