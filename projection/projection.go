package projection

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/DeluxeOwl/chronicle/event"
	"github.com/DeluxeOwl/chronicle/version"
)

//go:generate go run github.com/matryer/moq@latest -pkg projection_test -skip-ensure -rm -out projection_mock_test.go . Checkpointer AsyncProjection

// TODO: how would a ProjectionTx even make sense? Maybe combine TransactionalAggregateProcessor + another one that doesn't take the events
// TODO: maybe polling is not needed if the event log blocks (e.g. channel, nats)

type AsyncProjection interface {
	MatchesEvent(eventName string) bool
	Handle(ctx context.Context, rec *event.GlobalRecord) error
}

type Checkpointer interface {
	GetCheckpoint(ctx context.Context, projectionName string) (version.Version, error)
	SaveCheckpoint(ctx context.Context, projectionName string, v version.Version) error
}

type OnSaveCheckpointErrFunc = func(ctx context.Context, err error) error

type AsyncProjectionRunner struct {
	eventlog       event.GlobalLog
	checkpointer   Checkpointer
	projectionName string

	// configurable
	pollInterval              time.Duration
	cancelCheckpointTimeout   time.Duration
	log                       *slog.Logger
	lastCheckpointSaveEnabled bool
	checkpointPolicy          CheckpointPolicy
	onSaveCheckpointErrFunc   OnSaveCheckpointErrFunc
	isTailing                 bool

	projection AsyncProjection
}

type AsyncProjectionRunnerOption func(*AsyncProjectionRunner)

const (
	DefaultPollInterval            = 200 * time.Millisecond
	DefaultCancelCheckpointTimeout = 5 * time.Second
)

var (
	ContinueOnError = func(ctx context.Context, err error) error { return nil }
	StopOnError     = func(ctx context.Context, err error) error { return err }
)

func NewAsyncProjectionRunner(
	log event.GlobalLog,
	checkpointer Checkpointer,
	projection AsyncProjection,
	projectionName string,
	opts ...AsyncProjectionRunnerOption,
) (*AsyncProjectionRunner, error) {
	runner := &AsyncProjectionRunner{
		eventlog:                  log,
		checkpointer:              checkpointer,
		pollInterval:              DefaultPollInterval,
		cancelCheckpointTimeout:   DefaultCancelCheckpointTimeout,
		log:                       slog.Default(),
		onSaveCheckpointErrFunc:   StopOnError,
		projection:                projection,
		projectionName:            projectionName,
		checkpointPolicy:          &batchEndPolicy{},
		lastCheckpointSaveEnabled: true,
		isTailing:                 false, // Default to polling behavior
	}

	for _, opt := range opts {
		opt(runner)
	}

	return runner, nil
}

func (r *AsyncProjectionRunner) Run(ctx context.Context) error {
	currentVersion, err := r.checkpointer.GetCheckpoint(ctx, r.projectionName)
	if err != nil {
		return fmt.Errorf(
			"projection %q: failed to get initial checkpoint: %w",
			r.projectionName,
			err,
		)
	}

	r.log.InfoContext(
		ctx,
		"Loaded initial checkpoint",
		"projection",
		r.projectionName,
		"version",
		currentVersion,
	)

	if r.isTailing {
		r.log.InfoContext(ctx, "Starting projection in Tailing mode", "name", r.projectionName)
		return r.runTailing(ctx, currentVersion)
	}

	r.log.InfoContext(
		ctx,
		"Starting projection in Polling mode",
		"name",
		r.projectionName,
		"poll_interval",
		r.pollInterval,
	)
	return r.runPolling(ctx, currentVersion)
}

func (r *AsyncProjectionRunner) runTailing(
	ctx context.Context,
	currentVersion version.Version,
) error {
	// In tailing mode, we call readAndProcess once and let it block.
	// It will only return when the context is canceled or a fatal error occurs.
	lastProcessedVersion, err := r.readAndProcess(ctx, currentVersion)
	if err != nil {
		// Check if the error is due to context cancellation, which is our signal for a graceful shutdown.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			if !r.lastCheckpointSaveEnabled {
				return err
			}

			return r.handleShutdown(ctx, lastProcessedVersion)
		}

		return err
	}

	if !r.lastCheckpointSaveEnabled {
		return nil
	}

	// The stream ended without a context error. This is unexpected for a tailing consumer,
	// but we can handle it gracefully by saving a final checkpoint.
	r.log.InfoContext(ctx, "Tailing stream ended unexpectedly, saving final checkpoint",
		"projection", r.projectionName, "version", lastProcessedVersion)

	return r.handleShutdown(ctx, lastProcessedVersion)
}

func (r *AsyncProjectionRunner) runPolling(
	ctx context.Context,
	currentVersion version.Version,
) error {
	ticker := time.NewTicker(r.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			if !r.lastCheckpointSaveEnabled {
				return ctx.Err()
			}
			return r.handleShutdown(ctx, currentVersion)
		case <-ticker.C:
			newVersion, err := r.readAndProcess(ctx, currentVersion)
			if err != nil {
				return err
			}
			currentVersion = newVersion
		}
	}
}

func (r *AsyncProjectionRunner) handleShutdown(
	ctx context.Context,
	currentVersion version.Version,
) error {
	r.log.InfoContext(ctx, "Projection stopping, saving final checkpoint",
		"projection", r.projectionName, "version", currentVersion)

	saveCtx, cancel := context.WithTimeout(context.Background(), r.cancelCheckpointTimeout)
	defer cancel()

	if err := r.checkpointer.SaveCheckpoint(saveCtx, r.projectionName, currentVersion); err != nil {
		r.log.ErrorContext(saveCtx, "Failed to save final checkpoint on shutdown",
			"projection", r.projectionName, "error", err)
		if cerr := r.onSaveCheckpointErrFunc(saveCtx, err); cerr != nil {
			return fmt.Errorf("projection %q: save checkpoint err: %w", r.projectionName, cerr)
		}
	}

	// Always return the original context error to signal shutdown completion.
	if ctx.Err() != nil {
		return ctx.Err()
	}

	return nil
}

func (r *AsyncProjectionRunner) readAndProcess(
	ctx context.Context,
	lastSavedVersion version.Version,
) (version.Version, error) {
	var (
		eventsSinceLastSave int
		lastSaveTime        = time.Now()
		// tracks the last successfully processed version.
		// It's the highest version we can safely checkpoint.
		lastProcessedVersion = lastSavedVersion
	)

	stream := r.eventlog.ReadAllEvents(ctx, version.SelectFrom(lastSavedVersion+1))
	for rec, streamErr := range stream {
		if streamErr != nil {
			r.log.ErrorContext(ctx, "Stream error, stopping projection",
				"projection", r.projectionName, "error", streamErr)

			// On stream error (e.g., context canceled), return the version of the last
			// event we successfully processed, along with the error. Required for
			// graceful shutdown in tailing mode.
			return lastProcessedVersion, fmt.Errorf(
				"projection %q: stream error: %w",
				r.projectionName,
				streamErr,
			)
		}

		if r.projection.MatchesEvent(rec.EventName()) {
			if err := r.projection.Handle(ctx, rec); err != nil {
				// On handler error, we return the last known good version (the one before this
				// failing event) so we can retry from there next time.
				return lastProcessedVersion, fmt.Errorf(
					"projection %q: handler failed on event %d: %w",
					r.projectionName, rec.GlobalVersion(), err)
			}
		}

		// Update the last processed version now that handling was successful.
		lastProcessedVersion = rec.GlobalVersion()
		eventsSinceLastSave++

		info := CheckpointInfo{
			EventsSinceLastSave: eventsSinceLastSave,
			TimeSinceLastSave:   time.Since(lastSaveTime),
		}
		if r.checkpointPolicy.ShouldCheckpoint(info) {
			r.log.DebugContext(
				ctx,
				"Policy triggered checkpoint save",
				"projection",
				r.projectionName,
				"events",
				eventsSinceLastSave,
				"duration",
				info.TimeSinceLastSave,
			)

			if err := r.saveCheckpoint(ctx, lastProcessedVersion); err != nil {
				// We failed a mid-batch save. Return the last known good *saved* version and the error.
				return lastSavedVersion, err
			}

			// A mid-batch checkpoint was saved successfully.
			// This now becomes our new "last saved version".
			lastSavedVersion = lastProcessedVersion
			eventsSinceLastSave = 0
			lastSaveTime = time.Now()
		}
	}

	// After the polling loop finishes (batch is done), check for unsaved progress.
	if lastProcessedVersion > lastSavedVersion {
		if err := r.saveCheckpoint(ctx, lastProcessedVersion); err != nil {
			return lastSavedVersion, err
		}

		lastSavedVersion = lastProcessedVersion
		r.log.DebugContext(
			ctx,
			"Processed batch and saved checkpoint",
			"projection",
			r.projectionName,
			"count",
			eventsSinceLastSave,
			"new_version",
			lastProcessedVersion,
		)
	}

	return lastSavedVersion, nil
}

func (r *AsyncProjectionRunner) saveCheckpoint(
	ctx context.Context,
	version version.Version,
) error {
	if err := r.checkpointer.SaveCheckpoint(ctx, r.projectionName, version); err != nil {
		r.log.ErrorContext(ctx, "Failed to save checkpoint",
			"projection", r.projectionName, "error", err)
		if cerr := r.onSaveCheckpointErrFunc(ctx, err); cerr != nil {
			return fmt.Errorf("projection %q: save checkpoint err: %w", r.projectionName, cerr)
		}
	}
	return nil
}

func WithPollInterval(interval time.Duration) AsyncProjectionRunnerOption {
	return func(r *AsyncProjectionRunner) {
		r.pollInterval = interval
	}
}

func WithCancelCheckpointTimeout(timeout time.Duration) AsyncProjectionRunnerOption {
	return func(r *AsyncProjectionRunner) {
		r.cancelCheckpointTimeout = timeout
	}
}

func WithSlogHandler(handler slog.Handler) AsyncProjectionRunnerOption {
	return func(r *AsyncProjectionRunner) {
		if handler == nil {
			r.log = slog.New(slog.DiscardHandler)
			return
		}
		r.log = slog.New(handler)
	}
}

func WithSaveCheckpointErrPolicy(
	errFunc OnSaveCheckpointErrFunc,
) AsyncProjectionRunnerOption {
	return func(r *AsyncProjectionRunner) {
		if errFunc == nil {
			return
		}
		r.onSaveCheckpointErrFunc = errFunc
	}
}

func WithLastCheckpointSaveEnabled(enabled bool) AsyncProjectionRunnerOption {
	return func(r *AsyncProjectionRunner) {
		r.lastCheckpointSaveEnabled = enabled
	}
}

func WithCheckpointPolicy(policy CheckpointPolicy) AsyncProjectionRunnerOption {
	return func(r *AsyncProjectionRunner) {
		r.checkpointPolicy = policy
	}
}

// WithTailing configures the runner to use a blocking strategy suitable for
// event logs that wait for new events (like a channel or a NATS subscription)
// instead of polling. When set to true, the PollInterval is ignored.
func WithTailing(enabled bool) AsyncProjectionRunnerOption {
	return func(r *AsyncProjectionRunner) {
		r.isTailing = enabled
	}
}
