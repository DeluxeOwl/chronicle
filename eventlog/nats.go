package eventlog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/DeluxeOwl/chronicle/event"
	"github.com/DeluxeOwl/chronicle/version"
	"github.com/gofrs/uuid/v5"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

var (
	_ event.Log                                        = (*NATS)(nil)
	_ event.TransactionalEventLog[jetstream.JetStream] = (*NATS)(nil)
)

const (
	// See: https://github.com/nats-io/nats-architecture-and-design/blob/main/adr/ADR-50.md
	natsBatchIDHeader     = "Nats-Batch-Id"
	natsBatchSeqHeader    = "Nats-Batch-Sequence"
	natsBatchCommitHeader = "Nats-Batch-Commit"

	// Custom headers to store the per-aggregate version number.
	headerAggregateVersion = "Chronicle-Aggregate-Version"
	headerEventName        = "Chronicle-Event-Name"

	natsExpectedLastSubjectSeqHeader = "Nats-Expected-Last-Subject-Sequence"
)

// Temporary struct to unmarshal PubAck with batch info (NATS server 2.12), since the official
// jetstream.PubAck does not include batch fields yet.
// TODO: Remove this struct once the official client includes these fields.
type pubAck struct {
	jetstream.PubAck
	BatchId   string             `json:"batch"`
	BatchSize int                `json:"count"`
	Error     jetstream.APIError `json:"error"`
}

// NATS provides an event.Log implementation using NATS Jetstream as the backing store.
//
// It employs a "one stream per aggregate" model. Each unique event.LogID is
// mapped to its own dedicated Jetstream stream. This design choice ensures that the
// Jetstream sequence numbers for a stream are always synchronized with the aggregate's
// version, which allows for simple and reliable optimistic concurrency control.
//
// For appending single events (the common case), it uses a highly efficient, standard
// publish operation. For appending multiple events atomically, it uses the newer
// atomic batch publish feature (ADR-50).
type NATS struct {
	nc *nats.Conn
	js jetstream.JetStream

	// Configuration for dynamically created streams.
	storageType jetstream.StorageType
	retention   jetstream.RetentionPolicy

	// Prefixes used to generate stream and subject names from an event.LogID.
	streamPrefix  string
	subjectPrefix string
}

// NATSOption defines a function that configures a NATS event log instance.
type NATSOption func(*NATS)

// WithNATSStreamName is a configuration option that sets the name of the stream
// used to store events. If not provided, it defaults to "chronicle_events".
func WithNATSStreamName(name string) NATSOption {
	return func(n *NATS) {
		n.streamPrefix = name
	}
}

// WithNATSSubjectPrefix sets the prefix used for all event subjects.
// For an aggregate with ID "order/123", the subject would be "<prefix>.order_123".
// If not provided, it defaults to "chronicle.events".
func WithNATSSubjectPrefix(prefix string) NATSOption {
	return func(n *NATS) {
		n.subjectPrefix = prefix
	}
}

// WithNATSStorage sets the storage backend (e.g., File or Memory) for the Jetstream streams.
// Defaults to jetstream.FileStorage for durability.
func WithNATSStorage(storage jetstream.StorageType) NATSOption {
	return func(n *NATS) {
		n.storageType = storage
	}
}

// WithNATSRetentionPolicy sets the retention policy for the Jetstream streams (e.g., Limits, Interest).
// Defaults to jetstream.LimitsPolicy.
func WithNATSRetentionPolicy(policy jetstream.RetentionPolicy) NATSOption {
	return func(n *NATS) {
		n.retention = policy
	}
}

// NewNATSJetStream creates a new NATS event log instance.
// It requires a connected `*nats.Conn` and accepts functional options for configuration.
// This constructor does NOT create any streams.
//
// Streams are created on-demand when events are first appended for a new aggregate.
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
	}

	for _, opt := range opts {
		opt(njs)
	}

	return njs, nil
}

// AppendEvents appends one or more events for a given aggregate ID to its dedicated stream.
// It performs an optimistic concurrency check based on the `expected` version.
//
// This method is optimized: for single events, it uses a standard, efficient publish call.
// For multiple events, it uses the atomic batch publish feature to ensure all-or-nothing semantics.
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
		// Use the simple, non-batch publish for single events.
		return c.appendSingleEvent(ctx, id, exp, events[0])
	}

	// For more than one event, use the transactional batch publish.
	var newVersion version.Version
	err := c.WithinTx(ctx, func(ctx context.Context, tx jetstream.JetStream) error {
		v, _, err := c.AppendInTx(ctx, tx, id, exp, events)
		if err != nil {
			return err
		}
		newVersion = v
		return nil
	})
	if err != nil {
		// The error from AppendInTx is already descriptive.
		return version.Zero, err
	}

	return newVersion, nil
}

// ReadEvents returns an iterator for the event history of a single aggregate.
// It reads from the aggregate's dedicated stream by creating an ephemeral pull consumer.
func (c *NATS) ReadEvents(ctx context.Context, id event.LogID, selector version.Selector) event.Records {
	return func(yield func(*event.Record, error) bool) {
		streamName := c.logIDToStreamName(id)
		subject := c.logIDToSubjectName(id)

		stream, err := c.js.Stream(ctx, streamName)
		if err != nil {
			// If the stream doesn't exist, it means no events have ever been written
			// for this aggregate, which is a normal condition, not an error.
			if errors.Is(err, jetstream.ErrStreamNotFound) {
				return

			}
			yield(nil, fmt.Errorf("stream not reachable: %v", err))
			return
		}
		consumerCfg := jetstream.ConsumerConfig{
			FilterSubject: subject,
			AckPolicy:     jetstream.AckExplicitPolicy,
			DeliverPolicy: jetstream.DeliverByStartSequencePolicy,
			OptStartSeq:   uint64(selector.From), // The selector's `From` version directly maps to the stream's start sequence.

			InactiveThreshold: 5 * time.Minute,
		}

		// A start sequence of 0 is invalid for DeliverByStartSequencePolicy,
		// so we must switch to DeliverAllPolicy in that case.
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
			// A timeout is provided to `Next` to prevent the loop from blocking indefinitely
			// if there are no more messages. This is the mechanism that signals the end of the stream.
			msg, err := iter.Next(jetstream.NextMaxWait(500 * time.Millisecond))
			if err != nil {
				if errors.Is(err, jetstream.ErrMsgIteratorClosed) {
					// Normal end of iteration
					return
				}
				if errors.Is(err, nats.ErrTimeout) {
					// No more messages currently available
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

			// Acknowledge the message so it is not redelivered.
			if err := msg.Ack(); err != nil {
				yield(nil, fmt.Errorf("read events: acknowledge message: %w", err))
				return
			}
		}

	}
}

// AppendInTx appends a batch of two or more events to the log within a transaction-like context.
// It uses the NATS atomic batch publish feature (ADR-50) for all-or-nothing writes.
func (c *NATS) AppendInTx(
	ctx context.Context,
	_ jetstream.JetStream,
	id event.LogID,
	expected version.Check,
	events event.RawEvents) (version.Version, []*event.Record, error) {

	exp, ok := expected.(version.CheckExact)
	if !ok {
		return version.Zero, nil, fmt.Errorf("append in tx: %w", ErrUnsupportedCheck)
	}

	stream := c.logIDToStreamName(id)
	subject := c.logIDToSubjectName(id)
	msgs := convertRawEventsToNatsMsgs(subject, events, version.Version(exp))
	expectedSeq := uint64(exp)

	// Idempotently ensure the stream for this aggregate exists before publishing.
	_, err := c.ensureStream(ctx, jetstream.StreamConfig{
		Name:               stream,
		Subjects:           []string{subject},
		Storage:            c.storageType,
		Retention:          c.retention,
		AllowAtomicPublish: true, // This flag is required for batch writes.
	})
	if err != nil {
		return version.Zero, nil, fmt.Errorf("cannot create stream: %w", err)
	}

	// The optimistic concurrency check is applied to the entire batch.
	_, err = c.publishBatch(ctx, msgs, &expectedSeq)
	if err != nil {
		if actualVersion, ok := parseActualVersionFromError(err); ok {
			return version.Zero, nil, version.NewConflictError(version.Version(exp), actualVersion)
		}
		return version.Zero, nil, fmt.Errorf("append events: publish batch to jetstream: %w", err)
	}

	// On success, the new version is the expected version plus the number of new events.
	finalVersion := version.Version(exp) + version.Version(len(events))

	// The `event.TransactionalLog` interface requires us to return the persisted records.
	records := make([]*event.Record, len(events))
	for i, raw := range events {
		records[i] = event.NewRecord(version.Version(exp)+version.Version(i+1), id, raw.EventName(), raw.Data())
	}

	return finalVersion, records, nil
}

// WithinTx provides a transactional boundary for event log operations.
// For NATS, single publish and batch publish operations are already atomic by nature,
// so this function simply executes the provided function without creating a
// separate transaction handle.
func (c *NATS) WithinTx(ctx context.Context, fn func(ctx context.Context, tx jetstream.JetStream) error) error {
	return fn(ctx, c.js)
}

// ensureStream idempotently ensures a stream with the given configuration exists.
// If the stream does not exist, it is created. If it exists, it is updated to
// match the configuration, which is a safe, idempotent operation.
func (c *NATS) ensureStream(ctx context.Context, cfg jetstream.StreamConfig) (jetstream.Stream, error) {
	stream, err := c.js.Stream(ctx, cfg.Name)
	if err != nil || stream == nil {
		if errors.Is(err, jetstream.ErrStreamNotFound) {
			stream, err = c.js.CreateStream(ctx, cfg)
			if err != nil {
				return nil, fmt.Errorf("failed to create stream %s: %w", cfg.Name, err)
			}
			return stream, nil
		}
		return nil, fmt.Errorf("failed to get stream %s info: %w", cfg.Name, err)
	}

	// Stream exists, update it (idempotent)
	streamInfo, err := stream.Info(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get stream info %s: %w", cfg.Name, err)
	}

	cfg.Retention = streamInfo.Config.Retention

	updatedStream, err := c.js.UpdateStream(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to update stream %s: %w", cfg.Name, err)
	}
	return updatedStream, nil
}

// publishBatch sends a slice of messages to a JetStream stream atomically.
// This function implements the client-side logic defined in the NATS ADR (ADR-50) for
// JetStream Batch Publishing.
//
// It works by:
//  1. Generating a unique batch ID.
//  2. Sending the first message as a request to initiate the batch on the server.
//  3. Sending intermediate messages as simple publishes for performance.
//  4. Sending the final message as a request with a commit flag, which triggers
//     the atomic write on the server.
func (c *NATS) publishBatch(
	ctx context.Context,
	msgs []*nats.Msg,
	expectedLastSeq *uint64,
) (*pubAck, error) {

	if len(msgs) == 0 {
		return nil, errors.New("cannot publish an empty batch")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// 1. Generate a unique ID for this entire batch.
	uid, err := uuid.NewV7()
	if err != nil {
		return nil, fmt.Errorf("failed to generate batch ID: %w", err)
	}
	batchID := uid.String()

	totalMsgs := len(msgs)

	// --- The Publishing Loop ---
	for i, originalMsg := range msgs {
		if err := ctx.Err(); err != nil {
			// If the context is cancelled mid-batch, the batch will time out
			// on the server and be abandoned.
			return nil, err
		}

		batchSeq := i + 1
		isFirst := i == 0
		isLast := i == totalMsgs-1

		// Prepare the message with appropriate batch headers.
		msg := c.prepareBatchMessage(originalMsg, batchID, batchSeq, totalMsgs, expectedLastSeq)

		if isFirst || isLast {
			reply, err := c.nc.RequestMsgWithContext(ctx, msg)
			if err != nil {
				return nil, fmt.Errorf("request for batch message %d failed: %w", batchSeq, err)
			}
			// Only the last message's reply contains the definitive PubAck.
			if isLast {
				var pa pubAck
				if err := json.Unmarshal(reply.Data, &pa); err != nil {
					return nil, fmt.Errorf("could not unmarshal commit response: %w", err)
				}
				if pa.Error.Code != 0 {
					return nil, &pa.Error
				}
				return &pa, nil
			}
			// The reply for the first message should be empty unless there's an immediate error.
			if len(reply.Data) > 0 {
				var apiErr pubAck
				if json.Unmarshal(reply.Data, &apiErr) == nil && apiErr.Error.ErrorCode != 0 {
					return nil, fmt.Errorf("received error initiating batch: %w", &apiErr.Error)
				}
			}
		} else {
			// Intermediate messages are fire-and-forget for performance.
			if err := c.nc.PublishMsg(msg); err != nil {
				return nil, err
			}
		}
	}

	// This part of the code should be unreachable.
	return nil, errors.New("batch publishing loop finished without a commit message")
}

// prepareBatchMessage is a helper that constructs a `*nats.Msg` for use in the
// ADR-50 batch publish protocol. It clones the original message and injects the
// necessary `Nats-Batch-*` and optimistic locking headers
func (c *NATS) prepareBatchMessage(originalMsg *nats.Msg, batchID string, seq int, total int, expectedLastSeq *uint64) *nats.Msg {
	msg := &nats.Msg{
		Subject: originalMsg.Subject,
		Reply:   originalMsg.Reply,
		Data:    originalMsg.Data,
		Header:  cloneHeader(originalMsg.Header),
	}

	msg.Header.Set(natsBatchIDHeader, batchID)
	msg.Header.Set(natsBatchSeqHeader, strconv.Itoa(seq))

	isFirst := seq == 1
	isLast := seq == total

	if isFirst && expectedLastSeq != nil {
		msg.Header.Set(natsExpectedLastSubjectSeqHeader, strconv.FormatUint(*expectedLastSeq, 10))
	}

	if isLast {
		msg.Header.Set(natsBatchCommitHeader, "1")
	}

	return msg
}

// convertRawEventsToNatsMsgs transforms a slice of Chronicle's RawEvents into a slice
// of `*nats.Msg` suitable for publishing. It embeds the aggregate version and event
// name into the headers of each message.
func convertRawEventsToNatsMsgs(subject string, events event.RawEvents, startingVersion version.Version) []*nats.Msg {
	msgs := make([]*nats.Msg, len(events))
	for i, rawEvent := range events {
		eventVersion := startingVersion + version.Version(i+1)

		msgs[i] = &nats.Msg{
			Subject: subject,
			Data:    rawEvent.Data(),
			Header: nats.Header{
				headerEventName:        []string{rawEvent.EventName()},
				headerAggregateVersion: []string{strconv.FormatUint(uint64(eventVersion), 10)},
			},
		}
	}
	return msgs
}

// cloneHeader performs a deep copy of a nats.Header map.
// This is necessary because a shallow copy would result in both the original
// and the new message sharing the same header map in memory.
func cloneHeader(original nats.Header) nats.Header {
	if original == nil {
		return nats.Header{}
	}
	clone := make(nats.Header, len(original))
	for key, values := range original {
		// Also copy the slice of values to be completely safe.
		clone[key] = append([]string(nil), values...)
	}
	return clone
}

// appendSingleEvent is an optimized path for writing a single event to a stream.
// It uses the standard `jetstream.PublishMsg` which is more efficient than the
// full ADR-50 batch protocol for this common case.
func (c *NATS) appendSingleEvent(
	ctx context.Context,
	id event.LogID,
	expected version.CheckExact,
	rawEvent event.Raw,
) (version.Version, error) {
	streamName := c.logIDToStreamName(id)
	subject := c.logIDToSubjectName(id)
	_, err := c.ensureStream(ctx, jetstream.StreamConfig{
		Name:      streamName,
		Subjects:  []string{subject},
		Storage:   c.storageType,
		Retention: c.retention,
	})

	if err != nil {
		return version.Zero, fmt.Errorf("could not ensure stream '%s': %w", streamName, err)
	}

	newEventVersion := version.Version(expected) + 1

	// Create the nats.Msg, embedding the version and event name in the headers.
	msg := &nats.Msg{
		Subject: subject,
		Data:    rawEvent.Data(),
		Header: nats.Header{
			headerEventName:        []string{rawEvent.EventName()},
			headerAggregateVersion: []string{strconv.FormatUint(uint64(newEventVersion), 10)},
		},
	}

	// Prepare the publish options for optimistic concurrency control.
	pubOpts := []jetstream.PublishOpt{
		jetstream.WithExpectLastSequencePerSubject(uint64(expected)),
	}

	// Publish the single message.
	_, err = c.js.PublishMsg(ctx, msg, pubOpts...)
	if err != nil {
		if actualVersion, ok := parseActualVersionFromError(err); ok {
			return version.Zero, version.NewConflictError(version.Version(expected), actualVersion)
		}
		// Handle other publish errors.
		return version.Zero, fmt.Errorf("append single event: %w", err)
	}
	return newEventVersion, nil
}

// wrongLastSeqRegexp is designed to find one or more digits at the very end of the string.
var wrongLastSeqRegexp = regexp.MustCompile(`(\d+)$`)

// parseActualVersionFromError inspects an error to see if it's a Jetstream
// "wrong last sequence" conflict (ErrorCode 10071). If it is, it safely parses the
// actual version from the error's description string and returns it. This is more
// reliable and efficient than making a separate network call to get the latest version.
func parseActualVersionFromError(err error) (version.Version, bool) {
	var apiErr *jetstream.APIError
	// First, check if the error is a jetstream.APIError and has the correct code.
	if errors.As(err, &apiErr) && apiErr.ErrorCode == jetstream.JSErrCodeStreamWrongLastSequence {
		matches := wrongLastSeqRegexp.FindStringSubmatch(apiErr.Description)
		if len(matches) > 1 {
			actual, parseErr := strconv.ParseUint(matches[1], 10, 64)
			if parseErr == nil {
				return version.Version(actual), true
			}
		}
	}
	// The error was not the specific conflict error we were looking for.
	return version.Zero, false
}

// logIDToStreamName converts an event.LogID into a NATS-compatible stream name.
// e.g., "foo/123" -> "chronicle_events_foo_123"
func (c *NATS) logIDToStreamName(id event.LogID) string {
	safeID := strings.ReplaceAll(string(id), "/", "_")
	safeID = strings.ReplaceAll(safeID, ".", "_")
	safeID = strings.ReplaceAll(safeID, " ", "_")
	return fmt.Sprintf("%s_%s", c.streamPrefix, safeID)
}

// logIDToSubjectName converts an event.LogID into a NATS-compatible stream name.
func (c *NATS) logIDToSubjectName(id event.LogID) string {
	safeID := strings.ReplaceAll(string(id), "/", "_")
	safeID = strings.ReplaceAll(safeID, ".", "_")
	safeID = strings.ReplaceAll(safeID, " ", "_")
	return fmt.Sprintf("%s.%s", c.subjectPrefix, safeID)
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
