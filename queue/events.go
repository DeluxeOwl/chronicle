package queue

import (
	"time"

	"github.com/DeluxeOwl/chronicle/event"
)

//sumtype:decl
type queueEvent interface {
	event.Any
	isQueueEvent()
}

type messageEnqueued struct {
	ID          string    `json:"id"`
	Payload     []byte    `json:"payload"`
	AvailableAt time.Time `json:"availableAt"`
}

func (*messageEnqueued) EventName() string { return "queue/message_enqueued" }
func (*messageEnqueued) isQueueEvent()     {}

type messageClaimed struct {
	ID         string    `json:"id"`
	Receipt    Receipt   `json:"receipt"`
	LeaseUntil time.Time `json:"leaseUntil"`
}

func (*messageClaimed) EventName() string { return "queue/message_claimed" }
func (*messageClaimed) isQueueEvent()     {}

type messageAcknowledged struct {
	ID string `json:"id"`
}

func (*messageAcknowledged) EventName() string { return "queue/message_acknowledged" }
func (*messageAcknowledged) isQueueEvent()     {}

type messageRescheduled struct {
	ID          string    `json:"id"`
	AvailableAt time.Time `json:"availableAt"`
}

func (*messageRescheduled) EventName() string { return "queue/message_rescheduled" }
func (*messageRescheduled) isQueueEvent()     {}

type messageLeaseExtended struct {
	ID         string    `json:"id"`
	LeaseUntil time.Time `json:"leaseUntil"`
}

func (*messageLeaseExtended) EventName() string { return "queue/message_lease_extended" }
func (*messageLeaseExtended) isQueueEvent()     {}
