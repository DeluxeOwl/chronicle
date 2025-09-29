package eventlog

import (
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/DeluxeOwl/chronicle/event"
	"github.com/DeluxeOwl/chronicle/version"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/synadia-io/orbit.go/jetstreamext"
)

var (
	_ event.Log                                        = (*NATS)(nil)
	_ event.TransactionalEventLog[jetstream.JetStream] = (*NATS)(nil)
)

const (
	// Custom headers to store the per-aggregate version number.
	headerAggregateVersion = "Chronicle-Aggregate-Version"
	headerEventName        = "Chronicle-Event-Name"
)

// NATS provides an event.Log implementation using NATS JetStream as the backing store.
//
// It uses a "one stream per aggregate" model:
//
//   - Each event.LogID has its own dedicated JetStream stream.
//   - Each stream has exactly one subject (the aggregate's subject).
//   - The aggregate's version is equal to the JetStream sequence for that subject.
//
// This makes optimistic concurrency straightforward:
//
//   - expected aggregate version == expected last sequence per subject.
//
// Single-event appends use standard JetStream publish.
// Multi-event appends use ADR-50 atomic batch publishing via orbit.go.
type NATS struct {
	nc *nats.Conn
	js jetstream.JetStream

	// Prefixes used to generate stream and subject names from an event.LogID.
	// For an ID "order/123" and defaults:
	//   stream  -> "chronicle_events.order_123"
	//   subject -> "chronicle.events.order_123"
	streamPrefix  string
	subjectPrefix string

	// Configuration for dynamically created per-aggregate streams.
	storageType jetstream.StorageType
	retention   jetstream.RetentionPolicy

	// Timeout for reads when pulling messages.
	readTimeout time.Duration
}

// NATSOption defines a function that configures a NATS event log instance.
type NATSOption func(*NATS)

// WithNATSStreamName sets the *prefix* used when generating per-aggregate stream names.
// The actual stream name for a given aggregate will be "<name>.<sanitized-id>".
// If not provided, it defaults to "chronicle_events".
func WithNATSStreamName(name string) NATSOption {
	return func(n *NATS) {
		n.streamPrefix = name
	}
}

// WithNATSSubjectPrefix sets the prefix used for all event subjects.
// For an aggregate with ID "order/123", the subject will be "<prefix>.order_123".
// If not provided, it defaults to "chronicle.events".
func WithNATSSubjectPrefix(prefix string) NATSOption {
	return func(n *NATS) {
		n.subjectPrefix = prefix
	}
}

// WithNATSStorage sets the storage backend (e.g., File or Memory) for the JetStream streams.
// Defaults to jetstream.FileStorage for durability.
func WithNATSStorage(storage jetstream.StorageType) NATSOption {
	return func(n *NATS) {
		n.storageType = storage
	}
}

// WithNATSRetentionPolicy sets the retention policy for the JetStream streams (e.g., Limits, Interest).
// Defaults to jetstream.LimitsPolicy.
func WithNATSRetentionPolicy(policy jetstream.RetentionPolicy) NATSOption {
	return func(n *NATS) {
		n.retention = policy
	}
}

// WithReadTimeOut sets the read wait duration during events fetching from streams.
// Defaults to 500 milliseconds.
func WithReadTimeOut(t time.Duration) NATSOption {
	return func(n *NATS) {
		n.readTimeout = t
	}
}

// NewNATSJetStream creates a new NATS event log instance.
// It requires a connected `*nats.Conn` and accepts functional options for configuration.
//
// Streams are not created up front; they are created lazily when events
// are first appended for a given aggregate.
func NewNATSJetStream(nc *nats.Conn, opts ...NATSOption) (*NATS, error) {
	if nc == nil {
		return nil, errors.New("nats connection cannot be nil")
	}

	js, err := jetstream.New(nc)
	if err != nil {
		return nil, fmt.Errorf("failed to create jetstream context: %w", err)
	}

	njs := &NATS{
		nc:            nc,
		js:            js,
		streamPrefix:  "chronicle_events",
		subjectPrefix: "chronicle.events",
		storageType:   jetstream.FileStorage,
		retention:     jetstream.LimitsPolicy,
		readTimeout:   500 * time.Millisecond,
	}

	for _, opt := range opts {
		opt(njs)
	}

	return njs, nil
}

// AppendEvents appends one or more events for a given aggregate ID.
// It performs an optimistic concurrency check based on the `expected` version.
//
// For a single event, it uses a simple JetStream publish.
// For multiple events, it uses ADR-50 atomic batch publishing via orbit.go.
func (c *NATS) AppendEvents(
	ctx context.Context,
	id event.LogID,
	expected version.Check,
	events event.RawEvents,
) (version.Version, error) {
	if len(events) == 0 {
		return version.Zero, ErrNoEvents
	}

	exp, ok := expected.(version.CheckExact)
	if !ok {
		return version.Zero, ErrUnsupportedCheck
	}

	if len(events) == 1 {
		return c.appendSingleEvent(ctx, id, exp, events[0])
	}

	var newVersion version.Version
	err := c.WithinTx(ctx, func(txCtx context.Context, tx jetstream.JetStream) error {
		v, _, err := c.AppendInTx(txCtx, tx, id, exp, events)
		if err != nil {
			return err
		}
		newVersion = v
		return nil
	})
	if err != nil {
		return version.Zero, err
	}

	return newVersion, nil
}

// ReadEvents returns an iterator for the event history of a single aggregate.
//
// It reads from the aggregate's dedicated stream by creating an ephemeral
// pull consumer filtered to the aggregate's subject.
//
// The selector's From/To versions map directly to JetStream sequence numbers
// in the per-aggregate stream.
func (c *NATS) ReadEvents(
	ctx context.Context,
	id event.LogID,
	selector version.Selector,
) event.Records {
	return func(yield func(*event.Record, error) bool) {
		streamName := c.logIDToStreamName(id)
		subject := c.logIDToSubject(id)

		stream, err := c.js.Stream(ctx, streamName)
		if err != nil {
			// If the stream doesn't exist, it means no events have ever been written
			// for this aggregate, which is a normal condition, not an error.
			if errors.Is(err, jetstream.ErrStreamNotFound) {
				return
			}
			yield(nil, fmt.Errorf("read events: stream %q not reachable: %w", streamName, err))
			return
		}

		consumerCfg := jetstream.ConsumerConfig{
			FilterSubject:     subject,
			AckPolicy:         jetstream.AckExplicitPolicy,
			DeliverPolicy:     jetstream.DeliverByStartSequencePolicy,
			OptStartSeq:       uint64(selector.From),
			InactiveThreshold: 5 * time.Minute,
		}

		// A start sequence of 0 is invalid for DeliverByStartSequencePolicy,
		// so we switch to DeliverAllPolicy in that case.
		if selector.From == 0 {
			consumerCfg.DeliverPolicy = jetstream.DeliverAllPolicy
		}

		consumer, err := stream.CreateOrUpdateConsumer(ctx, consumerCfg)
		if err != nil {
			yield(nil, fmt.Errorf("read events: create consumer: %w", err))
			return
		}

		iter, err := consumer.Messages()
		if err != nil {
			yield(nil, fmt.Errorf("read events: get messages: %w", err))
			return
		}
		defer iter.Stop()

		for {
			msg, err := iter.Next(jetstream.NextMaxWait(c.readTimeout))
			if err != nil {
				if errors.Is(err, jetstream.ErrMsgIteratorClosed) {
					return
				}
				if errors.Is(err, nats.ErrTimeout) {
					// No more messages currently available.
					return
				}
				yield(nil, fmt.Errorf("read events: iterate messages: %w", err))
				return
			}

			record, err := c.natsMsgToRecord(msg, id)
			if err != nil {
				yield(nil, fmt.Errorf("read events: failed to convert message: %w", err))
				return
			}

			if selector.To != 0 && record.Version() > selector.To {
				return
			}

			if !yield(record, nil) {
				return
			}

			if err := msg.Ack(); err != nil {
				yield(nil, fmt.Errorf("read events: acknowledge message: %w", err))
				return
			}
		}
	}
}

// AppendInTx appends a batch of events atomically using JetStream's
// atomic batch publish feature via orbit.go's jetstreamext.BatchPublisher.
//
// The optimistic concurrency check is done using the expected aggregate
// version, mapped to JetStream's "expected last sequence per subject".
func (c *NATS) AppendInTx(
	ctx context.Context,
	_ jetstream.JetStream, // Included to satisfy TransactionalEventLog, not used here.
	id event.LogID,
	expected version.Check,
	events event.RawEvents,
) (version.Version, []*event.Record, error) {
	if len(events) == 0 {
		return version.Zero, nil, ErrNoEvents
	}

	exp, ok := expected.(version.CheckExact)
	if !ok {
		return version.Zero, nil, fmt.Errorf("append in tx: %w", ErrUnsupportedCheck)
	}

	streamName := c.logIDToStreamName(id)
	subject := c.logIDToSubject(id)
	startingVersion := version.Version(exp)

	if err := c.ensureStream(ctx, streamName, subject); err != nil {
		return version.Zero, nil, fmt.Errorf("append in tx: ensure stream: %w", err)
	}

	msgs, err := convertRawEventsToNatsMsgs(subject, events, startingVersion)
	if err != nil {
		return version.Zero, nil, fmt.Errorf("append in tx: prepare messages: %w", err)
	}

	// If somehow only one event gets here, fall back to the single-event path.
	if len(msgs) == 1 {
		v, err := c.appendSingleEvent(ctx, id, exp, events[0])
		if err != nil {
			return version.Zero, nil, err
		}
		record := event.NewRecord(v, id, events[0].EventName(), events[0].Data())
		return v, []*event.Record{record}, nil
	}

	batch, err := jetstreamext.NewBatchPublisher(c.js)
	if err != nil {
		return version.Zero, nil, fmt.Errorf("append in tx: create batch publisher: %w", err)
	}

	// Ensure incomplete batches are abandoned explicitly.
	defer func() {
		if !batch.IsClosed() {
			_ = batch.Discard()
		}
	}()

	// Add all but the final message to the batch.
	for i := 0; i < len(msgs)-1; i++ {
		msg := msgs[i]
		opts := []jetstreamext.BatchMsgOpt(nil)
		if i == 0 {
			// Apply optimistic concurrency check to the first message.
			opts = append(opts,
				jetstreamext.WithBatchExpectLastSequencePerSubject(uint64(exp)),
			)
		}

		if err := batch.AddMsg(msg, opts...); err != nil {
			if actualVersion, ok := parseActualVersionFromError(err); ok {
				return version.Zero, nil, version.NewConflictError(version.Version(exp), actualVersion)
			}
			return version.Zero, nil, fmt.Errorf("append in tx: add message %d to batch: %w", i+1, err)
		}
	}

	// Commit the batch with the final message.
	lastMsg := msgs[len(msgs)-1]
	if _, err := batch.CommitMsg(ctx, lastMsg); err != nil {
		if actualVersion, ok := parseActualVersionFromError(err); ok {
			return version.Zero, nil, version.NewConflictError(version.Version(exp), actualVersion)
		}
		return version.Zero, nil, fmt.Errorf("append in tx: commit batch: %w", err)
	}

	// On success, the new version is the expected version plus the number of events appended.
	finalVersion, err := addToVersion(startingVersion, int64(len(events)))
	if err != nil {
		return version.Zero, nil, fmt.Errorf("append in tx: compute final version: %w", err)
	}

	records := make([]*event.Record, len(events))
	for i, raw := range events {
		recordVersion, err := addToVersion(startingVersion, int64(i+1))
		if err != nil {
			return version.Zero, nil, fmt.Errorf("append in tx: compute record version: %w", err)
		}
		records[i] = event.NewRecord(
			recordVersion,
			id,
			raw.EventName(),
			raw.Data(),
		)
	}

	return finalVersion, records, nil
}

// WithinTx provides a transactional boundary for event log operations.
//
// For NATS, single publish and batch publish operations are already atomic,
// so this function simply executes the provided function with the shared
// JetStream context.
func (c *NATS) WithinTx(
	ctx context.Context,
	fn func(ctx context.Context, tx jetstream.JetStream) error,
) error {
	return fn(ctx, c.js)
}

// ensureStream guarantees that a per-aggregate stream exists.
//
// It creates a new stream with:
//   - Name:    streamName
//   - Subject: subject (single subject per stream)
//   - Storage, retention from configuration
//   - AllowAtomicPublish enabled
//
// If the stream already exists, it is returned as-is. We assume only this
// component creates/manages these per-aggregate streams.
func (c *NATS) ensureStream(
	ctx context.Context,
	streamName, subject string,
) error {
	cfg := jetstream.StreamConfig{
		Name:               streamName,
		Subjects:           []string{subject},
		Storage:            c.storageType,
		Retention:          c.retention,
		AllowAtomicPublish: true,
	}

	_, err := c.js.Stream(ctx, streamName)
	if err == nil {
		return nil
	}
	if !errors.Is(err, jetstream.ErrStreamNotFound) {
		return fmt.Errorf("ensure stream %q: get stream: %w", streamName, err)
	}

	_, err = c.js.CreateStream(ctx, cfg)
	if err != nil {
		return fmt.Errorf("ensure stream %q: create: %w", streamName, err)
	}
	return nil
}

// convertRawEventsToNatsMsgs transforms a slice of Chronicle's RawEvents into a slice
// of `*nats.Msg` suitable for publishing. It embeds the aggregate version and event
// name into the headers of each message.
//
// startingVersion is the aggregate version *before* applying this batch; the first
// message will get version startingVersion+1, etc.
func convertRawEventsToNatsMsgs(
	subject string,
	events event.RawEvents,
	startingVersion version.Version,
) ([]*nats.Msg, error) {
	msgs := make([]*nats.Msg, len(events))

	for i, rawEvent := range events {
		eventVersion, err := addToVersion(startingVersion, int64(i+1))
		if err != nil {
			return nil, fmt.Errorf("compute event version: %w", err)
		}

		msgs[i] = &nats.Msg{
			Subject: subject,
			Data:    rawEvent.Data(),
			Header: nats.Header{
				headerEventName:        []string{rawEvent.EventName()},
				headerAggregateVersion: []string{strconv.FormatUint(uint64(eventVersion), 10)},
			},
		}
	}

	return msgs, nil
}

// appendSingleEvent is an optimized path for writing a single event to a stream.
// It uses the standard JetStream PublishMsg with an optimistic concurrency check.
func (c *NATS) appendSingleEvent(
	ctx context.Context,
	id event.LogID,
	expected version.CheckExact,
	rawEvent event.Raw,
) (version.Version, error) {
	streamName := c.logIDToStreamName(id)
	subject := c.logIDToSubject(id)

	if err := c.ensureStream(ctx, streamName, subject); err != nil {
		return version.Zero, fmt.Errorf("append single event: ensure stream: %w", err)
	}

	newEventVersion := version.Version(expected) + 1

	msg := &nats.Msg{
		Subject: subject,
		Data:    rawEvent.Data(),
		Header: nats.Header{
			headerEventName:        []string{rawEvent.EventName()},
			headerAggregateVersion: []string{strconv.FormatUint(uint64(newEventVersion), 10)},
		},
	}

	pubOpts := []jetstream.PublishOpt{
		jetstream.WithExpectLastSequencePerSubject(uint64(expected)),
	}

	if _, err := c.js.PublishMsg(ctx, msg, pubOpts...); err != nil {
		if actualVersion, ok := parseActualVersionFromError(err); ok {
			return version.Zero, version.NewConflictError(version.Version(expected), actualVersion)
		}
		return version.Zero, fmt.Errorf("append single event: publish: %w", err)
	}

	return newEventVersion, nil
}

// wrongLastSeqRegexp is designed to find one or more digits at the very end of the string.
var wrongLastSeqRegexp = regexp.MustCompile(`(\d+)$`)

// parseActualVersionFromError inspects an error to see if it's a JetStream
// "wrong last sequence" conflict (ErrorCode 10071). If it is, it safely parses the
// actual version from the error's description string and returns it.
func parseActualVersionFromError(err error) (version.Version, bool) {
	var apiErr *jetstream.APIError

	if !errors.As(err, &apiErr) {
		return version.Zero, false
	}
	if apiErr.ErrorCode != jetstream.JSErrCodeStreamWrongLastSequence {
		return version.Zero, false
	}

	matches := wrongLastSeqRegexp.FindStringSubmatch(apiErr.Description)
	if len(matches) <= 1 {
		return version.Zero, false
	}

	actual, parseErr := strconv.ParseUint(matches[1], 10, 64)
	if parseErr != nil {
		return version.Zero, false
	}

	return version.Version(actual), true
}

// natsMsgToRecord converts a received `jetstream.Msg` into a `*event.Record`.
// It extracts the necessary metadata (version, event name) from the message headers.
func (c *NATS) natsMsgToRecord(msg jetstream.Msg, id event.LogID) (*event.Record, error) {
	eventName := msg.Headers().Get(headerEventName)
	if eventName == "" {
		return nil, errors.New("message is missing event name header")
	}

	versionStr := msg.Headers().Get(headerAggregateVersion)
	if versionStr == "" {
		return nil, errors.New("message is missing aggregate version header")
	}

	eventVersionValue, err := strconv.ParseUint(versionStr, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("failed to parse event version: %w", err)
	}

	return event.NewRecord(
		version.Version(eventVersionValue),
		id,
		eventName,
		msg.Data(),
	), nil
}

// logIDToStreamName converts a LogID into a per-aggregate JetStream stream name.
// e.g., "order/123" -> "chronicle_events.order_123"
func (c *NATS) logIDToStreamName(id event.LogID) string {
	safeID := sanitizeID(string(id))
	// JetStream names cannot contain '.', so use '_' as separator.
	return fmt.Sprintf("%s_%s", c.streamPrefix, safeID)
}

// logIDToSubject converts a LogID into a NATS subject for a specific instance.
// e.g., "order/123" -> "chronicle.events.order_123"
func (c *NATS) logIDToSubject(id event.LogID) string {
	safeID := sanitizeID(string(id))
	return fmt.Sprintf("%s.%s", c.subjectPrefix, safeID)
}

// sanitizeID makes an aggregate ID safe for use in stream and subject names.
func sanitizeID(id string) string {
	// Replace characters that are problematic for stream/subject names.
	s := strings.ReplaceAll(id, "/", "_")
	s = strings.ReplaceAll(s, "\\", "_")
	s = strings.ReplaceAll(s, " ", "_")
	s = strings.ReplaceAll(s, ".", "_")
	return s
}

func addToVersion(base version.Version, offset int64) (version.Version, error) {
	if offset < 0 {
		return version.Zero, fmt.Errorf("negative offset: %d", offset)
	}

	offsetU64 := uint64(offset)
	if offsetU64 > math.MaxUint64-uint64(base) {
		return version.Zero, fmt.Errorf("version overflow: base %d offset %d", base, offset)
	}

	return base + version.Version(offsetU64), nil
}
