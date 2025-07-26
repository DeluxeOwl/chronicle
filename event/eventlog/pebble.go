package eventlog

import (
	"context"
	"fmt"

	"github.com/DeluxeOwl/chronicle/event"
	"github.com/DeluxeOwl/chronicle/version"
	"github.com/cockroachdb/pebble"
)

var _ event.Log = new(Pebble)

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
	panic("unimplemented")
}

func (p *Pebble) ReadEvents(
	ctx context.Context,
	id event.LogID,
	selector version.Selector,
) event.Records {
	panic("unimplemented")
}
