package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

// ErrWorkflowWaiting is returned by Run when the workflow is waiting for an event.
// The caller should not treat this as a failure — the workflow will be automatically
// re-triggered when the event is published (or when the timeout expires).
var ErrWorkflowWaiting = errors.New("workflow is waiting for event")

// isWaitingError checks if an error is ErrWorkflowWaiting.
func isWaitingError(err error) bool {
	return errors.Is(err, ErrWorkflowWaiting)
}

// ErrEventTimeout is returned when an awaited event does not arrive
// before the specified timeout.
var ErrEventTimeout = errors.New("event wait timed out")

// waitForEventResult is persisted as a step result for an WaitForEvent step.
// It stores the event name being waited for and, once the event arrives,
// the payload.
type waitForEventResult struct {
	EventName string          `json:"eventName"`
	Payload   json.RawMessage `json:"payload,omitempty" exhaustruct:"optional"`
	Resolved  bool            `json:"resolved" exhaustruct:"optional"`
	TimedOut  bool            `json:"timedOut,omitempty" exhaustruct:"optional"`
	Timeout   time.Duration   `json:"timeout,omitempty" exhaustruct:"optional"`
	Deadline  time.Time       `json:"deadline,omitempty" exhaustruct:"optional"`
}

// workflowWaiting records that a workflow is waiting for an external event.
type workflowWaiting struct {
	StepIndex     int           `json:"stepIndex"`
	AwaitingEvent string        `json:"awaitingEvent"`
	Timeout       time.Duration `json:"timeout,omitempty"`
	Deadline      time.Time     `json:"deadline,omitempty"`
}

func (*workflowWaiting) EventName() string { return "workflow/waiting" }
func (*workflowWaiting) isWorkflowEvent()  {}

// workflowEventReceived records that an awaited event has been received.
type workflowEventReceived struct {
	StepIndex     int             `json:"stepIndex"`
	ReceivedEvent string          `json:"receivedEvent"`
	Payload       json.RawMessage `json:"payload"`
}

func (*workflowEventReceived) EventName() string { return "workflow/event_received" }
func (*workflowEventReceived) isWorkflowEvent()  {}

// WaitForEventOptions configures an WaitForEvent call.
type WaitForEventOptions struct {
	// Timeout is the maximum time to wait for the event.
	// If zero, the workflow waits indefinitely.
	Timeout time.Duration
}

// WaitForEvent suspends the workflow until a named event is published via PublishEvent.
//
// Like Sleep, this is a durable checkpoint. The wait state is recorded in the event log.
// If the process restarts, calling Run again on this instance will check whether the
// event has been published and either continue waiting or resume execution.
//
// When a workflow hits an WaitForEvent that hasn't been resolved yet, Run returns
// ErrWorkflowWaiting.
//
// Example:
//
//	type ShipmentEvent struct {
//	    TrackingNumber string `json:"tracking_number"`
//	}
//	shipment, err := workflow.WaitForEvent[ShipmentEvent](wctx, "order.shipped:order-42")
//
//nolint:gocognit,funlen
func WaitForEvent[T any](wctx *Context, eventName string, opts ...WaitForEventOptions) (T, error) {
	var zero T
	runner := wctx.runner

	// Claim the step index
	stepIndex := wctx.stepCount
	wctx.stepCount++

	// Reload instance to get latest state
	instance, err := runner.repo.Get(wctx.ctx, wctx.instanceID)
	if err != nil {
		return zero, fmt.Errorf("reload instance for await event: %w", err)
	}

	now := runner.now()

	var timeout time.Duration
	if len(opts) > 0 {
		timeout = opts[0].Timeout
	}

	// Check if this step was already recorded (replay)
	//nolint:nestif // This is readable
	if cachedResult, ok := instance.stepResults[stepIndex]; ok {
		var ar waitForEventResult
		if err := json.Unmarshal(cachedResult, &ar); err != nil {
			return zero, fmt.Errorf("unmarshal cached await event result: %w", err)
		}

		if ar.Resolved {
			// Event already arrived — return the payload
			var result T
			if err := json.Unmarshal(ar.Payload, &result); err != nil {
				return zero, fmt.Errorf("unmarshal event payload: %w", err)
			}
			return result, nil
		}

		if ar.TimedOut {
			return zero, ErrEventTimeout
		}

		// Check timeout deadline
		if !ar.Deadline.IsZero() && !now.Before(ar.Deadline) {
			// Timeout has expired — record as timed out
			timedOutResult := waitForEventResult{
				EventName: ar.EventName,
				TimedOut:  true,
				Timeout:   ar.Timeout,
				Deadline:  ar.Deadline,
			}
			resultJSON, err := json.Marshal(timedOutResult)
			if err != nil {
				return zero, fmt.Errorf("marshal timed out result: %w", err)
			}

			// Reload to record the timeout
			instance, err = runner.repo.Get(wctx.ctx, wctx.instanceID)
			if err != nil {
				return zero, fmt.Errorf("reload for timeout: %w", err)
			}
			if err := instance.recordThat(&stepCompleted{
				StepIndex:   stepIndex,
				Result:      resultJSON,
				CompletedAt: now,
			}); err != nil {
				return zero, fmt.Errorf("record event timeout: %w", err)
			}
			if _, _, err := runner.repo.Save(wctx.ctx, instance); err != nil {
				return zero, fmt.Errorf("save event timeout: %w", err)
			}
			return zero, ErrEventTimeout
		}

		// Still waiting — check if event has arrived in the waiting store
		payload, found := runner.waitStore.GetEvent(wctx.instanceID, eventName)
		if found {
			// Event has arrived! Update the step result
			resolvedResult := waitForEventResult{
				EventName: eventName,
				Payload:   payload,
				Resolved:  true,
			}
			resultJSON, err := json.Marshal(resolvedResult)
			if err != nil {
				return zero, fmt.Errorf("marshal resolved result: %w", err)
			}

			// Reload and record
			instance, err = runner.repo.Get(wctx.ctx, wctx.instanceID)
			if err != nil {
				return zero, fmt.Errorf("reload for event received: %w", err)
			}
			if err := instance.recordThat(&workflowEventReceived{
				StepIndex:     stepIndex,
				ReceivedEvent: eventName,
				Payload:       payload,
			}); err != nil {
				return zero, fmt.Errorf("record event received: %w", err)
			}
			// Overwrite step result with resolved version
			if err := instance.recordThat(&stepCompleted{
				StepIndex:   stepIndex,
				Result:      resultJSON,
				CompletedAt: runner.now(),
			}); err != nil {
				return zero, fmt.Errorf("record resolved step: %w", err)
			}
			if _, _, err := runner.repo.Save(wctx.ctx, instance); err != nil {
				return zero, fmt.Errorf("save resolved event: %w", err)
			}

			var result T
			if err := json.Unmarshal(payload, &result); err != nil {
				return zero, fmt.Errorf("unmarshal event payload: %w", err)
			}
			return result, nil
		}

		// Still waiting
		runner.logger.DebugContext(wctx.ctx,
			"await event replay, still waiting",
			"instanceID", wctx.instanceID,
			"stepIndex", stepIndex,
			"eventName", eventName,
		)
		return zero, ErrWorkflowWaiting
	}

	// First execution: record the wait
	var deadline time.Time
	if timeout > 0 {
		deadline = now.Add(timeout)
	}

	ar := waitForEventResult{
		EventName: eventName,
		Resolved:  false,
		Timeout:   timeout,
		Deadline:  deadline,
	}
	resultJSON, err := json.Marshal(ar)
	if err != nil {
		return zero, fmt.Errorf("marshal await event result: %w", err)
	}

	if err := instance.recordThat(&workflowWaiting{
		StepIndex:     stepIndex,
		AwaitingEvent: eventName,
		Timeout:       timeout,
		Deadline:      deadline,
	}); err != nil {
		return zero, fmt.Errorf("record workflow waiting: %w", err)
	}

	if err := instance.recordThat(&stepCompleted{
		StepIndex:   stepIndex,
		Result:      resultJSON,
		CompletedAt: now,
	}); err != nil {
		return zero, fmt.Errorf("record await event step: %w", err)
	}

	if _, _, err := runner.repo.Save(wctx.ctx, instance); err != nil {
		return zero, fmt.Errorf("save await event step: %w", err)
	}

	// Register the wait in the runner's waiting store
	runner.waitStore.Register(wctx.instanceID, eventName, instance.workflowName)

	// If there's a timeout, enqueue a delayed wake-up task
	if timeout > 0 {
		if err := runner.queue.Enqueue(wctx.ctx, QueuedTask{
			InstanceID:   wctx.instanceID,
			WorkflowName: instance.workflowName,
			RunAfter:     deadline,
		}); err != nil {
			return zero, fmt.Errorf("enqueue event timeout task: %w", err)
		}
	}

	runner.logger.InfoContext(wctx.ctx,
		"workflow waiting for event",
		"instanceID", wctx.instanceID,
		"stepIndex", stepIndex,
		"eventName", eventName,
		"timeout", timeout,
	)

	return zero, ErrWorkflowWaiting
}

// PublishEvent publishes a named event that can wake up workflows waiting via WaitForEvent.
// The payload is stored and delivered to any workflow waiting for this event name.
//
// Events are idempotent: the first publish for a given name wins, subsequent publishes are ignored.
//
// Example:
//
//	err := workflow.PublishEvent(ctx, runner, "order.shipped:order-42",
//	    map[string]any{"tracking_number": "XYZ"})
func PublishEvent(ctx context.Context, runner *Runner, eventName string, payload any) error {
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal event payload: %w", err)
	}

	// Store the event and get any waiting workflows
	waiters := runner.waitStore.Publish(eventName, payloadJSON)

	// Enqueue wake-up tasks for all waiting workflows
	for _, w := range waiters {
		if err := runner.queue.Enqueue(ctx, QueuedTask{
			InstanceID:   w.InstanceID,
			WorkflowName: w.WorkflowName,
		}); err != nil {
			runner.logger.ErrorContext(ctx,
				"failed to enqueue event wake task",
				"instanceID", w.InstanceID,
				"eventName", eventName,
				"error", err,
			)
			// Continue trying other waiters
		}
	}

	runner.logger.InfoContext(ctx,
		"event published",
		"eventName", eventName,
		"waitersWoken", len(waiters),
	)

	return nil
}

// waitingWorkflow tracks a workflow that is waiting for an event.
type waitingWorkflow struct {
	InstanceID   InstanceID
	WorkflowName string
}

// eventWaitStore is the abstraction for managing event waiting state.
// The in-memory implementation (waitStore) is used for single-process mode;
// the persistent implementation (persistentWaitStore) is used with SyncQueue
// for distributed systems where PublishEvent from one process must be able to
// wake workflows running on another.
type eventWaitStore interface {
	// Register records that a workflow is waiting for an event.
	// For the in-memory store, this also checks for pre-published events.
	// For the persistent store, this is a no-op (handled by SyncQueue.Process).
	Register(instanceID InstanceID, eventName string, workflowName string)

	// Publish stores an event payload and returns the list of workflows that were waiting.
	// First-write-wins: if the event has already been published, returns nil.
	Publish(eventName string, payload json.RawMessage) []waitingWorkflow

	// GetEvent checks if an event has been received for a specific workflow instance.
	GetEvent(instanceID InstanceID, eventName string) (json.RawMessage, bool)
}

// waitStore is the in-memory implementation of eventWaitStore.
// For distributed systems, use persistentWaitStore instead.
type waitStore struct {
	mu sync.Mutex

	// eventName → list of waiting workflows
	waiters map[string][]waitingWorkflow

	// eventName → payload (first-write-wins)
	published map[string]json.RawMessage

	// instanceID:eventName → payload (for replay lookups)
	received map[string]json.RawMessage
}

func newWaitStore() *waitStore {
	return &waitStore{
		waiters:   make(map[string][]waitingWorkflow),
		published: make(map[string]json.RawMessage),
		received:  make(map[string]json.RawMessage),
		mu:        sync.Mutex{},
	}
}

// Register records that a workflow is waiting for an event.
// If the event has already been published, the payload is stored for the
// workflow's instance but no wake-up is triggered here — the caller
// should check GetEvent on the next Run.
func (ws *waitStore) Register(instanceID InstanceID, eventName string, workflowName string) {
	ws.mu.Lock()
	defer ws.mu.Unlock()

	// If event already published, store for this instance
	if payload, ok := ws.published[eventName]; ok {
		key := string(instanceID) + ":" + eventName
		ws.received[key] = payload
		return
	}

	ws.waiters[eventName] = append(ws.waiters[eventName], waitingWorkflow{
		InstanceID:   instanceID,
		WorkflowName: workflowName,
	})
}

// Publish stores an event payload and returns the list of workflows that were waiting.
// First-write-wins: if the event has already been published, returns nil.
func (ws *waitStore) Publish(eventName string, payload json.RawMessage) []waitingWorkflow {
	ws.mu.Lock()
	defer ws.mu.Unlock()

	// First-write-wins
	if _, ok := ws.published[eventName]; ok {
		return nil
	}

	ws.published[eventName] = payload

	// Move all waiters to received
	waiters := ws.waiters[eventName]
	for _, w := range waiters {
		key := string(w.InstanceID) + ":" + eventName
		ws.received[key] = payload
	}
	delete(ws.waiters, eventName)

	return waiters
}

// GetEvent checks if an event has been received for a specific workflow instance.
func (ws *waitStore) GetEvent(instanceID InstanceID, eventName string) (json.RawMessage, bool) {
	ws.mu.Lock()
	defer ws.mu.Unlock()

	key := string(instanceID) + ":" + eventName
	payload, ok := ws.received[key]
	return payload, ok
}

// Compile-time interface check.
var _ eventWaitStore = (*waitStore)(nil)
