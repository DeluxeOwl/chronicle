package workflow_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DeluxeOwl/chronicle/workflow"
	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/require"
)

type EventTestParams struct {
	OrderID string `json:"orderID"`
}

type EventTestOutput struct {
	Result string `json:"result"`
}

type ShipmentPayload struct {
	TrackingNumber string `json:"tracking_number"`
}

func TestAwaitEvent_BasicFlow(t *testing.T) {
	db := setupTestDB(t)
	runner, err := workflow.NewSqliteRunner(db)
	require.NoError(t, err)

	wf := workflow.New(runner, "await-basic", func(wctx *workflow.Context, params *EventTestParams) (*EventTestOutput, error) {
		// Step 1: do some work
		_, err := workflow.Step(wctx, func(ctx context.Context) (string, error) {
			return "prepared", nil
		})
		if err != nil {
			return nil, err
		}

		// Wait for shipment event
		shipment, err := workflow.AwaitEvent[ShipmentPayload](wctx, "order.shipped:"+params.OrderID)
		if err != nil {
			return nil, err
		}

		return &EventTestOutput{Result: "shipped-" + shipment.TrackingNumber}, nil
	})

	ctx := t.Context()
	instanceID, err := wf.Start(ctx, &EventTestParams{OrderID: "order-42"})
	require.NoError(t, err)

	// First run: step 1 succeeds, hits AwaitEvent → parked
	_, err = wf.Run(ctx, instanceID)
	require.ErrorIs(t, err, workflow.ErrWorkflowWaiting)

	// Verify status
	info, err := wf.GetStatus(ctx, instanceID)
	require.NoError(t, err)
	require.Equal(t, workflow.StatusWaiting, info.Status)

	// Emit the event
	err = workflow.EmitEvent(ctx, runner, "order.shipped:order-42", ShipmentPayload{
		TrackingNumber: "XYZ-123",
	})
	require.NoError(t, err)

	// Re-run: AwaitEvent resolves with the payload, workflow completes
	output, err := wf.Run(ctx, instanceID)
	require.NoError(t, err)
	require.Equal(t, "shipped-XYZ-123", output.Result)
}

func TestAwaitEvent_EventEmittedBeforeAwait(t *testing.T) {
	// If the event is emitted before the workflow reaches AwaitEvent,
	// the workflow should still get the payload.
	db := setupTestDB(t)
	runner, err := workflow.NewSqliteRunner(db)
	require.NoError(t, err)

	wf := workflow.New(runner, "await-pre-emit", func(wctx *workflow.Context, params *EventTestParams) (*EventTestOutput, error) {
		shipment, err := workflow.AwaitEvent[ShipmentPayload](wctx, "pre-emit-event")
		if err != nil {
			return nil, err
		}
		return &EventTestOutput{Result: shipment.TrackingNumber}, nil
	})

	ctx := t.Context()

	// Emit the event FIRST
	err = workflow.EmitEvent(ctx, runner, "pre-emit-event", ShipmentPayload{
		TrackingNumber: "EARLY-456",
	})
	require.NoError(t, err)

	// Start and run the workflow
	instanceID, err := wf.Start(ctx, &EventTestParams{})
	require.NoError(t, err)

	// First run hits AwaitEvent → parked (event not yet linked to this instance's step)
	_, err = wf.Run(ctx, instanceID)
	require.ErrorIs(t, err, workflow.ErrWorkflowWaiting)

	// Second run: the Register call saw the emitted event, so GetEvent returns it
	output, err := wf.Run(ctx, instanceID)
	require.NoError(t, err)
	require.Equal(t, "EARLY-456", output.Result)
}

func TestAwaitEvent_WithTimeout_TimesOut(t *testing.T) {
	db := setupTestDB(t)
	clock := newClock(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	runner, err := workflow.NewSqliteRunner(db, workflow.WithNowFunc(clock.Now))
	require.NoError(t, err)

	wf := workflow.New(runner, "await-timeout", func(wctx *workflow.Context, params *EventTestParams) (*EventTestOutput, error) {
		_, err := workflow.AwaitEvent[ShipmentPayload](wctx, "never-arrives", workflow.AwaitEventOptions{
			Timeout: 1 * time.Hour,
		})
		if err != nil {
			return nil, err
		}
		return &EventTestOutput{Result: "unreachable"}, nil
	})

	ctx := t.Context()
	instanceID, err := wf.Start(ctx, &EventTestParams{})
	require.NoError(t, err)

	// Run 1: hits AwaitEvent → parked
	_, err = wf.Run(ctx, instanceID)
	require.ErrorIs(t, err, workflow.ErrWorkflowWaiting)

	// Advance clock past the timeout
	clock.Advance(2 * time.Hour)

	// Run 2: timeout expired → ErrEventTimeout
	_, err = wf.Run(ctx, instanceID)
	require.ErrorIs(t, err, workflow.ErrEventTimeout)
}

func TestAwaitEvent_WithTimeout_EventArrivesBeforeTimeout(t *testing.T) {
	db := setupTestDB(t)
	clock := newClock(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	runner, err := workflow.NewSqliteRunner(db, workflow.WithNowFunc(clock.Now))
	require.NoError(t, err)

	wf := workflow.New(runner, "await-timeout-ok", func(wctx *workflow.Context, params *EventTestParams) (*EventTestOutput, error) {
		shipment, err := workflow.AwaitEvent[ShipmentPayload](wctx, "arrives-in-time", workflow.AwaitEventOptions{
			Timeout: 24 * time.Hour,
		})
		if err != nil {
			return nil, err
		}
		return &EventTestOutput{Result: shipment.TrackingNumber}, nil
	})

	ctx := t.Context()
	instanceID, err := wf.Start(ctx, &EventTestParams{})
	require.NoError(t, err)

	_, err = wf.Run(ctx, instanceID)
	require.ErrorIs(t, err, workflow.ErrWorkflowWaiting)

	// Emit before timeout
	clock.Advance(12 * time.Hour)
	err = workflow.EmitEvent(ctx, runner, "arrives-in-time", ShipmentPayload{TrackingNumber: "ON-TIME"})
	require.NoError(t, err)

	output, err := wf.Run(ctx, instanceID)
	require.NoError(t, err)
	require.Equal(t, "ON-TIME", output.Result)
}

func TestAwaitEvent_MultipleWorkflowsWaitSameEvent(t *testing.T) {
	db := setupTestDB(t)
	runner, err := workflow.NewSqliteRunner(db)
	require.NoError(t, err)

	wf := workflow.New(runner, "await-multi", func(wctx *workflow.Context, params *EventTestParams) (*EventTestOutput, error) {
		shipment, err := workflow.AwaitEvent[ShipmentPayload](wctx, "broadcast-event")
		if err != nil {
			return nil, err
		}
		return &EventTestOutput{Result: params.OrderID + ":" + shipment.TrackingNumber}, nil
	})

	ctx := t.Context()

	id1, err := wf.Start(ctx, &EventTestParams{OrderID: "wf-1"})
	require.NoError(t, err)
	id2, err := wf.Start(ctx, &EventTestParams{OrderID: "wf-2"})
	require.NoError(t, err)

	// Both park on AwaitEvent
	_, err = wf.Run(ctx, id1)
	require.ErrorIs(t, err, workflow.ErrWorkflowWaiting)
	_, err = wf.Run(ctx, id2)
	require.ErrorIs(t, err, workflow.ErrWorkflowWaiting)

	// Emit once
	err = workflow.EmitEvent(ctx, runner, "broadcast-event", ShipmentPayload{TrackingNumber: "SHARED"})
	require.NoError(t, err)

	// Both complete
	out1, err := wf.Run(ctx, id1)
	require.NoError(t, err)
	require.Equal(t, "wf-1:SHARED", out1.Result)

	out2, err := wf.Run(ctx, id2)
	require.NoError(t, err)
	require.Equal(t, "wf-2:SHARED", out2.Result)
}

func TestAwaitEvent_FirstEmitWins(t *testing.T) {
	db := setupTestDB(t)
	runner, err := workflow.NewSqliteRunner(db)
	require.NoError(t, err)

	wf := workflow.New(runner, "await-idempotent", func(wctx *workflow.Context, params *EventTestParams) (*EventTestOutput, error) {
		shipment, err := workflow.AwaitEvent[ShipmentPayload](wctx, "idempotent-event")
		if err != nil {
			return nil, err
		}
		return &EventTestOutput{Result: shipment.TrackingNumber}, nil
	})

	ctx := t.Context()
	instanceID, err := wf.Start(ctx, &EventTestParams{})
	require.NoError(t, err)

	_, err = wf.Run(ctx, instanceID)
	require.ErrorIs(t, err, workflow.ErrWorkflowWaiting)

	// Emit first time
	err = workflow.EmitEvent(ctx, runner, "idempotent-event", ShipmentPayload{TrackingNumber: "FIRST"})
	require.NoError(t, err)

	// Emit second time — should be ignored
	err = workflow.EmitEvent(ctx, runner, "idempotent-event", ShipmentPayload{TrackingNumber: "SECOND"})
	require.NoError(t, err)

	output, err := wf.Run(ctx, instanceID)
	require.NoError(t, err)
	require.Equal(t, "FIRST", output.Result, "first emit should win")
}

func TestAwaitEvent_WithStepsAroundIt(t *testing.T) {
	db := setupTestDB(t)
	runner, err := workflow.NewSqliteRunner(db)
	require.NoError(t, err)

	var step1Count, step2Count atomic.Int32

	wf := workflow.New(runner, "await-steps-around", func(wctx *workflow.Context, params *EventTestParams) (*EventTestOutput, error) {
		val, err := workflow.Step(wctx, func(ctx context.Context) (string, error) {
			step1Count.Add(1)
			return "before", nil
		})
		if err != nil {
			return nil, err
		}

		shipment, err := workflow.AwaitEvent[ShipmentPayload](wctx, "middle-event")
		if err != nil {
			return nil, err
		}

		result, err := workflow.Step(wctx, func(ctx context.Context) (string, error) {
			step2Count.Add(1)
			return val + "-" + shipment.TrackingNumber + "-after", nil
		})
		if err != nil {
			return nil, err
		}

		return &EventTestOutput{Result: result}, nil
	})

	ctx := t.Context()
	instanceID, err := wf.Start(ctx, &EventTestParams{})
	require.NoError(t, err)

	// Run 1: step 1 executes, AwaitEvent parks
	_, err = wf.Run(ctx, instanceID)
	require.ErrorIs(t, err, workflow.ErrWorkflowWaiting)
	require.Equal(t, int32(1), step1Count.Load())
	require.Equal(t, int32(0), step2Count.Load())

	// Emit
	err = workflow.EmitEvent(ctx, runner, "middle-event", ShipmentPayload{TrackingNumber: "MID"})
	require.NoError(t, err)

	// Run 2: step 1 replays, AwaitEvent resolves, step 2 executes
	output, err := wf.Run(ctx, instanceID)
	require.NoError(t, err)
	require.Equal(t, "before-MID-after", output.Result)
	require.Equal(t, int32(1), step1Count.Load(), "step 1 should not re-execute")
	require.Equal(t, int32(1), step2Count.Load())
}

func TestAwaitEvent_WithSleep(t *testing.T) {
	db := setupTestDB(t)
	clock := newClock(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	runner, err := workflow.NewSqliteRunner(db, workflow.WithNowFunc(clock.Now))
	require.NoError(t, err)

	wf := workflow.New(runner, "await-with-sleep", func(wctx *workflow.Context, params *EventTestParams) (*EventTestOutput, error) {
		// Sleep first
		if err := workflow.Sleep(wctx, 1*time.Hour); err != nil {
			return nil, err
		}

		// Then await event
		shipment, err := workflow.AwaitEvent[ShipmentPayload](wctx, "post-sleep-event")
		if err != nil {
			return nil, err
		}

		return &EventTestOutput{Result: "slept-then-" + shipment.TrackingNumber}, nil
	})

	ctx := t.Context()
	instanceID, err := wf.Start(ctx, &EventTestParams{})
	require.NoError(t, err)

	// Run 1: hits sleep
	_, err = wf.Run(ctx, instanceID)
	require.ErrorIs(t, err, workflow.ErrWorkflowSleeping)

	// Advance past sleep
	clock.Advance(2 * time.Hour)

	// Run 2: sleep elapsed, hits AwaitEvent → parked
	_, err = wf.Run(ctx, instanceID)
	require.ErrorIs(t, err, workflow.ErrWorkflowWaiting)

	// Emit
	err = workflow.EmitEvent(ctx, runner, "post-sleep-event", ShipmentPayload{TrackingNumber: "AFTER"})
	require.NoError(t, err)

	// Run 3: everything replays, completes
	output, err := wf.Run(ctx, instanceID)
	require.NoError(t, err)
	require.Equal(t, "slept-then-AFTER", output.Result)
}

func TestAwaitEvent_WorkerDriven(t *testing.T) {
	db := setupTestDB(t)
	runner, err := workflow.NewSqliteRunner(db)
	require.NoError(t, err)

	wf := workflow.New(runner, "await-worker", func(wctx *workflow.Context, params *EventTestParams) (*EventTestOutput, error) {
		shipment, err := workflow.AwaitEvent[ShipmentPayload](wctx, "worker-event")
		if err != nil {
			return nil, err
		}
		return &EventTestOutput{Result: shipment.TrackingNumber}, nil
	})

	ctx := t.Context()
	instanceID, err := wf.Start(ctx, &EventTestParams{})
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

	// Wait for the worker to park the workflow on AwaitEvent
	require.Eventually(t, func() bool {
		info, err := wf.GetStatus(ctx, instanceID)
		return err == nil && info.Status == workflow.StatusWaiting
	}, 2*time.Second, 20*time.Millisecond)

	// Emit the event — this enqueues a wake-up task
	err = workflow.EmitEvent(ctx, runner, "worker-event", ShipmentPayload{TrackingNumber: "WORKER-OK"})
	require.NoError(t, err)

	// Worker picks up the wake-up task and completes the workflow
	require.Eventually(t, func() bool {
		result, err := wf.GetResult(ctx, instanceID)
		return err == nil && result.Result == "WORKER-OK"
	}, 2*time.Second, 20*time.Millisecond)

	cancel()
	<-done
}

func TestAwaitEvent_WithRetry(t *testing.T) {
	// Workflow: AwaitEvent → step that fails → retry → AwaitEvent replays (resolved) → step succeeds
	db := setupTestDB(t)
	clock := newClock(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	runner, err := workflow.NewSqliteRunner(db, workflow.WithNowFunc(clock.Now))
	require.NoError(t, err)

	var stepCount atomic.Int32

	wf := workflow.New(runner, "await-retry", func(wctx *workflow.Context, params *EventTestParams) (*EventTestOutput, error) {
		shipment, err := workflow.AwaitEvent[ShipmentPayload](wctx, "retry-event")
		if err != nil {
			return nil, err
		}

		result, err := workflow.Step(wctx, func(ctx context.Context) (string, error) {
			n := stepCount.Add(1)
			if n == 1 {
				return "", errors.New("transient step failure")
			}
			return shipment.TrackingNumber + "-processed", nil
		})
		if err != nil {
			return nil, err
		}

		return &EventTestOutput{Result: result}, nil
	})

	ctx := t.Context()
	instanceID, err := wf.Start(ctx, &EventTestParams{}, workflow.WithRetryStrategy(workflow.RetryStrategy{
		MaxAttempts: 3,
		BaseDelay:   1 * time.Second,
		Factor:      1.0,
		MaxDelay:    1 * time.Minute,
	}))
	require.NoError(t, err)

	// Run 1: parks on AwaitEvent
	_, err = wf.Run(ctx, instanceID)
	require.ErrorIs(t, err, workflow.ErrWorkflowWaiting)

	// Emit event
	err = workflow.EmitEvent(ctx, runner, "retry-event", ShipmentPayload{TrackingNumber: "RETRY"})
	require.NoError(t, err)

	// Run 2: AwaitEvent resolves, step fails → retry
	_, err = wf.Run(ctx, instanceID)
	require.ErrorIs(t, err, workflow.ErrWorkflowRetrying)
	require.Equal(t, int32(1), stepCount.Load())

	clock.Advance(2 * time.Second)

	// Run 3: AwaitEvent replays (resolved), step succeeds
	output, err := wf.Run(ctx, instanceID)
	require.NoError(t, err)
	require.Equal(t, "RETRY-processed", output.Result)
}

func TestAwaitEvent_ComplexPayload(t *testing.T) {
	db := setupTestDB(t)
	runner, err := workflow.NewSqliteRunner(db)
	require.NoError(t, err)

	type ComplexPayload struct {
		Items []string       `json:"items"`
		Meta  map[string]int `json:"meta"`
	}

	wf := workflow.New(runner, "await-complex", func(wctx *workflow.Context, params *EventTestParams) (*EventTestOutput, error) {
		payload, err := workflow.AwaitEvent[ComplexPayload](wctx, "complex-event")
		if err != nil {
			return nil, err
		}
		return &EventTestOutput{Result: payload.Items[0]}, nil
	})

	ctx := t.Context()
	instanceID, err := wf.Start(ctx, &EventTestParams{})
	require.NoError(t, err)

	_, err = wf.Run(ctx, instanceID)
	require.ErrorIs(t, err, workflow.ErrWorkflowWaiting)

	err = workflow.EmitEvent(ctx, runner, "complex-event", ComplexPayload{
		Items: []string{"first", "second"},
		Meta:  map[string]int{"count": 2},
	})
	require.NoError(t, err)

	output, err := wf.Run(ctx, instanceID)
	require.NoError(t, err)
	require.Equal(t, "first", output.Result)
}

func TestAwaitEvent_CompletedWorkflowDoesNotReAwait(t *testing.T) {
	db := setupTestDB(t)
	runner, err := workflow.NewSqliteRunner(db)
	require.NoError(t, err)

	wf := workflow.New(runner, "await-completed", func(wctx *workflow.Context, params *EventTestParams) (*EventTestOutput, error) {
		shipment, err := workflow.AwaitEvent[ShipmentPayload](wctx, "once-event")
		if err != nil {
			return nil, err
		}
		return &EventTestOutput{Result: shipment.TrackingNumber}, nil
	})

	ctx := t.Context()
	instanceID, err := wf.Start(ctx, &EventTestParams{})
	require.NoError(t, err)

	_, err = wf.Run(ctx, instanceID)
	require.ErrorIs(t, err, workflow.ErrWorkflowWaiting)

	err = workflow.EmitEvent(ctx, runner, "once-event", ShipmentPayload{TrackingNumber: "DONE"})
	require.NoError(t, err)

	output1, err := wf.Run(ctx, instanceID)
	require.NoError(t, err)
	require.Equal(t, "DONE", output1.Result)

	// Running again should return cached result
	output2, err := wf.Run(ctx, instanceID)
	require.NoError(t, err)
	require.Equal(t, "DONE", output2.Result)
}

func TestAwaitEvent_TimeoutWithRetry(t *testing.T) {
	// When an event times out, it's a real error that can be retried
	db := setupTestDB(t)
	clock := newClock(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	runner, err := workflow.NewSqliteRunner(db, workflow.WithNowFunc(clock.Now))
	require.NoError(t, err)

	wf := workflow.New(runner, "timeout-retry", func(wctx *workflow.Context, params *EventTestParams) (*EventTestOutput, error) {
		_, err := workflow.AwaitEvent[ShipmentPayload](wctx, "timeout-event", workflow.AwaitEventOptions{
			Timeout: 1 * time.Hour,
		})
		if err != nil {
			return nil, err
		}
		return &EventTestOutput{Result: "should-not-reach"}, nil
	})

	ctx := t.Context()
	instanceID, err := wf.Start(ctx, &EventTestParams{}, workflow.WithRetryStrategy(workflow.RetryStrategy{
		MaxAttempts: 2,
		BaseDelay:   1 * time.Second,
		Factor:      1.0,
		MaxDelay:    1 * time.Minute,
	}))
	require.NoError(t, err)

	// Run 1: parks on AwaitEvent
	_, err = wf.Run(ctx, instanceID)
	require.ErrorIs(t, err, workflow.ErrWorkflowWaiting)

	// Advance past timeout
	clock.Advance(2 * time.Hour)

	// Run 2: timeout → ErrEventTimeout → retry kicks in
	_, err = wf.Run(ctx, instanceID)
	require.ErrorIs(t, err, workflow.ErrWorkflowRetrying)

	// After max attempts, should fail permanently
	clock.Advance(2 * time.Second)
	_, err = wf.Run(ctx, instanceID)
	// This time the step result is already cached as timed out
	require.Error(t, err)
}
