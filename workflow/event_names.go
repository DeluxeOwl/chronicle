package workflow

// Event names emitted by the workflow runner. Kept as named constants so that
// the same string isn't duplicated across the EventName implementations,
// projection filters and switch statements.
const (
	eventNameWorkflowStarted       = "workflow/started"
	eventNameStepCompleted         = "workflow/step_completed"
	eventNameWorkflowCompleted     = "workflow/completed"
	eventNameWorkflowFailed        = "workflow/failed"
	eventNameWorkflowRetried       = "workflow/retried"
	eventNameWorkflowWaiting       = "workflow/waiting"
	eventNameWorkflowEventReceived = "workflow/event_received"
	eventNameWorkflowCancelled     = "workflow/cancelled"
)
