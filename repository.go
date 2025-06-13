package chronicle

import (
	"github.com/DeluxeOwl/chronicle/aggregate"
	"github.com/DeluxeOwl/chronicle/event"
)

func NewRepository[TID aggregate.ID, E event.Any, R aggregate.Root[TID, E]](
	eventLog event.Log,
	newRoot func() R,
	opts ...aggregate.RepositoryOption[TID, E, R],
) (*aggregate.Repository[TID, E, R], error) {
	return aggregate.NewRepository(eventLog, newRoot, opts...)
}
