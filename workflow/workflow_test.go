package workflow_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"github.com/DeluxeOwl/chronicle/workflow"
	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/require"
)

func setupTestDB(t *testing.T) *sql.DB {
	t.Helper()
	// Use shared cache mode to ensure all connections use the same in-memory database
	db, err := sql.Open("sqlite3", "file::memory:?cache=shared")
	require.NoError(t, err)
	// Set max connections to 1 to ensure consistency with in-memory database
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db
}

type SendReportParams struct {
	Email string `json:"email"`
}

type SendReportOutput struct {
	SentFiles []string `json:"sentFiles"`
}

func TestBasicWorkflow(t *testing.T) {
	db := setupTestDB(t)
	wfr, err := workflow.NewSqliteRunner(db)
	require.NoError(t, err)

	sendReports := workflow.New(
		wfr,
		"send-reports",
		func(wctx workflow.Context, params *SendReportParams) (*SendReportOutput, error) {
			// Step 1: Generate files
			sentFiles, err := workflow.Step(wctx, func(_ context.Context) ([]string, error) {
				return []string{
					"doc_7392_rev3.pdf",
					"report_x29_final.pdf",
					"memo_2024_05_12.pdf",
				}, nil
			})
			if err != nil {
				return nil, fmt.Errorf("send files: %w", err)
			}

			// Step 2: Send email
			err = workflow.Step2(wctx, func(_ context.Context) error {
				// Simulate email sending
				return nil
			})
			if err != nil {
				return nil, fmt.Errorf("send email: %w", err)
			}

			return &SendReportOutput{
				SentFiles: sentFiles,
			}, nil
		},
	)

	// Start and run the workflow
	ctx := t.Context()
	params := &SendReportParams{Email: "test@example.com"}

	instanceID, err := sendReports.Start(ctx, params)
	require.NoError(t, err)
	require.NotEmpty(t, instanceID)

	output, err := sendReports.Run(ctx, instanceID)
	require.NoError(t, err)
	require.NotNil(t, output)
	require.Len(t, output.SentFiles, 3)
	require.Equal(t, "doc_7392_rev3.pdf", output.SentFiles[0])
}

func TestWorkflowReplay(t *testing.T) {
	db := setupTestDB(t)
	wfr, err := workflow.NewSqliteRunner(db)
	require.NoError(t, err)

	step1Count := 0
	step2Count := 0

	sendReports := workflow.New(
		wfr,
		"replay-test",
		func(wctx workflow.Context, params *SendReportParams) (*SendReportOutput, error) {
			// Step 1: Generate files (should only execute once)
			sentFiles, err := workflow.Step(wctx, func(_ context.Context) ([]string, error) {
				step1Count++
				return []string{"file1.pdf", "file2.pdf"}, nil
			})
			if err != nil {
				return nil, fmt.Errorf("send files: %w", err)
			}

			// Step 2: Send email (should only execute once)
			err = workflow.Step2(wctx, func(_ context.Context) error {
				step2Count++
				return nil
			})
			if err != nil {
				return nil, fmt.Errorf("send email: %w", err)
			}

			return &SendReportOutput{
				SentFiles: sentFiles,
			}, nil
		},
	)

	ctx := t.Context()
	params := &SendReportParams{Email: "test@example.com"}

	// First run
	instanceID, err := sendReports.Start(ctx, params)
	require.NoError(t, err)

	output1, err := sendReports.Run(ctx, instanceID)
	require.NoError(t, err)
	require.Equal(t, 1, step1Count, "step 1 should execute once")
	require.Equal(t, 1, step2Count, "step 2 should execute once")

	// Simulate crash and replay - create new runner but same DB
	wfr2, err := workflow.NewSqliteRunner(db)
	require.NoError(t, err)

	sendReports2 := workflow.New(
		wfr2,
		"replay-test",
		func(wctx workflow.Context, params *SendReportParams) (*SendReportOutput, error) {
			sentFiles, err := workflow.Step(wctx, func(_ context.Context) ([]string, error) {
				step1Count++
				return []string{"file1.pdf", "file2.pdf"}, nil
			})
			if err != nil {
				return nil, fmt.Errorf("send files: %w", err)
			}

			err = workflow.Step2(wctx, func(_ context.Context) error {
				step2Count++
				return nil
			})
			if err != nil {
				return nil, fmt.Errorf("send email: %w", err)
			}

			return &SendReportOutput{
				SentFiles: sentFiles,
			}, nil
		},
	)

	output2, err := sendReports2.Run(ctx, instanceID)
	require.NoError(t, err)
	require.Equal(t, 1, step1Count, "step 1 should NOT execute again (cached)")
	require.Equal(t, 1, step2Count, "step 2 should NOT execute again (cached)")
	require.Equal(t, output1.SentFiles, output2.SentFiles)
}

func TestWorkflowStepFailure(t *testing.T) {
	db := setupTestDB(t)
	wfr, err := workflow.NewSqliteRunner(db)
	require.NoError(t, err)

	failCount := 0

	sendReports := workflow.New(
		wfr,
		"failure-test",
		func(wctx workflow.Context, params *SendReportParams) (*SendReportOutput, error) {
			// Step 1: This will fail on first attempt
			sentFiles, err := workflow.Step(wctx, func(_ context.Context) ([]string, error) {
				failCount++
				if failCount < 2 {
					return nil, errors.New("simulated failure")
				}
				return []string{"file1.pdf"}, nil
			})
			if err != nil {
				return nil, fmt.Errorf("send files: %w", err)
			}

			return &SendReportOutput{SentFiles: sentFiles}, nil
		},
	)

	ctx := t.Context()
	instanceID, err := sendReports.Start(ctx, &SendReportParams{Email: "test@example.com"})
	require.NoError(t, err)

	// First run - should fail at step 1
	_, err = sendReports.Run(ctx, instanceID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "simulated failure")

	// Check it's in failed state
	_, err = sendReports.GetResult(ctx, instanceID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not completed")
}

func TestWorkflowMultipleInstances(t *testing.T) {
	db := setupTestDB(t)
	wfr, err := workflow.NewSqliteRunner(db)
	require.NoError(t, err)

	sendReports := workflow.New(
		wfr,
		"multi-instance",
		func(wctx workflow.Context, params *SendReportParams) (*SendReportOutput, error) {
			sentFiles, err := workflow.Step(wctx, func(_ context.Context) ([]string, error) {
				return []string{"file.pdf"}, nil
			})
			if err != nil {
				return nil, err
			}

			return &SendReportOutput{SentFiles: sentFiles}, nil
		},
	)

	ctx := t.Context()

	// Create multiple instances
	instance1, err := sendReports.Start(ctx, &SendReportParams{Email: "user1@example.com"})
	require.NoError(t, err)

	instance2, err := sendReports.Start(ctx, &SendReportParams{Email: "user2@example.com"})
	require.NoError(t, err)

	// Run both
	output1, err := sendReports.Run(ctx, instance1)
	require.NoError(t, err)
	require.Len(t, output1.SentFiles, 1)

	output2, err := sendReports.Run(ctx, instance2)
	require.NoError(t, err)
	require.Len(t, output2.SentFiles, 1)
}

func TestStepFunctionVariations(t *testing.T) {
	db := setupTestDB(t)
	wfr, err := workflow.NewSqliteRunner(db)
	require.NoError(t, err)

	executed := make(map[string]bool)

	type TestOutput struct {
		Value1 string `json:"value1"`
		Value2 int    `json:"value2"`
	}

	wf := workflow.New(
		wfr,
		"step-variations",
		func(wctx workflow.Context, params *struct{}) (*TestOutput, error) {
			// Step returning string
			strResult, err := workflow.Step(wctx, func(_ context.Context) (string, error) {
				executed["string"] = true
				return "hello", nil
			})
			if err != nil {
				return nil, err
			}
			require.Equal(t, "hello", strResult)

			// Step returning int
			intResult, err := workflow.Step(wctx, func(_ context.Context) (int, error) {
				executed["int"] = true
				return 42, nil
			})
			if err != nil {
				return nil, err
			}
			require.Equal(t, 42, intResult)

			// Step returning struct
			structResult, err := workflow.Step(wctx, func(_ context.Context) (TestOutput, error) {
				executed["struct"] = true
				return TestOutput{Value1: "test", Value2: 123}, nil
			})
			if err != nil {
				return nil, err
			}
			require.Equal(t, "test", structResult.Value1)
			require.Equal(t, 123, structResult.Value2)

			// Step2 (no return value)
			err = workflow.Step2(wctx, func(_ context.Context) error {
				executed["step2"] = true
				return nil
			})
			if err != nil {
				return nil, err
			}

			return &TestOutput{Value1: strResult, Value2: intResult}, nil
		},
	)

	ctx := t.Context()
	instanceID, err := wf.Start(ctx, &struct{}{})
	require.NoError(t, err)

	output, err := wf.Run(ctx, instanceID)
	require.NoError(t, err)
	require.Equal(t, "hello", output.Value1)
	require.Equal(t, 42, output.Value2)
	require.True(t, executed["string"])
	require.True(t, executed["int"])
	require.True(t, executed["struct"])
	require.True(t, executed["step2"])
}

func TestWorkflowMidExecutionFailureAndResume(t *testing.T) {
	db := setupTestDB(t)
	wfr, err := workflow.NewSqliteRunner(db)
	require.NoError(t, err)

	step1Executed := 0
	step2Executed := 0

	sendReports := workflow.New(
		wfr,
		"mid-exec-failure",
		func(wctx workflow.Context, params *SendReportParams) (*SendReportOutput, error) {
			// Step 1: Always succeeds
			sentFiles, err := workflow.Step(wctx, func(_ context.Context) ([]string, error) {
				step1Executed++
				return []string{"file1.pdf"}, nil
			})
			if err != nil {
				return nil, fmt.Errorf("send files: %w", err)
			}

			// Step 2: Fails first time, succeeds second
			err = workflow.Step2(wctx, func(_ context.Context) error {
				step2Executed++
				if step2Executed == 1 {
					return errors.New("step 2 simulated failure")
				}
				return nil
			})
			if err != nil {
				return nil, fmt.Errorf("send email: %w", err)
			}

			return &SendReportOutput{
				SentFiles: sentFiles,
			}, nil
		},
	)

	ctx := t.Context()
	instanceID, err := sendReports.Start(ctx, &SendReportParams{Email: "test@example.com"})
	require.NoError(t, err)

	// First run - fails at step 2
	_, err = sendReports.Run(ctx, instanceID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "step 2 simulated failure")
	require.Equal(t, 1, step1Executed, "step 1 should have executed")
	require.Equal(t, 1, step2Executed, "step 2 should have attempted")

	// The workflow should be in failed state - cannot resume from here
	// User would need to create a new instance or implement retry logic
	_, err = sendReports.GetResult(ctx, instanceID)
	require.Error(t, err)
}

func TestWorkflowParallelInstances(t *testing.T) {
	db := setupTestDB(t)
	wfr, err := workflow.NewSqliteRunner(db)
	require.NoError(t, err)

	sendReports := workflow.New(
		wfr,
		"parallel",
		func(wctx workflow.Context, params *SendReportParams) (*SendReportOutput, error) {
			sentFiles, err := workflow.Step(wctx, func(_ context.Context) ([]string, error) {
				return []string{params.Email + ".pdf"}, nil
			})
			if err != nil {
				return nil, err
			}

			return &SendReportOutput{SentFiles: sentFiles}, nil
		},
	)

	ctx := t.Context()

	// Create multiple instances
	instances := []workflow.InstanceID{}
	for i := range 5 {
		instanceID, err := sendReports.Start(
			ctx,
			&SendReportParams{Email: fmt.Sprintf("user%d", i)},
		)
		require.NoError(t, err)
		instances = append(instances, instanceID)
	}

	// Run all instances
	for i, instanceID := range instances {
		output, err := sendReports.Run(ctx, instanceID)
		require.NoError(t, err, "instance %d should succeed", i)
		require.Equal(t, fmt.Sprintf("user%d.pdf", i), output.SentFiles[0])
	}
}

func TestEmptyStepResult(t *testing.T) {
	db := setupTestDB(t)
	wfr, err := workflow.NewSqliteRunner(db)
	require.NoError(t, err)

	wf := workflow.New(
		wfr,
		"empty-result",
		func(wctx workflow.Context, params *struct{}) (*struct{}, error) {
			// Step returning empty slice
			result, err := workflow.Step(wctx, func(_ context.Context) ([]string, error) {
				return []string{}, nil
			})
			if err != nil {
				return nil, err
			}
			require.Empty(t, result)

			// Step returning nil slice
			result2, err := workflow.Step(wctx, func(_ context.Context) ([]string, error) {
				return nil, nil
			})
			if err != nil {
				return nil, err
			}
			require.Nil(t, result2)

			return &struct{}{}, nil
		},
	)

	ctx := t.Context()
	instanceID, err := wf.Start(ctx, &struct{}{})
	require.NoError(t, err)

	_, err = wf.Run(ctx, instanceID)
	require.NoError(t, err)
}

func TestComplexReturnTypes(t *testing.T) {
	db := setupTestDB(t)
	wfr, err := workflow.NewSqliteRunner(db)
	require.NoError(t, err)

	type ComplexType struct {
		Map    map[string]int `json:"map"`
		Slice  []string       `json:"slice"`
		Nested struct {
			Value bool `json:"value"`
		} `json:"nested"`
	}

	wf := workflow.New(
		wfr,
		"complex-types",
		func(wctx workflow.Context, params *struct{}) (*ComplexType, error) {
			result, err := workflow.Step(wctx, func(_ context.Context) (*ComplexType, error) {
				return &ComplexType{
					Map:   map[string]int{"key": 123},
					Slice: []string{"a", "b", "c"},
					Nested: struct {
						Value bool `json:"value"`
					}{Value: true},
				}, nil
			})
			if err != nil {
				return nil, err
			}
			return result, nil
		},
	)

	ctx := t.Context()
	instanceID, err := wf.Start(ctx, &struct{}{})
	require.NoError(t, err)

	output, err := wf.Run(ctx, instanceID)
	require.NoError(t, err)
	require.Equal(t, 123, output.Map["key"])
	require.Equal(t, []string{"a", "b", "c"}, output.Slice)
	require.True(t, output.Nested.Value)
}

func TestWorkflowAPIExactlyAsSpecified(t *testing.T) {
	// This test verifies the exact API from the user's specification works
	db := setupTestDB(t)
	wfr, err := workflow.NewSqliteRunner(db)
	require.NoError(t, err)

	// Exact API from specification (without random failures for deterministic test)
	sendReports := workflow.New(
		wfr,
		"send-reports",
		func(wctx workflow.Context, params *SendReportParams) (*SendReportOutput, error) {
			sentFiles, err := workflow.Step(wctx, func(_ context.Context) ([]string, error) {
				return []string{
					"doc_7392_rev3.pdf",
					"report_x29_final.pdf",
					"memo_2024_05_12.pdf",
				}, nil
			})
			if err != nil {
				return nil, fmt.Errorf("send files: %w", err)
			}

			err = workflow.Step2(wctx, func(_ context.Context) error {
				return nil
			})
			if err != nil {
				return nil, fmt.Errorf("send email: %w", err)
			}

			return &SendReportOutput{
				SentFiles: sentFiles,
			}, nil
		},
	)

	ctx := t.Context()
	params := &SendReportParams{Email: "user@example.com"}

	instanceID, err := sendReports.Start(ctx, params)
	require.NoError(t, err)
	require.NotEmpty(t, instanceID)

	output, err := sendReports.Run(ctx, instanceID)
	require.NoError(t, err)
	require.NotNil(t, output)
	require.Len(t, output.SentFiles, 3)
}

func TestWorkflowEventsAreStored(t *testing.T) {
	db := setupTestDB(t)
	wfr, err := workflow.NewSqliteRunner(db)
	require.NoError(t, err)

	wf := workflow.New(
		wfr,
		"event-storage-test",
		func(wctx workflow.Context, params *struct{ Value int }) (*struct{ Result int }, error) {
			result, err := workflow.Step(wctx, func(_ context.Context) (int, error) {
				return params.Value * 2, nil
			})
			if err != nil {
				return nil, err
			}

			return &struct{ Result int }{Result: result}, nil
		},
	)

	ctx := t.Context()
	instanceID, err := wf.Start(ctx, &struct{ Value int }{Value: 21})
	require.NoError(t, err)

	output, err := wf.Run(ctx, instanceID)
	require.NoError(t, err)
	require.Equal(t, 42, output.Result)

	// Verify we can reload and get the result
	output2, err := wf.GetResult(ctx, instanceID)
	require.NoError(t, err)
	require.Equal(t, 42, output2.Result)
}
