package workflow_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/DeluxeOwl/chronicle/event"
	"github.com/DeluxeOwl/chronicle/eventlog"
	"github.com/DeluxeOwl/chronicle/version"
	"github.com/DeluxeOwl/chronicle/workflow"
	"github.com/stretchr/testify/require"
)

func setupAsyncWakeRecovery(
	t *testing.T,
) (*workflow.Runner, *workflow.AsyncQueue, event.GlobalLog, *controllableClock) {
	t.Helper()
	clock := newClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	queue, err := workflow.NewAsyncQueue(
		t.Context(),
		setupAsyncDB(t),
		workflow.WithAsyncQueueNowFunc(clock.Now),
	)
	require.NoError(t, err)
	log := eventlog.NewMemory()
	runner, err := workflow.NewRunner(
		log,
		nil,
		workflow.WithTaskQueue(queue),
		workflow.WithNowFunc(clock.Now),
	)
	require.NoError(t, err)
	return runner, queue, log, clock
}

// Driving Handle synchronously pauses projection progress without pausing publication.
func projectAsyncWakeEvents(
	t *testing.T,
	queue *workflow.AsyncQueue,
	log event.GlobalReader,
	after version.Version,
) version.Version {
	t.Helper()
	for rec, err := range log.ReadAllEvents(t.Context(), version.SelectFromBeginning) {
		require.NoError(t, err)
		if rec.GlobalVersion() <= after {
			continue
		}
		require.NoError(t, queue.Handle(t.Context(), rec))
		after = rec.GlobalVersion()
	}
	return after
}

func TestAsyncQueue_LaggingWaitProjectionPreservesPublishedWake(t *testing.T) {
	for _, acknowledgeFirst := range []bool{true, false} {
		t.Run(
			map[bool]string{true: "after_acknowledgment", false: "while_claimed"}[acknowledgeFirst],
			func(t *testing.T) {
				runner, queue, log, clock := setupAsyncWakeRecovery(t)
				wf := workflow.New(
					runner,
					"lagging-event-wake",
					func(wctx *workflow.Context, _ *struct{}) (*WorkerTestOutput, error) {
						payload, err := workflow.WaitForEvent[string](
							wctx,
							"delivery:42",
							workflow.WaitForEventOptions{Timeout: 6 * time.Hour},
						)
						if err != nil {
							return nil, err
						}
						return &WorkerTestOutput{Result: payload}, nil
					},
				)
				ctx := t.Context()
				id, err := wf.Start(ctx, &struct{}{})
				require.NoError(t, err)
				projected := projectAsyncWakeEvents(t, queue, log, version.Zero)
				claim, err := queue.PollClaim(ctx)
				require.NoError(t, err)
				require.NotNil(t, claim)
				_, err = wf.Run(ctx, claim.InstanceID)
				require.ErrorIs(t, err, workflow.ErrWorkflowWaiting)
				if acknowledgeFirst {
					require.NoError(t, queue.CompleteClaim(ctx, *claim))
				}

				clock.Advance(time.Second)
				// NewRunner uses memory publication storage, not the queue's SQL publication table.
				require.NoError(t, workflow.PublishEvent(ctx, runner, "delivery:42", "delivered"))
				projected = projectAsyncWakeEvents(t, queue, log, projected)
				if !acknowledgeFirst {
					blocked, pollErr := queue.PollClaim(ctx)
					require.NoError(t, pollErr)
					require.Nil(t, blocked, "projection must retain active receipt ownership")
					require.NoError(t, queue.CompleteClaim(ctx, *claim))
				}

				wake, err := queue.PollClaim(ctx)
				require.NoError(t, err)
				require.NotNil(
					t,
					wake,
					"a lagging wait projection must not postpone an already published wake",
				)
				require.Equal(t, id, wake.InstanceID)
				require.NotEqual(t, claim.Receipt, wake.Receipt)
				out, err := wf.Run(ctx, wake.InstanceID)
				require.NoError(t, err)
				require.Equal(t, "delivered", out.Result)
				require.NoError(t, queue.CompleteClaim(ctx, *wake))

				projectAsyncWakeEvents(t, queue, log, projected)
				finished, err := queue.PollClaim(ctx)
				require.NoError(t, err)
				require.Nil(t, finished, "terminal projection must remove any redundant wake")
			},
		)
	}
}

func TestAsyncQueue_ConservativeWakesHonorDurableDelays(t *testing.T) {
	for _, retry := range []bool{false, true} {
		t.Run(map[bool]string{false: "sleep", true: "retry"}[retry], func(t *testing.T) {
			runner, queue, log, clock := setupAsyncWakeRecovery(t)
			entries, callbacks := 0, 0
			expectedErr := workflow.ErrWorkflowSleeping
			if retry {
				expectedErr = workflow.ErrWorkflowRetrying
			}
			wf := workflow.New(
				runner,
				"conservative-delay",
				func(wctx *workflow.Context, _ *struct{}) (*WorkerTestOutput, error) {
					entries++
					if !retry {
						if err := workflow.Sleep(wctx, time.Hour); err != nil {
							return nil, err
						}
					}
					if err := workflow.Step2(wctx, func(context.Context) error {
						callbacks++
						if retry && callbacks == 1 {
							return errors.New("transient failure")
						}
						return nil
					}); err != nil {
						return nil, err
					}
					return &WorkerTestOutput{Result: "done"}, nil
				},
			)
			ctx := t.Context()
			id, err := wf.Start(ctx, &struct{}{}, workflow.WithRetryStrategy(workflow.RetryStrategy{
				MaxAttempts: 3,
				BaseDelay:   time.Hour,
				Factor:      1,
				MaxDelay:    time.Hour,
			}))
			require.NoError(t, err)
			projected := projectAsyncWakeEvents(t, queue, log, version.Zero)
			claim, err := queue.PollClaim(ctx)
			require.NoError(t, err)
			require.NotNil(t, claim)
			_, err = wf.Run(ctx, id)
			require.ErrorIs(t, err, expectedErr)
			require.NoError(t, queue.CompleteClaim(ctx, *claim))
			before, err := wf.GetStatus(ctx, id)
			require.NoError(t, err)
			require.Equal(t, 1, entries)
			previousCallbacks := callbacks

			// A newer wake can conservatively request a poll before the durable delay.
			require.NoError(
				t,
				queue.Enqueue(
					ctx,
					workflow.QueuedTask{
						InstanceID:   id,
						WorkflowName: "conservative-delay",
						RunAfter:     time.Time{},
					},
				),
			)
			projected = projectAsyncWakeEvents(t, queue, log, projected)
			wake, err := queue.PollClaim(ctx)
			require.NoError(t, err)
			require.NotNil(t, wake)
			_, err = wf.Run(ctx, wake.InstanceID)
			require.ErrorIs(t, err, expectedErr)
			require.Equal(t, 1, entries, "early notification must not reenter workflow code")
			require.Equal(
				t,
				previousCallbacks,
				callbacks,
				"early notification must not execute a step",
			)
			after, err := wf.GetStatus(ctx, id)
			require.NoError(t, err)
			require.Equal(t, before, after, "early notification is not a new domain retry")
			require.NoError(t, queue.ReleaseClaim(ctx, *wake, clock.Now().Add(time.Hour)))
			early, err := queue.PollClaim(ctx)
			require.NoError(t, err)
			require.Nil(t, early, "released work must remain parked until its durable deadline")

			clock.Advance(time.Hour)
			due, err := queue.PollClaim(ctx)
			require.NoError(t, err)
			require.NotNil(t, due, "early notification must not lose the delayed successor")
			out, err := wf.Run(ctx, due.InstanceID)
			require.NoError(t, err)
			require.Equal(t, "done", out.Result)
			require.Equal(t, 2, entries)
			require.Equal(t, previousCallbacks+1, callbacks)
			require.NoError(t, queue.CompleteClaim(ctx, *due))
			projectAsyncWakeEvents(t, queue, log, projected)
			finished, err := queue.PollClaim(ctx)
			require.NoError(t, err)
			require.Nil(t, finished)
		})
	}
}
