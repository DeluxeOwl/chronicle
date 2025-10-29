package projection

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/DeluxeOwl/chronicle/event"
	"github.com/DeluxeOwl/chronicle/version"
	"golang.org/x/sync/errgroup"
)

//go:generate go run github.com/matryer/moq@latest -pkg projection_test -skip-ensure -rm -out projection_mock_test.go . Checkpointer Projection

type Projection interface {
	Name() string
	// EventNames returns a slice of glob patterns to match against event names.
	// For example: "account.created", "account.*", or "*" for all events.
	EventNames() []string
	Handle(ctx context.Context, rec *event.GlobalRecord) error
}

type Checkpointer interface {
	GetCheckpoint(ctx context.Context, projectionName string) (version.Version, error)
	SaveCheckpoint(ctx context.Context, projectionName string, v version.Version) error
}

type OnSaveCheckpointErrFunc = func(ctx context.Context, err error) error

type Group struct {
	eventlog     event.GlobalLog
	checkpointer Checkpointer

	// configurable
	pollInterval            time.Duration
	cancelCheckpointTimeout time.Duration
	log                     *slog.Logger
	onSaveCheckpointErrFunc OnSaveCheckpointErrFunc

	managedProjections []managedProjection
	wg                 *errgroup.Group `exhaustruct:"optional"`
}

type GroupOption func(*Group)

func WithPollInterval(interval time.Duration) GroupOption {
	return func(g *Group) {
		g.pollInterval = interval
	}
}

func WithCancelCheckpointTimeout(timeout time.Duration) GroupOption {
	return func(g *Group) {
		g.cancelCheckpointTimeout = timeout
	}
}

func WithSlogHandler(handler slog.Handler) GroupOption {
	return func(g *Group) {
		if handler == nil {
			g.log = slog.New(slog.DiscardHandler)
			return
		}
		g.log = slog.New(handler)
	}
}

func WithOnSaveCheckpointErrFunc(errFunc OnSaveCheckpointErrFunc) GroupOption {
	return func(g *Group) {
		if errFunc == nil {
			return
		}
		g.onSaveCheckpointErrFunc = errFunc
	}
}

type managedProjection struct {
	projection        Projection
	eventNamePatterns []string
	matchesAll        bool // Optimization for the "*" pattern.
}

// isInterested checks if a projection should handle a given event name.
func (mp *managedProjection) isInterested(eventName string) bool {
	if mp.matchesAll {
		return true
	}
	for _, pattern := range mp.eventNamePatterns {
		// We can safely ignore the error from filepath.Match because we validate
		// all patterns when the Group is created in NewGroup. A malformed pattern
		// is a startup-time error, not a runtime one.
		if matched, _ := filepath.Match(pattern, eventName); matched {
			return true
		}
	}
	return false
}

const (
	DefaultPollInterval            = 200 * time.Millisecond
	DefaultCancelCheckpointTimeout = 5 * time.Second
)

var ErrBadPattern = errors.New("bad event name pattern")

// TODO: change this to only run one projection, not a group. Let users handle concurrency.
// TODO: how would a ProjectionTx even make sense? Maybe combine TransactionalAggregateProcessor + another one that doesn't take the events
// TODO: Then rename this to "EventualProjection"? "eventual projection runner"?

// NewGroup creates a new projection group. The pollInterval determines how often each projection checks for new events.
// It returns an error if any projection provides a malformed event name pattern.
func NewGroup(
	log event.GlobalLog,
	checkpointer Checkpointer,
	projections []Projection,
	opts ...GroupOption,
) (*Group, error) {
	g := &Group{
		eventlog:                log,
		checkpointer:            checkpointer,
		pollInterval:            DefaultPollInterval,
		cancelCheckpointTimeout: DefaultCancelCheckpointTimeout,
		log:                     slog.Default(),
		managedProjections:      make([]managedProjection, len(projections)),
		onSaveCheckpointErrFunc: func(ctx context.Context, err error) error { return nil },
	}

	for _, o := range opts {
		o(g)
	}

	for i, p := range projections {
		patterns := p.EventNames()
		if len(patterns) == 0 {
			// A projection that listens to nothing. This is valid.
			//nolint:exhaustruct // not needed.
			g.managedProjections[i] = managedProjection{projection: p}
			continue
		}

		// Validate patterns and check for the special "*" wildcard.
		matchesAll := false
		for _, pattern := range patterns {
			if _, err := filepath.Match(pattern, ""); err != nil {
				return nil, fmt.Errorf(
					"projection %q has an invalid event name pattern %q: %w",
					p.Name(),
					pattern,
					ErrBadPattern,
				)
			}
			if pattern == "*" {
				matchesAll = true
				break // If we match all, no need to check other patterns.
			}
		}

		g.managedProjections[i] = managedProjection{
			projection:        p,
			eventNamePatterns: patterns,
			matchesAll:        matchesAll,
		}
	}

	return g, nil
}

func (g *Group) Run(ctx context.Context) {
	wg, ctx := errgroup.WithContext(ctx)
	g.wg = wg

	for _, mp := range g.managedProjections {
		g.wg.Go(func() error {
			return g.runProjectionLoop(ctx, mp)
		})
	}
}

// Wait blocks until all projections have stopped, either due to an error or context cancellation.
// It returns the first error encountered by any projection.
func (g *Group) Wait() error {
	err := g.wg.Wait()
	// If the context was canceled, the shutdown is considered clean.
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

//nolint:gocognit,funlen // Channels.
func (g *Group) runProjectionLoop(ctx context.Context, mp managedProjection) error {
	pname := mp.projection.Name()
	// A projection with no event names to listen to is a no-op that just gets a checkpoint.
	if len(mp.eventNamePatterns) == 0 {
		g.log.InfoContext(
			ctx,
			"Projection has no event names to handle, will not run.",
			"name",
			pname,
		)
		return nil
	}

	g.log.InfoContext(ctx, "Starting projection", "name", pname, "poll_interval", g.pollInterval)

	currentVersion, err := g.checkpointer.GetCheckpoint(ctx, pname)
	if err != nil {
		return fmt.Errorf("projection %q: failed to get initial checkpoint: %w", pname, err)
	}
	g.log.InfoContext(
		ctx,
		"Loaded initial checkpoint",
		"projection",
		pname,
		"version",
		currentVersion,
	)

	ticker := time.NewTicker(g.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			g.log.InfoContext(
				ctx,
				"Projection stopping, saving final checkpoint",
				"projection",
				pname,
				"version",
				currentVersion,
			)
			saveCtx, cancel := context.WithTimeout(context.Background(), g.cancelCheckpointTimeout)
			defer cancel()
			if err := g.checkpointer.SaveCheckpoint(saveCtx, pname, currentVersion); err != nil {
				g.log.ErrorContext(
					saveCtx,
					"Failed to save final checkpoint on shutdown",
					"projection",
					pname,
					"error",
					err,
				)
				if cerr := g.onSaveCheckpointErrFunc(saveCtx, err); cerr != nil {
					return fmt.Errorf("projection %q: save checkpoint err: %w", pname, cerr)
				}
			}
			return ctx.Err()

		case <-ticker.C:
			var processedInBatch int
			for rec, streamErr := range g.eventlog.ReadAllEvents(ctx, version.SelectFrom(currentVersion)) { // This is a go 1.23+ iterator, NOT a channel
				if streamErr != nil {
					g.log.ErrorContext(
						ctx,
						"Stream error, stopping projection",
						"projection",
						pname,
						"error",
						streamErr,
					)
					return fmt.Errorf("projection %q: stream error: %w", pname, streamErr)
				}

				if mp.isInterested(rec.EventName()) {
					if err := mp.projection.Handle(ctx, rec); err != nil {
						return fmt.Errorf(
							"projection %q: handler failed on event %d: %w",
							pname,
							rec.GlobalVersion(),
							err,
						)
					}
				}

				// Always advance the version, even for events we don't handle.
				currentVersion = rec.GlobalVersion()
				processedInBatch++
			}

			// Only save if we actually processed events.
			if processedInBatch == 0 {
				continue
			}

			if err := g.checkpointer.SaveCheckpoint(ctx, pname, currentVersion); err != nil {
				g.log.ErrorContext(
					ctx,
					"Failed to save checkpoint",
					"projection",
					pname,
					"error",
					err,
				)
				if cerr := g.onSaveCheckpointErrFunc(ctx, err); cerr != nil {
					return fmt.Errorf("projection %q: save checkpoint err: %w", pname, cerr)
				}
				continue
			}

			g.log.DebugContext(
				ctx,
				"Processed batch and saved checkpoint",
				"projection",
				pname,
				"count",
				processedInBatch,
				"new_version",
				currentVersion,
			)
		}
	}
}
