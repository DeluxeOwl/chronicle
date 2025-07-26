package eventlog

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/DeluxeOwl/chronicle/event"
	"github.com/DeluxeOwl/chronicle/version"
	"github.com/cockroachdb/pebble"
)

var _ event.Log = new(Pebble)

var (
	eventKeyPrefix   = []byte("e/")
	versionKeyPrefix = []byte("v/")
)

// We don't need to store LogID and Version in the value, as they are already in the key.
type pebbleEventData struct {
	Data      []byte `json:"data"`
	EventName string `json:"eventName"`
}

type Pebble struct {
	db *pebble.DB
}

func NewPebble(dirname string, opts *pebble.Options) (*Pebble, error) {
	db, err := pebble.Open(dirname, opts)
	if err != nil {
		return nil, fmt.Errorf("new pebble: %w", err)
	}

	return &Pebble{
		db: db,
	}, nil
}

func NewPebbleWithDB(db *pebble.DB) *Pebble {
	return &Pebble{
		db: db,
	}
}

func (p *Pebble) AppendEvents(
	ctx context.Context,
	id event.LogID,
	expected version.Check,
	events event.RawEvents,
) (version.Version, error) {
	if err := ctx.Err(); err != nil {
		return version.Zero, fmt.Errorf("append events: %w", err)
	}

	logIDVersionKey := versionKeyFor(id)
	actualLogVersion, err := p.getLogVersion(logIDVersionKey)
	if err != nil {
		return version.Zero, fmt.Errorf("append events: %w", err)
	}

	if exp, ok := expected.(version.CheckExact); ok {
		if err := exp.CheckExact(actualLogVersion); err != nil {
			return version.Zero, fmt.Errorf("append events: %w", err)
		}
	}

	if len(events) == 0 {
		return actualLogVersion, nil
	}

	// Atomic append
	batch := p.db.NewBatch()
	defer batch.Close()
	eventRecords := events.ToRecords(id, actualLogVersion)
	for _, record := range eventRecords {
		key := eventKeyFor(record)
		value, err := json.Marshal(pebbleEventData{
			Data:      record.Data(),
			EventName: record.EventName(),
		})
		if err != nil {
			return 0, fmt.Errorf("append events: could not marshal event data: %w", err)
		}
		if err := batch.Set(key, value, pebble.NoSync); err != nil {
			return 0, fmt.Errorf("append events: could not add event to batch: %w", err)
		}
	}
	newStreamVersion := actualLogVersion + version.Version(len(events))
	versionValue := make([]byte, uint64sizeBytes)
	binary.BigEndian.PutUint64(versionValue, uint64(newStreamVersion))

	if err := batch.Set(logIDVersionKey, versionValue, pebble.NoSync); err != nil {
		return 0, fmt.Errorf("append events: could not add version to batch: %w", err)
	}

	// Commit the batch atomically. Use pebble.Sync to guarantee durability.
	if err := batch.Commit(pebble.Sync); err != nil {
		return 0, fmt.Errorf("append events: commit batch: %w", err)
	}

	return newStreamVersion, nil
}

func (p *Pebble) ReadEvents(
	ctx context.Context,
	id event.LogID,
	selector version.Selector,
) event.Records {
	panic("unimplemented")
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

const (
	uint64sizeBytes = 8
	slashSizeBytes  = 1
)

func eventKeyFor(record *event.Record) []byte {
	idBytes := []byte(record.LogID())
	key := make([]byte, 0, len(eventKeyPrefix)+len(idBytes)+slashSizeBytes+uint64sizeBytes)
	key = append(key, eventKeyPrefix...)
	key = append(key, idBytes...)
	key = append(key, '/')
	key = binary.BigEndian.AppendUint64(key, uint64(record.Version()))
	return key
}
