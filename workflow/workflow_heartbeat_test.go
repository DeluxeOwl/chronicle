package workflow_test

import (
	"context"
	"testing"
	"time"

	"github.com/DeluxeOwl/chronicle/workflow"
	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/require"
)

func TestHeartbeat_NoOpWithMemoryQueue(t *testing.T) {
	db := setupTestDB(t)
	runner, err := workflow.NewSqliteRunner(db)
	require.NoError(t, err)

	type Out struct {
		Result string `json:"result"`
	}

	wf := workflow.New(runner, "heartbeat-noop", func(wctx *workflow.Context, params *struct{}) (*Out, error) {
		result, err := workflow.Step(wctx, func(ctx context.Context) (string, error) {
			// Call heartbeat inside a step — should be a no-op with MemoryQueue
			if err := workflow.Heartbeat(wctx, 5*time.Minute); err != nil {
				return "", err
			}
			return "heartbeat-ok", nil
		})
		if err != nil {
			return nil, err
		}
		return &Out{Result: result}, nil
	})

	ctx := t.Context()
	instanceID, err := wf.Start(ctx, &struct{}{})
	require.NoError(t, err)

	output, err := wf.Run(ctx, instanceID)
	require.NoError(t, err)
	require.Equal(t, "heartbeat-ok", output.Result)
}

func TestHeartbeat_MultipleCallsInStep(t *testing.T) {
	db := setupTestDB(t)
	runner, err := workflow.NewSqliteRunner(db)
	require.NoError(t, err)

	type Out struct {
		Iterations int `json:"iterations"`
	}

	wf := workflow.New(runner, "heartbeat-multi", func(wctx *workflow.Context, params *struct{}) (*Out, error) {
		count, err := workflow.Step(wctx, func(ctx context.Context) (int, error) {
			for i := range 10 {
				_ = i
				if err := workflow.Heartbeat(wctx, 1*time.Minute); err != nil {
					return 0, err
				}
			}
			return 10, nil
		})
		if err != nil {
			return nil, err
		}
		return &Out{Iterations: count}, nil
	})

	ctx := t.Context()
	instanceID, err := wf.Start(ctx, &struct{}{})
	require.NoError(t, err)

	output, err := wf.Run(ctx, instanceID)
	require.NoError(t, err)
	require.Equal(t, 10, output.Iterations)
}

func TestHeartbeat_WorkerDriven(t *testing.T) {
	db := setupTestDB(t)
	runner, err := workflow.NewSqliteRunner(db)
	require.NoError(t, err)

	type Out struct {
		Result string `json:"result"`
	}

	wf := workflow.New(runner, "heartbeat-worker", func(wctx *workflow.Context, params *struct{}) (*Out, error) {
		result, err := workflow.Step(wctx, func(ctx context.Context) (string, error) {
			// Simulate long-running work with heartbeats
			for range 3 {
				if err := workflow.Heartbeat(wctx, 2*time.Minute); err != nil {
					return "", err
				}
			}
			return "worker-heartbeat-ok", nil
		})
		if err != nil {
			return nil, err
		}
		return &Out{Result: result}, nil
	})

	ctx := t.Context()
	instanceID, err := wf.Start(ctx, &struct{}{})
	require.NoError(t, err)

	workerCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = runner.RunWorker(workerCtx, workflow.WorkerOptions{
			PollInterval: 10 * time.Millisecond,
		})
	}()

	require.Eventually(t, func() bool {
		result, err := wf.GetResult(ctx, instanceID)
		return err == nil && result.Result == "worker-heartbeat-ok"
	}, 2*time.Second, 20*time.Millisecond)

	cancel()
	<-done
}
