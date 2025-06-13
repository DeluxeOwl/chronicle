package chronicle

import "github.com/DeluxeOwl/chronicle/event/eventlog"

func NewEventLogMemory() *eventlog.Memory {
	return eventlog.NewMemory()
}
