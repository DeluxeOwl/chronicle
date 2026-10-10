package queue

import (
	"crypto/sha256"
	"fmt"
	"slices"
	"time"

	"github.com/DeluxeOwl/chronicle/aggregate"
	"github.com/DeluxeOwl/chronicle/event"
)

type state struct {
	aggregate.Base
	id       ID
	messages map[string]*pendingMessage
	order    []string
	receipts map[Receipt]string
	seen     map[string]enqueueFingerprint
}

type enqueueFingerprint struct {
	payloadHash [sha256.Size]byte
	availableAt time.Time
}

type pendingMessage struct {
	id          string
	payload     []byte
	availableAt time.Time
	receipt     Receipt
	leaseUntil  time.Time
	attempt     uint64
}

func newState(id ID) *state {
	return &state{
		Base: aggregate.Base{}, id: id, messages: make(map[string]*pendingMessage),
		order: nil, receipts: make(map[Receipt]string), seen: make(map[string]enqueueFingerprint),
	}
}

func (s *state) ID() ID { return s.id }

func (s *state) EventFuncs() event.FuncsFor[queueEvent] {
	return event.FuncsFor[queueEvent]{
		func() queueEvent { return new(messageEnqueued) },
		func() queueEvent { return new(messageClaimed) },
		func() queueEvent { return new(messageAcknowledged) },
		func() queueEvent { return new(messageRescheduled) },
		func() queueEvent { return new(messageLeaseExtended) },
	}
}

func (s *state) Apply(evt queueEvent) error {
	switch e := evt.(type) {
	case *messageEnqueued:
		s.seen[e.ID] = enqueueFingerprint{payloadHash: sha256.Sum256(e.Payload), availableAt: e.AvailableAt}
		s.messages[e.ID] = &pendingMessage{
			id: e.ID, payload: e.Payload, availableAt: e.AvailableAt,
			receipt: "", leaseUntil: time.Time{}, attempt: 0,
		}
		s.order = append(s.order, e.ID)
	case *messageClaimed:
		msg := s.messages[e.ID]
		delete(s.receipts, msg.receipt)
		msg.receipt = e.Receipt
		msg.leaseUntil = e.LeaseUntil
		msg.attempt++
		s.receipts[e.Receipt] = e.ID
	case *messageAcknowledged:
		delete(s.receipts, s.messages[e.ID].receipt)
		delete(s.messages, e.ID)
		index := slices.Index(s.order, e.ID)
		s.order = slices.Delete(s.order, index, index+1)
	case *messageRescheduled:
		msg := s.messages[e.ID]
		delete(s.receipts, msg.receipt)
		msg.availableAt = e.AvailableAt
		msg.receipt = ""
		msg.leaseUntil = time.Time{}
	case *messageLeaseExtended:
		s.messages[e.ID].leaseUntil = e.LeaseUntil
	default:
		return fmt.Errorf("unexpected queue event: %T", evt)
	}
	return nil
}

func (s *state) next(now time.Time) *pendingMessage {
	for _, id := range s.order {
		msg := s.messages[id]
		if !msg.availableAt.After(now) && !msg.leaseUntil.After(now) {
			return msg
		}
	}
	return nil
}

func (s *state) leasedMessage(receipt Receipt, now time.Time) (*pendingMessage, error) {
	id, ok := s.receipts[receipt]
	if !ok {
		return nil, ErrLeaseLost
	}
	msg := s.messages[id]
	if !now.Before(msg.leaseUntil) {
		return nil, ErrLeaseLost
	}
	return msg, nil
}
