package projection

import (
	"context"

	"github.com/DeluxeOwl/chronicle/encoding"
	"github.com/DeluxeOwl/chronicle/event"
	"github.com/DeluxeOwl/chronicle/version"
	"github.com/emirpasic/gods/v2/maps/treemap"
	"github.com/emirpasic/gods/v2/sets/treeset"
)

//go:generate go run github.com/matryer/moq@latest -pkg projection_test -skip-ensure -rm -out projection_mock_test.go . Checkpointer Projection

type Projection interface {
	EventNames() []string
	Handle(ctx context.Context, rec event.GlobalRecord) error
}

type Checkpointer interface {
	Get(ctx context.Context, projectionType string) (version.Version, error)
	Set(ctx context.Context, projectionType string, v version.Version) error
}

type Group struct {
	log          event.GlobalLog
	registry     event.Registry[event.Any]
	codec        encoding.Codec
	checkpointer Checkpointer

	eventsToProjections *treemap.Map[string, Projection]
}

func NewGroup(log event.GlobalLog, checkpointer Checkpointer, projections []Projection) *Group {
	g := &Group{
		log:                 log,
		registry:            event.GlobalRegistry,
		codec:               encoding.DefaultJSONB,
		checkpointer:        checkpointer,
		eventsToProjections: treemap.New[string, Projection](),
	}

	for _, p := range projections {
		uniqueEventNames := treeset.New(p.EventNames()...)
		it := uniqueEventNames.Iterator()
		for it.Next() {
			g.eventsToProjections.Put(it.Value(), p)
		}
	}

	return g
}
