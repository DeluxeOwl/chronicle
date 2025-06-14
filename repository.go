package chronicle

import (
	"github.com/DeluxeOwl/chronicle/aggregate"
	"github.com/DeluxeOwl/chronicle/event"
)

func NewAggregateRepository[TID aggregate.ID, E event.Any, R aggregate.Root[TID, E]](
	eventLog event.Log,
	newRoot func() R,
	opts ...aggregate.RepoOption[TID, E, R],
) (*aggregate.Repo[TID, E, R], error) {
	return aggregate.NewRepo(eventLog, newRoot, opts...)
}
