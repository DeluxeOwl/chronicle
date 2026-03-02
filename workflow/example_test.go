package workflow_test

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/DeluxeOwl/chronicle/workflow"
	_ "github.com/mattn/go-sqlite3"
)

func ExampleWorkflow() {
	// Open a SQLite database
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		panic(err)
	}
	defer db.Close()

	// Create a workflow runner
	wfr, err := workflow.NewSqliteRunner(db)
	if err != nil {
		panic(err)
	}

	// Define input/output types
	type SendEmailParams struct {
		To      string `json:"to"`
		Subject string `json:"subject"`
	}

	type SendEmailOutput struct {
		MessageID string `json:"messageID"`
	}

	// Create a workflow
	sendEmail := workflow.New(
		wfr,
		"send-email",
		func(wctx workflow.Context, params *SendEmailParams) (*SendEmailOutput, error) {
			// Step 1: Generate message ID
			messageID, err := workflow.Step(wctx, func(_ context.Context) (string, error) {
				// In real usage, this might call an external service
				return "msg_12345", nil
			})
			if err != nil {
				return nil, fmt.Errorf("generate message id: %w", err)
			}

			// Step 2: Send the email
			err = workflow.Step2(wctx, func(_ context.Context) error {
				// In real usage, this might call an email provider
				fmt.Printf("Sending email to %s: %s\n", params.To, params.Subject)
				return nil
			})
			if err != nil {
				return nil, fmt.Errorf("send email: %w", err)
			}

			return &SendEmailOutput{MessageID: messageID}, nil
		},
	)

	ctx := context.Background()

	// Start a new workflow instance
	instanceID, err := sendEmail.Start(ctx, &SendEmailParams{
		To:      "user@example.com",
		Subject: "Hello from workflow",
	})
	if err != nil {
		panic(err)
	}

	// Run the workflow to completion
	output, err := sendEmail.Run(ctx, instanceID)
	if err != nil {
		panic(err)
	}

	fmt.Printf("Workflow completed! Message ID: %s\n", output.MessageID)

	// Output:
	// Sending email to user@example.com: Hello from workflow
	// Workflow completed! Message ID: msg_12345
}
