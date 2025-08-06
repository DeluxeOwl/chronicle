package event

import (
	"context"
	"fmt"
	"iter"

	"github.com/DeluxeOwl/chronicle/version"
)

type Reader interface {
	ReadEvents(ctx context.Context, id LogID, selector version.Selector) Records
}

type Appender interface {
	AppendEvents(
		ctx context.Context,
		id LogID,
		expected version.Check,
		events RawEvents,
	) (version.Version, error)
}

type Log interface {
	Reader
	Appender
}

type Records iter.Seq2[*Record, error]

func (r Records) Collect() ([]*Record, error) {
	collected := []*Record{}
	for record, err := range r {
		if err != nil {
			return nil, fmt.Errorf("records collect: %w", err)
		}
		collected = append(collected, record)
	}
	return collected, nil
}

// This is implemented by the event logs who can keep a global version of the events.
type GlobalReader interface {
	ReadAllEvents(ctx context.Context, globalSelector version.Selector) GlobalRecords
}

type GlobalRecords iter.Seq2[*GlobalRecord, error]

func (r GlobalRecords) Collect() ([]*GlobalRecord, error) {
	collected := []*GlobalRecord{}
	for record, err := range r {
		if err != nil {
			return nil, fmt.Errorf("records collect: %w", err)
		}
		collected = append(collected, record)
	}
	return collected, nil
}
