// Package queue provides an at-least-once work queue backed by a Chronicle event log.
// Durability follows the supplied log. Handlers must make external effects
// idempotent because a crash between processing and acknowledgment can redeliver work.
package queue

import (
	"context"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/DeluxeOwl/chronicle/aggregate"
	"github.com/DeluxeOwl/chronicle/encoding"
	"github.com/DeluxeOwl/chronicle/event"
	"github.com/DeluxeOwl/chronicle/version"
)

var (
	// ErrLeaseLost means the receipt has no current, unexpired lease.
	ErrLeaseLost = errors.New("queue lease lost")
	// ErrMessageExists means an enqueue ID already has different contents.
	ErrMessageExists = errors.New("queue message ID already used with different contents")
)

// Queue is the public seam for durable delivery. Claims skip scheduled messages
// and active leases; execution and completion order are not strict FIFO.
type Queue[T any] interface {
	Enqueue(ctx context.Context, msg Message[T]) error
	Claim(ctx context.Context, lease time.Duration) (*Delivery[T], error)
	Ack(ctx context.Context, receipt Receipt) error
	Nack(ctx context.Context, receipt Receipt, retryAt time.Time) error
	Extend(ctx context.Context, receipt Receipt, lease time.Duration) error
}

var _ Queue[any] = (*Persistent[any])(nil)

// Receipt identifies a particular delivery attempt. It must be passed unchanged
// to Ack, Nack, or Extend; a message ID is not a receipt.
type Receipt string

// Message is a unit of work. IDs must be nonempty valid UTF-8 strings and unique
// within a named queue.
type Message[T any] struct {
	ID          string
	Payload     T
	AvailableAt time.Time `exhaustruct:"optional"`
}

// Delivery is a message whose claim has been persisted successfully.
type Delivery[T any] struct {
	Message    Message[T]
	Receipt    Receipt
	Attempt    uint64
	LeaseUntil time.Time
}

// Persistent stores a named queue in a single event stream. Multiple instances
// using the same log and name share the queue; the log determines durability.
// Methods are safe for concurrent calls if the configured clock, codec, and log are.
type Persistent[T any] struct {
	repo           aggregate.Repository[ID, queueEvent, *state]
	id             ID
	codec          encoding.Codec
	nowFunc        func() time.Time
	payloadFactory func() T
}

// New opens a named queue without writing an event. Payloads use JSON by default.
// All instances of a named queue must use the same payload type and codec.
func New[T any](log event.Log, name string, opts ...Option) (*Persistent[T], error) {
	if log == nil {
		return nil, errors.New("queue event log cannot be nil")
	}
	if strings.TrimSpace(name) == "" {
		return nil, errors.New("queue name cannot be empty")
	}

	config := options{
		nowFunc:          time.Now,
		codec:            encoding.DefaultJSONB,
		snapshotStore:    nil,
		snapshotEvery:    0,
		snapshotsEnabled: false,
		payloadFactory:   nil,
	}
	for _, opt := range opts {
		if opt == nil {
			return nil, errors.New("queue option cannot be nil")
		}
		opt(&config)
	}
	if config.nowFunc == nil {
		return nil, errors.New("queue clock cannot be nil")
	}
	if config.codec == nil {
		return nil, errors.New("queue payload codec cannot be nil")
	}

	factory, err := payloadFactoryFor[T](config)
	if err != nil {
		return nil, fmt.Errorf("new queue: %w", err)
	}

	id := ID("queue_" + base64.RawURLEncoding.EncodeToString([]byte(name)))
	repo, err := aggregate.NewESRepo(log, func() *state { return newState(id) }, nil,
		aggregate.AnyEventRegistry(event.NewRegistry[event.Any]()))
	if err != nil {
		return nil, fmt.Errorf("new queue: %w", err)
	}
	configured, err := configureSnapshots(repo, config)
	if err != nil {
		return nil, fmt.Errorf("new queue: %w", err)
	}
	return &Persistent[T]{
		repo:           configured,
		id:             id,
		codec:          config.codec,
		nowFunc:        config.nowFunc,
		payloadFactory: factory,
	}, nil
}

// Enqueue persists a message. A zero AvailableAt makes it immediately eligible.
// Retrying the same ID, encoded payload, and AvailableAt is a no-op, even after
// acknowledgment. Reusing an ID with different contents returns ErrMessageExists.
func (q *Persistent[T]) Enqueue(ctx context.Context, msg Message[T]) error {
	if msg.ID == "" || !utf8.ValidString(msg.ID) {
		return errors.New("queue message ID must be nonempty valid UTF-8")
	}
	payload, err := q.codec.Encode(msg.Payload)
	if err != nil {
		return fmt.Errorf("queue encode payload: %w", err)
	}
	return q.update(ctx, func(root *state) error {
		if previous, exists := root.seen[msg.ID]; exists {
			if previous.payloadHash != sha256.Sum256(payload) ||
				!previous.availableAt.Equal(msg.AvailableAt) {
				return ErrMessageExists
			}
			return nil
		}
		return aggregate.RecordEvent(root, queueEvent(&messageEnqueued{
			ID: msg.ID, Payload: payload, AvailableAt: msg.AvailableAt,
		}))
	})
}

// Claim leases the earliest enqueued eligible message. It returns nil, nil when
// no message is eligible. An expired lease makes a message eligible again.
func (q *Persistent[T]) Claim(ctx context.Context, lease time.Duration) (*Delivery[T], error) {
	if lease <= 0 {
		return nil, errors.New("queue lease must be positive")
	}

	var delivery *Delivery[T]
	err := q.update(ctx, func(root *state) error {
		delivery = nil
		now := q.nowFunc()
		msg := root.next(now)
		if msg == nil {
			return nil
		}
		payload, err := decodePayload(q.codec, q.payloadFactory, msg.payload)
		if err != nil {
			return fmt.Errorf("queue decode payload: %w", err)
		}
		receipt := Receipt(cryptorand.Text())
		deadline := now.Add(lease)
		if err := aggregate.RecordEvent(root, queueEvent(&messageClaimed{
			ID: msg.id, Receipt: receipt, LeaseUntil: deadline,
		})); err != nil {
			return err
		}
		delivery = &Delivery[T]{
			Message: Message[T]{ID: msg.id, Payload: payload, AvailableAt: msg.availableAt},
			Receipt: receipt, Attempt: msg.attempt, LeaseUntil: deadline,
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("queue claim: %w", err)
	}
	return delivery, nil
}

// Ack removes a message only if the receipt still owns its unexpired lease.
func (q *Persistent[T]) Ack(ctx context.Context, receipt Receipt) error {
	return q.update(ctx, func(root *state) error {
		msg, err := root.leasedMessage(receipt, q.nowFunc())
		if err != nil {
			return err
		}
		return aggregate.RecordEvent(root, queueEvent(&messageAcknowledged{ID: msg.id}))
	})
}

// Nack releases the current lease and schedules another attempt. A zero or past
// retryAt makes the message immediately eligible. The old receipt is invalidated.
func (q *Persistent[T]) Nack(ctx context.Context, receipt Receipt, retryAt time.Time) error {
	return q.update(ctx, func(root *state) error {
		msg, err := root.leasedMessage(receipt, q.nowFunc())
		if err != nil {
			return err
		}
		return aggregate.RecordEvent(
			root,
			queueEvent(&messageRescheduled{ID: msg.id, AvailableAt: retryAt}),
		)
	})
}

// Extend keeps the current receipt valid until at least now + lease. It never
// shortens an existing lease and cannot revive an expired or superseded receipt.
func (q *Persistent[T]) Extend(ctx context.Context, receipt Receipt, lease time.Duration) error {
	if lease <= 0 {
		return errors.New("queue lease must be positive")
	}
	return q.update(ctx, func(root *state) error {
		now := q.nowFunc()
		msg, err := root.leasedMessage(receipt, now)
		if err != nil {
			return err
		}
		deadline := now.Add(lease)
		if !deadline.After(msg.leaseUntil) {
			return nil
		}
		return aggregate.RecordEvent(
			root,
			queueEvent(&messageLeaseExtended{ID: msg.id, LeaseUntil: deadline}),
		)
	})
}

const maxConflictRetries = 16

func (q *Persistent[T]) update(ctx context.Context, command func(*state) error) error {
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		root, err := q.repo.Get(ctx, q.id)
		if errors.Is(err, aggregate.ErrRootNotFound) {
			root = newState(q.id)
		} else if err != nil {
			return fmt.Errorf("queue load: %w", err)
		}
		if err := command(root); err != nil {
			return err
		}
		if _, _, err = q.repo.Save(ctx, root); err == nil {
			return nil
		}
		var conflict *version.ConflictError
		if !errors.As(err, &conflict) || attempt >= maxConflictRetries {
			return fmt.Errorf("queue save: %w", err)
		}
		// Save flushes the root's pending events, so retry the command on fresh
		// state. Reusing the root could report success without persisting a claim.
		if err := waitForRetry(ctx, attempt); err != nil {
			return err
		}
	}
}

func waitForRetry(ctx context.Context, attempt int) error {
	ceiling := time.Millisecond << min(attempt, 6)
	//nolint:gosec // Retry jitter does not require cryptographic randomness.
	jitter := time.Duration(rand.Int64N(int64(ceiling)))
	timer := time.NewTimer(ceiling + jitter)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
