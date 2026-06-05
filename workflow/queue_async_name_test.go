package workflow_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/DeluxeOwl/chronicle/event"
	"github.com/DeluxeOwl/chronicle/eventlog"
	"github.com/DeluxeOwl/chronicle/version"
	"github.com/DeluxeOwl/chronicle/workflow"
	"github.com/stretchr/testify/require"
)

// TestAsyncQueue_WorkflowNameFromEvent_StepCompleted verifies that when the
// AsyncQueue processes a stepCompleted (sleep) event, it takes the workflow
// name from the event payload rather than falling back to the instance ID.
// This is bug #2: without the fix, if the ready_tasks row doesn't exist at
// query time, the fallback was `string(rec.LogID())` which is the instance ID,
// causing the worker to permanently fail the task as "unknown workflow".
func TestAsyncQueue_WorkflowNameFromEvent_StepCompleted(t *testing.T) {
	db := setupAsyncDB(t)
	q, err := workflow.NewAsyncQueue(db)
	require.NoError(t, err)

	// Create a memory event log to produce real GlobalRecord values.
	memLog := eventlog.NewMemory()
	ctx := t.Context()

	instanceID := "wf-test-sleep-name"

	// 1) Append a workflowStarted event (so the stream exists).
	startedPayload, _ := json.Marshal(map[string]any{
		"instanceID":   instanceID,
		"workflowName": "my-sleep-wf",
		"params":       nil,
		"startedAt":    time.Now().Format(time.RFC3339Nano),
	})
	startedRaw := event.NewRaw("workflow/started", startedPayload)
	_, err = memLog.AppendEvents(
		ctx,
		event.LogID(instanceID),
		version.CheckExact(0),
		event.RawEvents{startedRaw},
	)
	require.NoError(t, err)

	// 2) Append a stepCompleted (sleep) event WITH the workflowName field.
	sleepResult, _ := json.Marshal(map[string]any{
		"wakeAt":   time.Now().Add(1 * time.Hour).Format(time.RFC3339Nano),
		"duration": int64(1 * time.Hour),
	})
	stepPayload, _ := json.Marshal(map[string]any{
		"stepIndex":    0,
		"result":       json.RawMessage(sleepResult),
		"completedAt":  time.Now().Format(time.RFC3339Nano),
		"workflowName": "my-sleep-wf",
	})
	stepRaw := event.NewRaw("workflow/step_completed", stepPayload)
	_, err = memLog.AppendEvents(
		ctx,
		event.LogID(instanceID),
		version.CheckExact(1),
		event.RawEvents{stepRaw},
	)
	require.NoError(t, err)

	// 3) Delete any ready_tasks row that workflowStarted's Handle may have created,
	//    simulating the case where the row doesn't exist (e.g. projection reprocessing
	//    after completion).
	_, err = db.Exec(`DELETE FROM workflow_ready_tasks WHERE instance_id = ?`, instanceID)
	require.NoError(t, err)

	// 4) Process only the stepCompleted event through the AsyncQueue.
	//    Read the global stream and find our stepCompleted record.
	var stepRecord *event.GlobalRecord
	for rec, err := range memLog.ReadAllEvents(ctx, version.SelectFromBeginning) {
		require.NoError(t, err)
		if rec.EventName() == "workflow/step_completed" && string(rec.LogID()) == instanceID {
			stepRecord = rec
			break
		}
	}
	require.NotNil(t, stepRecord, "should find the stepCompleted GlobalRecord")

	// 5) Handle just the stepCompleted event.
	err = q.Handle(ctx, stepRecord)
	require.NoError(t, err)

	// 6) Verify the task was created with the correct workflow name.
	var workflowName string
	err = db.QueryRow(
		`SELECT workflow_name FROM workflow_ready_tasks WHERE instance_id = ?`,
		instanceID,
	).Scan(&workflowName)
	require.NoError(t, err)
	require.Equal(t, "my-sleep-wf", workflowName,
		"should use workflow name from event payload, not instance ID")
}

// TestAsyncQueue_WorkflowNameFromEvent_Retried verifies that when the
// AsyncQueue processes a workflowRetried event, it takes the workflow name
// from the event payload.
func TestAsyncQueue_WorkflowNameFromEvent_Retried(t *testing.T) {
	db := setupAsyncDB(t)
	q, err := workflow.NewAsyncQueue(db)
	require.NoError(t, err)

	memLog := eventlog.NewMemory()
	ctx := t.Context()

	instanceID := "wf-test-retry-name"

	// Append a workflowRetried event with workflowName in the payload.
	retriedPayload, _ := json.Marshal(map[string]any{
		"attempt":       2,
		"nextRunAfter":  time.Now().Add(5 * time.Second).Format(time.RFC3339Nano),
		"backoffDelay":  int64(5 * time.Second),
		"previousError": "some error",
		"retryStrategy": json.RawMessage(
			`{"maxAttempts":3,"baseDelay":1000000000,"factor":2,"maxDelay":60000000000}`,
		),
		"workflowName": "my-retry-wf",
	})
	raw := event.NewRaw("workflow/retried", retriedPayload)
	_, err = memLog.AppendEvents(
		ctx,
		event.LogID(instanceID),
		version.CheckExact(0),
		event.RawEvents{raw},
	)
	require.NoError(t, err)

	// No ready_tasks row exists — the fallback would be the instance ID.
	var rec *event.GlobalRecord
	for r, err := range memLog.ReadAllEvents(ctx, version.SelectFromBeginning) {
		require.NoError(t, err)
		if r.EventName() == "workflow/retried" {
			rec = r
			break
		}
	}
	require.NotNil(t, rec)

	err = q.Handle(ctx, rec)
	require.NoError(t, err)

	var workflowName string
	err = db.QueryRow(
		`SELECT workflow_name FROM workflow_ready_tasks WHERE instance_id = ?`,
		instanceID,
	).Scan(&workflowName)
	require.NoError(t, err)
	require.Equal(t, "my-retry-wf", workflowName,
		"should use workflow name from retried event, not instance ID")
}

// TestAsyncQueue_WorkflowNameFromEvent_EventReceived verifies that when the
// AsyncQueue processes a workflowEventReceived event, it takes the workflow
// name from the event payload.
func TestAsyncQueue_WorkflowNameFromEvent_EventReceived(t *testing.T) {
	db := setupAsyncDB(t)
	q, err := workflow.NewAsyncQueue(db)
	require.NoError(t, err)

	memLog := eventlog.NewMemory()
	ctx := t.Context()

	instanceID := "wf-test-evtrecv-name"

	// Append a workflowEventReceived event with workflowName in the payload.
	payload, _ := json.Marshal(map[string]any{
		"stepIndex":     0,
		"receivedEvent": "payment-received",
		"payload":       json.RawMessage(`{"amount":100}`),
		"workflowName":  "my-event-wf",
	})
	raw := event.NewRaw("workflow/event_received", payload)
	_, err = memLog.AppendEvents(
		ctx,
		event.LogID(instanceID),
		version.CheckExact(0),
		event.RawEvents{raw},
	)
	require.NoError(t, err)

	// No ready_tasks row exists.
	var rec *event.GlobalRecord
	for r, err := range memLog.ReadAllEvents(ctx, version.SelectFromBeginning) {
		require.NoError(t, err)
		if r.EventName() == "workflow/event_received" {
			rec = r
			break
		}
	}
	require.NotNil(t, rec)

	err = q.Handle(ctx, rec)
	require.NoError(t, err)

	var workflowName string
	err = db.QueryRow(
		`SELECT workflow_name FROM workflow_ready_tasks WHERE instance_id = ?`,
		instanceID,
	).Scan(&workflowName)
	require.NoError(t, err)
	require.Equal(t, "my-event-wf", workflowName,
		"should use workflow name from event_received event, not instance ID")
}

// TestAsyncQueue_WorkflowNameFallback_OldEvents verifies backward compatibility:
// when events don't carry a workflowName field (old format), the AsyncQueue
// falls back to the DB lookup and ultimately to the instance ID.
func TestAsyncQueue_WorkflowNameFallback_OldEvents(t *testing.T) {
	db := setupAsyncDB(t)
	q, err := workflow.NewAsyncQueue(db)
	require.NoError(t, err)

	memLog := eventlog.NewMemory()
	ctx := t.Context()

	instanceID := "wf-test-old-format"

	// Append a stepCompleted (sleep) event WITHOUT the workflowName field
	// (simulating an old event format).
	sleepResult, _ := json.Marshal(map[string]any{
		"wakeAt":   time.Now().Add(1 * time.Hour).Format(time.RFC3339Nano),
		"duration": int64(1 * time.Hour),
	})
	stepPayload, _ := json.Marshal(map[string]any{
		"stepIndex": 0,
		"result":    json.RawMessage(sleepResult),
		// No workflowName field — old format
	})
	raw := event.NewRaw("workflow/step_completed", stepPayload)
	_, err = memLog.AppendEvents(
		ctx,
		event.LogID(instanceID),
		version.CheckExact(0),
		event.RawEvents{raw},
	)
	require.NoError(t, err)

	// Pre-insert a ready_tasks row to simulate the DB having the correct name.
	_, err = db.Exec(
		`INSERT INTO workflow_ready_tasks (instance_id, workflow_name, run_after_ns) VALUES (?, ?, 0)`,
		instanceID,
		"correct-wf-name",
	)
	require.NoError(t, err)

	var rec *event.GlobalRecord
	for r, err := range memLog.ReadAllEvents(ctx, version.SelectFromBeginning) {
		require.NoError(t, err)
		rec = r
		break
	}
	require.NotNil(t, rec)

	err = q.Handle(ctx, rec)
	require.NoError(t, err)

	var workflowName string
	err = db.QueryRow(
		`SELECT workflow_name FROM workflow_ready_tasks WHERE instance_id = ?`,
		instanceID,
	).Scan(&workflowName)
	require.NoError(t, err)
	require.Equal(t, "correct-wf-name", workflowName,
		"should fall back to DB lookup for old events without workflowName")
}

// TestAsyncQueue_EndToEnd_WorkflowNamePreserved runs a full async workflow
// with sleep and verifies the worker can pick it up after the sleep elapses
// (proving the workflow name is correctly stored in the task queue).
func TestAsyncQueue_EndToEnd_WorkflowNamePreserved(t *testing.T) {
	clock := newClock(time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC))

	runner, cleanup := setupAsyncRunner(t, workflow.WithNowFunc(clock.Now))
	defer cleanup()

	wf := workflow.New(
		runner,
		"name-preserved-wf",
		func(wctx *workflow.Context, params *WorkerTestParams) (*WorkerTestOutput, error) {
			_, err := workflow.Step(wctx, func(ctx context.Context) (string, error) {
				return "before-sleep", nil
			})
			if err != nil {
				return nil, err
			}

			if err := workflow.Sleep(wctx, 1*time.Hour); err != nil {
				return nil, err
			}

			result, err := workflow.Step(wctx, func(ctx context.Context) (string, error) {
				return "after-sleep", nil
			})
			if err != nil {
				return nil, err
			}
			return &WorkerTestOutput{Result: result}, nil
		},
	)

	ctx := t.Context()
	instanceID, err := wf.Start(ctx, &WorkerTestParams{Value: "test"})
	require.NoError(t, err)

	workerCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = runner.RunWorker(workerCtx, workflow.WorkerOptions{
			PollInterval:        10 * time.Millisecond,
			LeaseExtendInterval: 0,
		})
	}()

	// Wait for the worker to hit the sleep step.
	time.Sleep(500 * time.Millisecond)

	// Workflow should not be complete yet.
	_, err = wf.GetResult(ctx, instanceID)
	require.Error(t, err, "should not be complete while sleeping")

	// Advance past sleep.
	clock.Advance(2 * time.Hour)

	// The worker should pick up the task with the correct workflow name and complete it.
	require.Eventually(t, func() bool {
		result, err := wf.GetResult(ctx, instanceID)
		return err == nil && result.Result == "after-sleep"
	}, 5*time.Second, 50*time.Millisecond,
		"workflow should complete after sleep — worker must resolve the correct workflow name")

	cancel()
	<-done
}
