package chronicle

import (
	"github.com/DeluxeOwl/chronicle/aggregate"
	"github.com/DeluxeOwl/chronicle/event"
)

func NewEventSourcedRepository[TID aggregate.ID, E event.Any, R aggregate.Root[TID, E]](
	eventLog event.Log,
	newRoot func() R,
	opts ...aggregate.ESRepoOption,
) (*aggregate.ESRepo[TID, E, R], error) {
	return aggregate.NewESRepo(eventLog, newRoot, opts...)
}
