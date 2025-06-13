package aggregate

import (
	"fmt"

	"github.com/DeluxeOwl/chronicle/event"
)

type DataIntegrityError[TExpectedEvent event.Any] struct {
	Event event.Any
}

func (d *DataIntegrityError[TExpectedEvent]) Error() string {
	var zeroEvent TExpectedEvent
	return fmt.Sprintf(
		"data integrity error: loaded event of type %T but aggregate expects type %T",
		d.Event, zeroEvent,
	)
}
