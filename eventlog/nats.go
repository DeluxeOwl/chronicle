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
	BatchId   string             `json:"batch,omitempty"`
	BatchSize int                `json:"count,omitempty"`
	Error     jetstream.APIError `json:"error"`
}

// NATS is an implementation of event.Log for NATS Jetstream.
// It uses a Jetstream stream to store events. Each aggregate's event log
// is a subject within that stream.
//
// Optimistic concurrency control is achieved using Jetstream's "Expected Last
// Subject Sequence" feature, which is highly reliable for preventing race
// conditions during writes.
type NATS struct {
	nc          *nats.Conn
	js          jetstream.JetStream
	storageType jetstream.StorageType
	retention   jetstream.RetentionPolicy

	streamPrefix  string
	subjectPrefix string
}
type NatsOption func(*NATS)

// WithStreamName is a configuration option that sets the name of the stream
// used to store events. If not provided, it defaults to "chronicle_events".
func WithStreamName(name string) NatsOption {
	return func(n *NATS) {
		n.streamPrefix = name
	}
}

// WithSubjectPrefix is a configuration option that sets the prefix for all
// subjects. Defaults to "chronicle.events". So an aggregate with ID "123" will
// have its events on "chronicle.events.123".
func WithSubjectPrefix(prefix string) NatsOption {
	return func(n *NATS) {
		n.subjectPrefix = prefix
	}
}

// WithStorage sets the storage backend for the Jetstream stream.
// Defaults to jetstream.FileStorage.
func WithStorage(storage jetstream.StorageType) NatsOption {
	return func(n *NATS) {
		n.storageType = storage
	}
}

// WithRetentionPolicy sets the retention policy for the Jetstream stream.
// Defaults to jetstream.WorkQueuePolicy.
func WithRetentionPolicy(policy jetstream.RetentionPolicy) NatsOption {
	return func(n *NATS) {
		n.retention = policy
	}
}

func NewNATSJetStream(nc *nats.Conn, opts ...NatsOption) (*NATS, error) {
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

// AppendEvents implements event.Log.
// It optimizes for the common case of appending a single event by using the standard
// jetstream.PublishMsg method. For batches of more than one event, it uses the
// atomic batch publish feature.
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

// ReadEvents implements event.Log.
func (c *NATS) ReadEvents(ctx context.Context, id event.LogID, selector version.Selector) event.Records {
	return func(yield func(*event.Record, error) bool) {
		streamName := c.logIDToStreamName(id)
		subject := c.logIDToSubjectName(id)

		stream, err := c.js.Stream(ctx, streamName)
		if err != nil {
			if errors.Is(err, jetstream.ErrStreamNotFound) {
				return

			}
			yield(nil, fmt.Errorf("stream not reachable: %v", err))
			return
		}
		consumerCfg := jetstream.ConsumerConfig{
			FilterSubject:     subject,
			AckPolicy:         jetstream.AckExplicitPolicy,
			DeliverPolicy:     jetstream.DeliverByStartSequencePolicy,
			OptStartSeq:       uint64(selector.From),
			InactiveThreshold: 5 * time.Minute,
		}

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

			if err := msg.Ack(); err != nil {
				yield(nil, fmt.Errorf("read events: acknowledge message: %w", err))
				return
			}
		}

	}
}

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

	_, err := c.ensureStream(ctx, jetstream.StreamConfig{
		Name:               stream,
		Subjects:           []string{subject},
		Storage:            c.storageType,
		Retention:          c.retention,
		AllowAtomicPublish: true,
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

	finalVersion := version.Version(exp) + version.Version(len(events))

	records := make([]*event.Record, len(events))
	for i, raw := range events {
		records[i] = event.NewRecord(version.Version(exp)+version.Version(i+1), id, raw.EventName(), raw.Data())
	}

	return finalVersion, records, nil
}

func (c *NATS) WithinTx(ctx context.Context, fn func(ctx context.Context, tx jetstream.JetStream) error) error {
	return fn(ctx, c.js)
}

// EnsureStream ensures that a stream with the given configuration exists.
// It creates the stream if it doesn't exist or updates it if it does.
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

		if isLast {
			// 4. FINAL MESSAGE: Send as a request and wait for the PubAck.
			reply, err := c.nc.RequestMsgWithContext(ctx, msg)
			if err != nil {
				return nil, fmt.Errorf("failed on final batch commit message: %w", err)
			}

			// The final reply contains the full PubAck.
			var pa pubAck
			if err := json.Unmarshal(reply.Data, &pa); err != nil {
				// Handle cases where the reply is just a raw API error.
				var apiErr jetstream.APIError
				if json.Unmarshal(reply.Data, &apiErr) == nil {
					return nil, &apiErr
				}
				return nil, fmt.Errorf("failed to unmarshal puback from commit response: %w", err)
			}
			if pa.Error.Code != 0 {
				return nil, &pa.Error
			}
			return &pa, nil

		} else if isFirst {
			// 2. FIRST MESSAGE: Send as a request to initiate the batch.
			reply, err := c.nc.RequestMsgWithContext(ctx, msg)
			if err != nil {
				return nil, fmt.Errorf("failed to initiate batch: %w", err)
			}
			// The reply for the first message should be empty unless there's an immediate error.
			if len(reply.Data) > 0 {
				var apiErr jetstream.APIError
				if json.Unmarshal(reply.Data, &apiErr) == nil && apiErr.ErrorCode != 0 {
					return nil, fmt.Errorf("received error initiating batch: %w", &apiErr)
				}
			}

		} else {
			// 3. INTERMEDIATE MESSAGES: Send as a simple, fire-and-forget publish.
			if err := c.nc.PublishMsg(msg); err != nil {
				return nil, fmt.Errorf("failed on intermediate batch message %d: %w", batchSeq, err)
			}
		}
	}

	// This part of the code should be unreachable.
	return nil, errors.New("batch publishing loop finished without a commit message")
}

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

func convertRawEventsToNatsMsgs(subject string, events event.RawEvents, startingVersion version.Version) []*nats.Msg {
	msgs := make([]*nats.Msg, len(events))
	for i, rawEvent := range events {
		// The aggregate's version for this specific event.
		eventVersion := startingVersion + version.Version(i+1)

		msgs[i] = &nats.Msg{
			Subject: subject,
			Data:    rawEvent.Data(),
			Header: nats.Header{
				headerEventName: []string{rawEvent.EventName()},
				// Store the version as a string in the header.
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

// appendSingleEvent handles the optimized case of writing a single event to the stream.
// It uses the standard PublishMsg which is robust and efficient.
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
// "wrong last sequence" conflict. If it is, it parses the actual version
// from the error's description string and returns it.
func parseActualVersionFromError(err error) (version.Version, bool) {
	var apiErr *jetstream.APIError
	// First, check if the error is a jetstream.APIError and has the correct code.
	if errors.As(err, &apiErr) && apiErr.ErrorCode == jetstream.JSErrCodeStreamWrongLastSequence {
		// If it is, use the regex to find the number at the end of the description.
		matches := wrongLastSeqRegexp.FindStringSubmatch(apiErr.Description)
		// matches[0] is the full match ("11"), matches[1] is the first capture group ("11").
		if len(matches) > 1 {
			actual, parseErr := strconv.ParseUint(matches[1], 10, 64)
			if parseErr == nil {
				// Success! We found and parsed the version.
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
