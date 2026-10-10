package workflow_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/DeluxeOwl/chronicle/eventlog"
	"github.com/DeluxeOwl/chronicle/workflow"
	"github.com/stretchr/testify/require"
)

type dynamicDomainError struct{ Details any }

func (dynamicDomainError) Error() string { return "dynamic domain failure" }

type sliceDomainError []string

func (sliceDomainError) Error() string { return "slice domain failure" }

func TestWorkflow_RetryProvenanceSupportsAllGoErrorValues(t *testing.T) {
	for _, failure := range []error{dynamicDomainError{Details: []string{"detail"}}, sliceDomainError{"detail"}} {
		t.Run(fmt.Sprintf("%T", failure), func(t *testing.T) {
			clock := newClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
			log := eventlog.NewMemory()
			calls := 0
			definition := func(wctx *workflow.Context, _ *struct{}) (*recoveryOutput, error) {
				value, primaryErr := workflow.Step(wctx, func(context.Context) (string, error) {
					calls++
					if calls == 1 {
						return "", failure
					}
					return "recovered", nil
				})
				if err := workflow.Step2(wctx, func(context.Context) error { return nil }); err != nil {
					return nil, err
				}
				if primaryErr != nil {
					return nil, fmt.Errorf("after cleanup: %w", primaryErr)
				}
				return &recoveryOutput{Value: value}, nil
			}
			runner, err := workflow.NewRunner(log, nil, workflow.WithNowFunc(clock.Now))
			require.NoError(t, err)
			wf := workflow.New(runner, "all-error-values", definition)
			id, err := wf.Start(
				t.Context(),
				&struct{}{},
				workflow.WithRetryStrategy(
					workflow.RetryStrategy{
						MaxAttempts: 2,
						BaseDelay:   time.Minute,
						Factor:      1,
						MaxDelay:    time.Minute,
					},
				),
			)
			require.NoError(t, err)
			_, err = wf.Run(t.Context(), id)
			require.ErrorIs(t, err, workflow.ErrWorkflowRetrying)
			clock.Advance(time.Minute)
			out, err := wf.Run(t.Context(), id)
			require.NoError(t, err)
			require.Equal(t, "recovered", out.Value)
			require.Equal(t, 2, calls)
		})
	}
}

func TestWorkflow_SharedSentinelCannotConfuseStepRetryIdentity(t *testing.T) {
	clock := newClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	log := eventlog.NewMemory()
	shared := errors.New("shared domain sentinel")
	primaryCalls, cleanupCalls := 0, 0
	definition := func(wctx *workflow.Context, _ *struct{}) (*recoveryOutput, error) {
		value, primaryErr := workflow.Step(wctx, func(context.Context) (string, error) {
			primaryCalls++
			if primaryCalls == 1 {
				return "", shared
			}
			return "recovered", nil
		})
		_, cleanupErr := workflow.Step(wctx, func(context.Context) (string, error) {
			cleanupCalls++
			return "", shared
		})
		if errors.Is(cleanupErr, workflow.ErrWorkflowInfrastructure) ||
			errors.Is(cleanupErr, workflow.ErrConflictAbort) {
			return nil, cleanupErr
		}
		if primaryErr != nil {
			require.ErrorIs(
				t,
				primaryErr,
				shared,
				"live provenance wrapping must preserve errors.Is",
			)
			return nil, fmt.Errorf("primary: %w", primaryErr)
		}
		return &recoveryOutput{Value: value}, nil
	}
	runner, err := workflow.NewRunner(log, nil, workflow.WithNowFunc(clock.Now))
	require.NoError(t, err)
	wf := workflow.New(runner, "shared-sentinel", definition)
	id, err := wf.Start(
		t.Context(),
		&struct{}{},
		workflow.WithRetryStrategy(
			workflow.RetryStrategy{
				MaxAttempts: 2,
				BaseDelay:   time.Minute,
				Factor:      1,
				MaxDelay:    time.Minute,
			},
		),
	)
	require.NoError(t, err)
	_, err = wf.Run(t.Context(), id)
	require.ErrorIs(t, err, workflow.ErrWorkflowRetrying)
	clock.Advance(time.Minute)
	out, err := wf.Run(t.Context(), id)
	require.NoError(t, err)
	require.Equal(t, "recovered", out.Value)
	require.Equal(t, 2, primaryCalls)
	require.Equal(
		t,
		1,
		cleanupCalls,
		"a shared cause must not invalidate the handled cleanup failure",
	)
}

func TestWorkflow_UnrelatedErrorDoesNotEraseHandledFailureWithoutLaterCheckpoint(t *testing.T) {
	clock := newClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	log := eventlog.NewMemory()
	calls, runs := 0, 0
	definition := func(wctx *workflow.Context, _ *struct{}) (*recoveryOutput, error) {
		value, err := workflow.Step(wctx, func(context.Context) (string, error) {
			calls++
			if calls == 1 {
				return "", errors.New("use fallback")
			}
			return "primary", nil
		})
		if errors.Is(err, workflow.ErrWorkflowInfrastructure) ||
			errors.Is(err, workflow.ErrConflictAbort) {
			return nil, err
		}
		if err != nil {
			value = "fallback"
		}
		runs++
		if runs == 1 {
			return nil, errors.New("unrelated later decision")
		}
		return &recoveryOutput{Value: value}, nil
	}
	runner, err := workflow.NewRunner(log, nil, workflow.WithNowFunc(clock.Now))
	require.NoError(t, err)
	wf := workflow.New(runner, "unrelated-decision", definition)
	id, err := wf.Start(
		t.Context(),
		&struct{}{},
		workflow.WithRetryStrategy(
			workflow.RetryStrategy{
				MaxAttempts: 2,
				BaseDelay:   time.Minute,
				Factor:      1,
				MaxDelay:    time.Minute,
			},
		),
	)
	require.NoError(t, err)
	_, err = wf.Run(t.Context(), id)
	require.ErrorIs(t, err, workflow.ErrWorkflowRetrying)
	clock.Advance(time.Minute)
	out, err := wf.Run(t.Context(), id)
	require.NoError(t, err)
	require.Equal(t, "fallback", out.Value)
	require.Equal(t, 1, calls)
}
