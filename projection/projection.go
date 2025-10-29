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
	EventNames() []string
	Handle(ctx context.Context, rec *event.GlobalRecord) error
}

type Checkpointer interface {
	GetCheckpoint(ctx context.Context, projectionName string) (version.Version, error)
	SaveCheckpoint(ctx context.Context, projectionName string, v version.Version) error
}

type Group struct {
	log          event.GlobalLog
	checkpointer Checkpointer
	pollInterval time.Duration

	managedProjections []managedProjection
	wg                 *errgroup.Group
}

type managedProjection struct {
	projection Projection
	eventNames map[string]struct{}
}

// NewGroup creates a new projection group. The pollInterval determines how often each projection checks for new events.
func NewGroup(log event.GlobalLog, checkpointer Checkpointer, pollInterval time.Duration, projections []Projection) *Group {
	if pollInterval == 0 {
		pollInterval = 250 * time.Millisecond // Default poll interval
	}
	g := &Group{
		log:          log,
		checkpointer: checkpointer,
		pollInterval: pollInterval,
	}

	for _, p := range projections {
		namesSet := map[string]struct{}{}
		for _, name := range p.EventNames() {
			namesSet[name] = struct{}{}
		}
		g.managedProjections = append(g.managedProjections, managedProjection{
			projection: p,
			eventNames: namesSet,
		})
	}

	return g
}

func (g *Group) Run(ctx context.Context) {
	wg, ctx := errgroup.WithContext(ctx)
	g.wg = wg

	for i := range g.managedProjections {
		// Create a new variable for the closure
		p := g.managedProjections[i]
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

func (g *Group) runProjectionLoop(ctx context.Context, mp managedProjection) error {
	pname := mp.projection.Name()
	slog.Info("Starting projection", "name", pname, "poll_interval", g.pollInterval)

	// 1. Get the initial checkpoint before starting the loop.
	currentVersion, err := g.checkpointer.GetCheckpoint(ctx, pname)
	if err != nil {
		return fmt.Errorf("projection %q: failed to get initial checkpoint: %w", pname, err)
	}
	slog.Info("Loaded initial checkpoint", "projection", pname, "version", currentVersion)

	ticker := time.NewTicker(g.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// 2. Context canceled: perform a final checkpoint save and exit cleanly.
			slog.Info("Projection stopping, saving final checkpoint", "projection", pname, "version", currentVersion)
			// Use a background context for the final save to ensure it runs even if the parent ctx is canceled.
			saveCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := g.checkpointer.SaveCheckpoint(saveCtx, pname, currentVersion); err != nil {
				slog.Error("Failed to save final checkpoint on shutdown", "projection", pname, "error", err)
			}
			return ctx.Err()

		case <-ticker.C:
			// 3. This is our main polling loop.
			stream := g.log.ReadAllEvents(ctx, version.SelectFrom(currentVersion))

			var processedInBatch int
			// The `range` here is now fine, as it's contained within a single tick.
			// It will process all available events and then the outer `select` can continue.
			for rec, streamErr := range stream {
				if streamErr != nil {
					// An error from the stream is likely a permanent issue with the event log.
					slog.Error("Stream error, stopping projection", "projection", pname, "error", streamErr)
					return fmt.Errorf("projection %q: stream error: %w", pname, streamErr)
				}

				// If the projection is interested, handle the event.
				if _, interested := mp.eventNames[rec.EventName()]; interested {
					if err := mp.projection.Handle(ctx, rec); err != nil {
						// A handler error is critical. Stop this projection to prevent inconsistent state.
						return fmt.Errorf("projection %q: handler failed on event %d: %w", pname, rec.GlobalVersion(), err)
					}
				}

				// VERY IMPORTANT: Always advance the version, even for events we don't handle.
				currentVersion = rec.GlobalVersion()
				processedInBatch++
			}

			// 4. After a batch is processed, save the new checkpoint.
			// Only save if we actually processed events to avoid unnecessary writes.
			if processedInBatch > 0 {
				if err := g.checkpointer.SaveCheckpoint(ctx, pname, currentVersion); err != nil {
					// Don't stop the whole projection for a checkpoint save error.
					// Log it and we'll retry on the next tick's batch.
					slog.Error("Failed to save checkpoint", "projection", pname, "error", err)
				} else {
					slog.Debug("Processed batch and saved checkpoint", "projection", pname, "count", processedInBatch, "new_version", currentVersion)
				}
			}
		}
	}
}
