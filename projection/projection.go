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
// TODO: Checkpoint policy, every N events or checkpoint after duration

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
	pollInterval            time.Duration
	cancelCheckpointTimeout time.Duration
	log                     *slog.Logger
	onSaveCheckpointErrFunc OnSaveCheckpointErrFunc

	projection AsyncProjection
}

type AsyncProjectionRunnerOption func(*AsyncProjectionRunner)

func WithPollInterval(interval time.Duration) AsyncProjectionRunnerOption {
	return func(g *AsyncProjectionRunner) {
		g.pollInterval = interval
	}
}

func WithCancelCheckpointTimeout(timeout time.Duration) AsyncProjectionRunnerOption {
	return func(g *AsyncProjectionRunner) {
		g.cancelCheckpointTimeout = timeout
	}
}

func WithSlogHandler(handler slog.Handler) AsyncProjectionRunnerOption {
	return func(g *AsyncProjectionRunner) {
		if handler == nil {
			g.log = slog.New(slog.DiscardHandler)
			return
		}
		g.log = slog.New(handler)
	}
}

// The default is StopOnError
func WithSaveCheckpointErrPolicy(errFunc OnSaveCheckpointErrFunc) AsyncProjectionRunnerOption {
	return func(g *AsyncProjectionRunner) {
		if errFunc == nil {
			return
		}
		g.onSaveCheckpointErrFunc = errFunc
	}
}

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
		eventlog:                log,
		checkpointer:            checkpointer,
		pollInterval:            DefaultPollInterval,
		cancelCheckpointTimeout: DefaultCancelCheckpointTimeout,
		log:                     slog.Default(),
		onSaveCheckpointErrFunc: StopOnError,
		projection:              projection,
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
	currentVersion version.Version,
) (version.Version, error) {
	var processedInBatch int
	newVersion := currentVersion

	for rec, streamErr := range r.eventlog.ReadAllEvents(ctx, version.SelectFrom(currentVersion)) {
		if streamErr != nil {
			r.log.ErrorContext(ctx, "Stream error, stopping projection",
				"projection", pname, "error", streamErr)
			return currentVersion, fmt.Errorf("projection %q: stream error: %w", pname, streamErr)
		}

		if r.projection.MatchesEvent(rec.EventName()) {
			if err := r.projection.Handle(ctx, rec); err != nil {
				return currentVersion, fmt.Errorf(
					"projection %q: handler failed on event %d: %w",
					pname, rec.GlobalVersion(), err)
			}
		}

		newVersion = rec.GlobalVersion()
		processedInBatch++
	}

	// Only save if we actually processed events
	if processedInBatch == 0 {
		return currentVersion, nil
	}

	if err := r.saveCheckpoint(ctx, pname, newVersion); err != nil {
		return currentVersion, err
	}

	r.log.DebugContext(ctx, "Processed batch and saved checkpoint",
		"projection", pname, "count", processedInBatch, "new_version", newVersion)

	return newVersion, nil
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
