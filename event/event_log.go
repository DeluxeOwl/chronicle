package event

import (
	"context"
	"iter"

	"github.com/DeluxeOwl/chronicle/version"
)

type AllReader interface {
	ReadAllEvents(ctx context.Context, selector version.Selector) Records
}

type Reader interface {
	ReadEvents(ctx context.Context, id LogID, selector version.Selector) Records
}

type Appender interface {
	AppendEvents(ctx context.Context, id LogID, expected version.Check, events RawEvents) (version.Version, error)
}

// TODO: Should this take generic type params?
// Imo someone should use a repo
// But what if I want a postgres that saves a Person specifically, and then convert that to this interface that's more "generic"?
type Log interface {
	Reader
	Appender
}

type Records iter.Seq2[*Record, error]
