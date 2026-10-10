package workflow_test

import (
	"context"
	"errors"
	"testing"

	"github.com/DeluxeOwl/chronicle/event"
	"github.com/DeluxeOwl/chronicle/eventlog"
	"github.com/DeluxeOwl/chronicle/workflow"
	"github.com/stretchr/testify/require"
)

func TestWorkflow_StatusAndResultReadOutagesDoNotFailCallingSteps(t *testing.T) {
	for _, query := range []string{"status", "result"} {
		t.Run(query, func(t *testing.T) {
			storageErr := errors.New("target stream temporarily unavailable")
			log := &streamReadFailureLog{
				Log:       eventlog.NewMemory(),
				failingID: "",
				failure:   storageErr,
			}
			runner, err := workflow.NewRunner(log, nil)
			require.NoError(t, err)
			target := workflow.New(
				runner,
				"query-target",
				func(*workflow.Context, *struct{}) (*recoveryOutput, error) {
					return &recoveryOutput{Value: "available"}, nil
				},
			)
			targetID, err := target.Start(t.Context(), &struct{}{})
			require.NoError(t, err)
			_, err = target.Run(t.Context(), targetID)
			require.NoError(t, err)
			parent := workflow.New(
				runner,
				"query-parent",
				func(wctx *workflow.Context, _ *struct{}) (*recoveryOutput, error) {
					value, err := workflow.Step(wctx, func(ctx context.Context) (string, error) {
						if query == "status" {
							status, err := target.GetStatus(ctx, targetID)
							return string(status.Status), err
						}
						result, err := target.GetResult(ctx, targetID)
						if err != nil {
							return "", err
						}
						return result.Value, nil
					})
					return &recoveryOutput{Value: value}, err
				},
			)
			id, err := parent.Start(t.Context(), &struct{}{})
			require.NoError(t, err)
			log.failingID = event.LogID(targetID)
			_, err = parent.Run(t.Context(), id)
			require.ErrorIs(t, err, storageErr)
			require.ErrorIs(t, err, workflow.ErrWorkflowInfrastructure)
			log.failingID = ""
			status, err := parent.GetStatus(t.Context(), id)
			require.NoError(t, err)
			require.Equal(t, workflow.StatusRunning, status.Status)
			out, err := parent.Run(t.Context(), id)
			require.NoError(t, err)
			if query == "status" {
				require.Equal(t, string(workflow.StatusCompleted), out.Value)
			} else {
				require.Equal(t, "available", out.Value)
			}
		})
	}
}
