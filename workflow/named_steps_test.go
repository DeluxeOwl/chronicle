package workflow_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DeluxeOwl/chronicle/eventlog"
	"github.com/DeluxeOwl/chronicle/workflow"
	"github.com/stretchr/testify/require"
)

func TestWorkflow_NamedStepIsVisibleWhileItRunsAndReplaysItsResult(t *testing.T) {
	log := eventlog.NewMemory()
	clock := newClock(time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC))
	runner, err := workflow.NewRunner(log, nil, workflow.WithNowFunc(clock.Now))
	require.NoError(t, err)
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var calls atomic.Int32
	wf := workflow.New(
		runner,
		"order",
		func(wctx *workflow.Context, _ *struct{}) (*WorkerTestOutput, error) {
			value, err := workflow.Step(
				wctx,
				func(ctx context.Context) (string, error) {
					calls.Add(1)
					select {
					case entered <- struct{}{}:
					default:
					}
					select {
					case <-release:
						return "charged", nil
					case <-ctx.Done():
						return "", ctx.Err()
					}
				},
				workflow.WithStepName("Charge payment"),
			)
			if err != nil {
				return nil, err
			}
			if err := workflow.Sleep(wctx, time.Hour); err != nil {
				return nil, err
			}
			return &WorkerTestOutput{Result: value}, nil
		},
	)
	id, err := wf.Start(t.Context(), &struct{}{})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	var runErr error
	go func() { defer close(done); _, runErr = wf.Run(ctx, id) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("named step execution did not stop")
		}
	})
	select {
	case <-entered:
	case <-done:
		t.Fatalf("execution exited before the callback: %v", runErr)
	case <-time.After(time.Second):
		t.Fatal("named step callback was not entered")
	}
	inspector, err := workflow.NewInspector(log, workflow.WithInspectorNowFunc(clock.Now))
	require.NoError(t, err)
	detail, err := inspector.Get(t.Context(), id)
	require.NoError(t, err)
	require.Len(t, detail.Steps, 1)
	require.Equal(t, "Charge payment", detail.Steps[0].Name)
	require.Equal(t, "running", detail.Steps[0].Status)
	require.NotNil(t, detail.Steps[0].StartedAt)
	clock.Advance(time.Minute)
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("workflow did not checkpoint its named result")
	}
	require.ErrorIs(t, runErr, workflow.ErrWorkflowSleeping)
	detail, err = inspector.Get(t.Context(), id)
	require.NoError(t, err)
	require.Equal(t, "completed", detail.Steps[0].Status)
	require.Equal(t, time.Date(2026, 1, 1, 12, 1, 0, 0, time.UTC), *detail.Steps[0].CompletedAt)
	clock.Advance(time.Hour)
	// A nonterminal resume must traverse Step, rather than returning a cached
	// workflow output from Run's terminal-state guard.
	result, err := wf.Run(t.Context(), id)
	require.NoError(t, err)
	require.Equal(t, "charged", result.Result)
	require.Equal(t, int32(1), calls.Load())
}
