package monitor_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DeluxeOwl/chronicle/event"
	"github.com/DeluxeOwl/chronicle/eventlog"
	"github.com/DeluxeOwl/chronicle/workflow"
	"github.com/DeluxeOwl/chronicle/workflow/monitor"
	"github.com/stretchr/testify/require"
)

func TestMonitor_SleepHistoryDistinguishesResumedAndUnelapsedFailure(t *testing.T) {
	failure := errors.New("domain rejection after delay")
	cases := []struct {
		name         string
		options      []workflow.StartOption
		outcome      error
		status       workflow.Status
		beforeWake   bool
		sleepStatus  string
		completed    int
		decisionName string
	}{
		{
			name: "failure_after_wake", options: nil, outcome: failure, status: workflow.StatusFailed,
			beforeWake: false, sleepStatus: "completed", completed: 1, decisionName: "workflow/failed",
		},
		{
			name: "retry_after_wake", options: sleepHistoryRetryOptions(), outcome: workflow.ErrWorkflowRetrying,
			status: workflow.StatusRetrying, beforeWake: false, sleepStatus: "completed", completed: 1,
			decisionName: "workflow/retried",
		},
		{
			name: "failure_before_wake", options: nil, outcome: failure, status: workflow.StatusFailed,
			beforeWake: true, sleepStatus: "sleeping", completed: 0, decisionName: "workflow/failed",
		},
		{
			name: "retry_before_wake", options: sleepHistoryRetryOptions(), outcome: workflow.ErrWorkflowRetrying,
			status: workflow.StatusRetrying, beforeWake: true, sleepStatus: "sleeping", completed: 0,
			decisionName: "workflow/retried",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			detail := runSleepToDecision(t, tc.options, failure, tc.outcome, tc.beforeWake)
			require.Equal(t, tc.status, detail.Summary.Status)
			require.Equal(t, tc.sleepStatus, detail.Steps[0].Status)
			require.Equal(t, tc.completed, detail.Summary.CompletedSteps)
			require.Nil(
				t,
				detail.Steps[0].CompletedAt,
				"a workflow decision does not record the exact sleep resumption time",
			)
			require.Equal(t, "Delay", detail.Steps[0].Name)
			require.Equal(t, "sleep", detail.Steps[0].Kind)
			require.Equal(t, tc.decisionName, detail.History[len(detail.History)-1].EventName)
		})
	}
}

func TestMonitor_CancellationDoesNotInventSleepResumption(t *testing.T) {
	for name, elapsed := range map[string]time.Duration{"before_wake": 30 * time.Second, "after_wake": 2 * time.Minute} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			log := eventlog.NewMemory()
			clock := &sleepHistoryClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
			runner := newRunner(t, log, workflow.WithNowFunc(clock.Now))
			wf := sleepThenReject(runner, errors.New("domain rejection"), false)
			id, err := wf.Start(ctx, &struct{}{})
			require.NoError(t, err)
			_, err = wf.Run(ctx, id)
			require.ErrorIs(t, err, workflow.ErrWorkflowSleeping)
			clock.now = clock.now.Add(elapsed)
			require.NoError(t, workflow.CancelWorkflow(ctx, runner, id, "operator cancellation"))
			detail := readSleepHistoryDetail(t, ctx, log, id, clock.Now())
			require.Equal(t, workflow.StatusCancelled, detail.Summary.Status)
			require.Equal(t, "cancelled", detail.Steps[0].Status)
			require.Nil(t, detail.Steps[0].CompletedAt)
			require.Zero(t, detail.Summary.CompletedSteps)
		})
	}
}

func runSleepToDecision(
	t *testing.T,
	opts []workflow.StartOption,
	failure, outcome error,
	beforeWake bool,
) workflow.InstanceDetail {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	log := eventlog.NewMemory()
	startedAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := &sleepHistoryClock{now: startedAt}
	runner := newRunner(t, log, workflow.WithNowFunc(clock.Now))
	wf := sleepThenReject(runner, failure, beforeWake)
	id, err := wf.Start(ctx, &struct{}{}, opts...)
	require.NoError(t, err)
	_, err = wf.Run(ctx, id)
	if !beforeWake {
		require.ErrorIs(t, err, workflow.ErrWorkflowSleeping)
		parked := readSleepHistoryDetail(t, ctx, log, id, clock.Now())
		require.Equal(t, workflow.StatusSleeping, parked.Summary.Status)
		require.Equal(t, "sleeping", parked.Steps[0].Status)
		require.Nil(t, parked.Steps[0].CompletedAt)
		clock.now = clock.now.Add(70 * time.Second)
		_, err = wf.Run(ctx, id)
	}
	require.ErrorIs(t, err, outcome)
	detail := readSleepHistoryDetail(t, ctx, log, id, clock.Now())
	require.Equal(t, &startedAt, detail.Steps[0].StartedAt)
	require.Equal(t, time.Date(2026, 1, 1, 0, 1, 0, 0, time.UTC), *detail.Steps[0].WakeAt)
	return detail
}

func sleepThenReject(
	runner *workflow.Runner,
	failure error,
	beforeWake bool,
) *workflow.Workflow[struct{}, struct{}] {
	return workflow.New(
		runner,
		"sleep-then-reject",
		func(wctx *workflow.Context, _ *struct{}) (*struct{}, error) {
			if err := workflow.Sleep(wctx, time.Minute, workflow.WithStepName("Delay")); err != nil &&
				!beforeWake {
				return nil, err
			}
			return nil, failure
		},
	)
}

func sleepHistoryRetryOptions() []workflow.StartOption {
	return []workflow.StartOption{workflow.WithRetryStrategy(workflow.RetryStrategy{
		MaxAttempts: 2, BaseDelay: 2 * time.Minute, Factor: 1, MaxDelay: 2 * time.Minute,
	})}
}

type sleepHistoryClock struct{ now time.Time }

func (c *sleepHistoryClock) Now() time.Time { return c.now }

func readSleepHistoryDetail(
	t *testing.T,
	ctx context.Context,
	log event.GlobalLog,
	id workflow.InstanceID,
	now time.Time,
) workflow.InstanceDetail {
	t.Helper()
	inspector, err := workflow.NewInspector(
		log,
		workflow.WithInspectorNowFunc(func() time.Time { return now }),
	)
	require.NoError(t, err)
	handler, err := monitor.NewHandler(inspector)
	require.NoError(t, err)
	response := httptest.NewRecorder()
	handler.ServeHTTP(
		response,
		httptest.NewRequest(http.MethodGet, "/api/workflows/"+string(id), nil).WithContext(ctx),
	)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var detail workflow.InstanceDetail
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &detail))
	require.Len(t, detail.Steps, 1)
	return detail
}
