package projection

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/DeluxeOwl/chronicle/event"
	"github.com/DeluxeOwl/chronicle/version"
)

//go:generate go run github.com/matryer/moq@latest -pkg projection_test -skip-ensure -rm -out projection_mock_test.go . Checkpointer AsyncProjection

// TODO: how would a ProjectionTx even make sense? Maybe combine TransactionalAggregateProcessor + another one that doesn't take the events
// TODO: Checkpoint policy, every N events or checkpoint after duration. Batch size.
// TODO: maybe polling is not needed if the event log blocks (e.g. channel, nats)

type AsyncProjection interface {
	Name() string
	MatchesEvent(eventName string) bool
	Handle(ctx context.Context, rec *event.GlobalRecord) error
}

type Checkpointer interface {
	GetCheckpoint(ctx context.Context, projectionName string) (version.Version, error)
	SaveCheckpoint(ctx context.Context, projectionName string, v version.Version) error
}

type OnSaveCheckpointErrFunc = func(ctx context.Context, err error) error

type AsyncProjectionRunner struct {
	eventlog     event.GlobalLog
	checkpointer Checkpointer

	// configurable
	pollInterval              time.Duration
	cancelCheckpointTimeout   time.Duration
	log                       *slog.Logger
	lastCheckpointSaveEnabled bool
	checkpointPolicy          CheckpointPolicy
	onSaveCheckpointErrFunc   OnSaveCheckpointErrFunc

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
		checkpointPolicy:          &batchEndPolicy{},
		lastCheckpointSaveEnabled: true,
	}

	for _, o := range opts {
		o(runner)
	}

	return runner, nil
}

func (r *AsyncProjectionRunner) Run(ctx context.Context) error {
	pname := r.projection.Name()

	r.log.InfoContext(ctx, "Starting projection", "name", pname, "poll_interval", r.pollInterval)

	currentVersion, err := r.checkpointer.GetCheckpoint(ctx, pname)
	if err != nil {
		return fmt.Errorf("projection %q: failed to get initial checkpoint: %w", pname, err)
	}

	r.log.InfoContext(
		ctx,
		"Loaded initial checkpoint",
		"projection",
		pname,
		"version",
		currentVersion,
	)

	ticker := time.NewTicker(r.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			if !r.lastCheckpointSaveEnabled {
				return ctx.Err()
			}
			return r.handleShutdown(ctx, pname, currentVersion)
		case <-ticker.C:
			newVersion, err := r.readAndProcess(ctx, pname, currentVersion)
			if err != nil {
				return err
			}
			currentVersion = newVersion
		}
	}
}

func (r *AsyncProjectionRunner) handleShutdown(
	ctx context.Context,
	pname string,
	currentVersion version.Version,
) error {
	r.log.InfoContext(ctx, "Projection stopping, saving final checkpoint",
		"projection", pname, "version", currentVersion)

	saveCtx, cancel := context.WithTimeout(context.Background(), r.cancelCheckpointTimeout)
	defer cancel()

	if err := r.checkpointer.SaveCheckpoint(saveCtx, pname, currentVersion); err != nil {
		r.log.ErrorContext(saveCtx, "Failed to save final checkpoint on shutdown",
			"projection", pname, "error", err)
		if cerr := r.onSaveCheckpointErrFunc(saveCtx, err); cerr != nil {
			return fmt.Errorf("projection %q: save checkpoint err: %w", pname, cerr)
		}
	}
	return ctx.Err()
}

func (r *AsyncProjectionRunner) readAndProcess(
	ctx context.Context,
	pname string,
	lastSavedVersion version.Version,
) (version.Version, error) {
	var (
		eventsSinceLastSave int
		lastSaveTime        = time.Now()
		// versionToSave tracks the version of the latest processed event.
		versionToSave = lastSavedVersion
	)

	stream := r.eventlog.ReadAllEvents(ctx, version.SelectFrom(lastSavedVersion+1))
	for rec, streamErr := range stream {
		if streamErr != nil {
			r.log.ErrorContext(ctx, "Stream error, stopping projection",
				"projection", pname, "error", streamErr)
			// On stream error, return the last known good version.
			return lastSavedVersion, fmt.Errorf("projection %q: stream error: %w", pname, streamErr)
		}

		if r.projection.MatchesEvent(rec.EventName()) {
			if err := r.projection.Handle(ctx, rec); err != nil {
				// On handler error, we MUST return the last successfully saved version,
				// NOT a partially processed one.
				return lastSavedVersion, fmt.Errorf(
					"projection %q: handler failed on event %d: %w",
					pname, rec.GlobalVersion(), err)
			}
		}

		versionToSave = rec.GlobalVersion()
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
				pname,
				"events",
				eventsSinceLastSave,
				"duration",
				info.TimeSinceLastSave,
			)

			if err := r.saveCheckpoint(ctx, pname, versionToSave); err != nil {
				// We failed a mid-batch save. Return the last known good version and the error.
				return lastSavedVersion, err
			}

			// A mid-batch checkpoint was saved successfully.
			// This now becomes our new "last saved version".
			lastSavedVersion = versionToSave
			eventsSinceLastSave = 0
			lastSaveTime = time.Now()
		}
	}

	// After the loop, check if we processed any events since the last save (either the initial one or a mid-batch one).
	if versionToSave > lastSavedVersion {
		if err := r.saveCheckpoint(ctx, pname, versionToSave); err != nil {
			// Failed to save the final part of the batch. Return the last known good version.
			return lastSavedVersion, err
		}
		// The final part of the batch was saved. The new checkpoint is versionToSave.
		lastSavedVersion = versionToSave
		r.log.DebugContext(ctx, "Processed batch and saved checkpoint",
			"projection", pname, "count", eventsSinceLastSave, "new_version", versionToSave)
	}

	// Return the latest successfully saved version.
	return lastSavedVersion, nil
}

func (r *AsyncProjectionRunner) saveCheckpoint(
	ctx context.Context,
	pname string,
	version version.Version,
) error {
	if err := r.checkpointer.SaveCheckpoint(ctx, pname, version); err != nil {
		r.log.ErrorContext(ctx, "Failed to save checkpoint",
			"projection", pname, "error", err)
		if cerr := r.onSaveCheckpointErrFunc(ctx, err); cerr != nil {
			return fmt.Errorf("projection %q: save checkpoint err: %w", pname, cerr)
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

// The default is StopOnError
func WithSaveCheckpointErrPolicy(errFunc OnSaveCheckpointErrFunc) AsyncProjectionRunnerOption {
	return func(r *AsyncProjectionRunner) {
		if errFunc == nil {
			return
		}
		r.onSaveCheckpointErrFunc = errFunc
	}
}

// Defaults to true
func WithLastCheckpointSaveEnabled(enabled bool) AsyncProjectionRunnerOption {
	return func(r *AsyncProjectionRunner) {
		r.lastCheckpointSaveEnabled = enabled
	}
}

// WithCheckpointPolicy sets the policy for when to save checkpoints.
// The default is to save a checkpoint at the end of each processed batch of events.
// Use policies like EveryNEvents(100) or AfterDuration(5*time.Second) for more frequent checkpointing.
func WithCheckpointPolicy(policy CheckpointPolicy) AsyncProjectionRunnerOption {
	return func(r *AsyncProjectionRunner) {
		r.checkpointPolicy = policy
	}
}
