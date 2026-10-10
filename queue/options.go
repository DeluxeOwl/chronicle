package queue

import (
	"time"

	"github.com/DeluxeOwl/chronicle/aggregate"
	"github.com/DeluxeOwl/chronicle/encoding"
)

type options struct {
	nowFunc          func() time.Time
	codec            encoding.Codec
	snapshotStore    aggregate.SnapshotStore[ID, *Snapshot]
	snapshotEvery    uint64
	snapshotsEnabled bool
	payloadFactory   any
}

// Option configures a persistent queue.
type Option func(*options)

// WithSnapshots reduces replay cost by saving queue state every n committed
// events. A nil store or zero interval is invalid. Snapshot save errors do not
// fail committed queue commands; event history remains the source of truth.
func WithSnapshots(store aggregate.SnapshotStore[ID, *Snapshot], n uint64) Option {
	return func(opts *options) {
		opts.snapshotsEnabled = true
		opts.snapshotStore = store
		opts.snapshotEvery = n
	}
}

// WithPayloadFactory allocates pointer payloads before decoding. The factory
// must return the queue's exact payload type and a non-nil initialized pointer.
// It is required for the built-in Protobuf codec, including dynamic messages.
// The factory must be safe for concurrent calls and return a fresh value each time.
func WithPayloadFactory[T any](factory func() T) Option {
	return func(opts *options) { opts.payloadFactory = factory }
}

// WithCodec sets the payload codec. Queue events themselves remain JSON encoded.
// The codec must be safe for concurrent calls and produce deterministic bytes
// for idempotent enqueue. Every instance of a named queue must use the same codec.
// The built-in Protobuf codec requires WithPayloadFactory and a message pointer.
// Without a factory, decoders receive *T, including **Value for pointer payloads.
func WithCodec(codec encoding.Codec) Option {
	return func(opts *options) { opts.codec = codec }
}

// WithNowFunc sets the clock used to decide eligibility and lease ownership.
// All workers sharing a queue must use synchronized clocks. The function must
// be safe for concurrent calls; it is never called during event replay.
func WithNowFunc(fn func() time.Time) Option {
	return func(opts *options) { opts.nowFunc = fn }
}
