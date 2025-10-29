package projection

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/DeluxeOwl/chronicle/event"
	"github.com/DeluxeOwl/chronicle/version"
	"golang.org/x/sync/errgroup"
)

//go:generate go run github.com/matryer/moq@latest -pkg projection_test -skip-ensure -rm -out projection_mock_test.go . Checkpointer Projection

type Projection interface {
	Name() string
	EventNames() []string // TODO regex event names, like account.*.opened, or account-*-opened, or * for everything
	Handle(ctx context.Context, rec *event.GlobalRecord) error
}

type Checkpointer interface {
	GetCheckpoint(ctx context.Context, projectionName string) (version.Version, error)
	SaveCheckpoint(ctx context.Context, projectionName string, v version.Version) error
}

type Group struct {
	eventlog     event.GlobalLog
	checkpointer Checkpointer

	// configurable
	pollInterval            time.Duration
	cancelCheckpointTimeout time.Duration
	log                     *slog.Logger

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

type managedProjection struct {
	projection Projection
	eventNames map[string]struct{}
}

const (
	DefaultPollInterval            = 200 * time.Millisecond
	DefaultCancelCheckpointTimeout = 5 * time.Second
)

// NewGroup creates a new projection group. The pollInterval determines how often each projection checks for new events.
func NewGroup(
	log event.GlobalLog,
	checkpointer Checkpointer,
	projections []Projection,
	opts ...GroupOption,
) *Group {
	g := &Group{
		eventlog:                log,
		checkpointer:            checkpointer,
		pollInterval:            DefaultPollInterval,
		cancelCheckpointTimeout: DefaultCancelCheckpointTimeout,
		log:                     slog.Default(),
		managedProjections:      make([]managedProjection, len(projections)),
	}

	for _, o := range opts {
		o(g)
	}

	for i, p := range projections {
		namesSet := map[string]struct{}{}
		for _, name := range p.EventNames() {
			namesSet[name] = struct{}{}
		}
		g.managedProjections[i] = managedProjection{
			projection: p,
			eventNames: namesSet,
		}
	}

	return g
}

func (g *Group) Run(ctx context.Context) {
	wg, ctx := errgroup.WithContext(ctx)
	g.wg = wg

	for _, p := range g.managedProjections {
		g.wg.Go(func() error {
			return g.runProjectionLoop(ctx, p)
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

//nolint:gocognit // Channels.
func (g *Group) runProjectionLoop(ctx context.Context, mp managedProjection) error {
	pname := mp.projection.Name()
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
			}
			return ctx.Err()

		case <-ticker.C:
			stream := g.eventlog.ReadAllEvents(ctx, version.SelectFrom(currentVersion))

			var processedInBatch int
			for rec, streamErr := range stream {
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

				if _, interested := mp.eventNames[rec.EventName()]; interested {
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
				continue
			}

			g.log.DebugContext(ctx, "Processed batch and saved checkpoint", "projection", pname, "count", processedInBatch, "new_version", currentVersion)
		}
	}
}
