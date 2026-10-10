package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
	"unicode/utf8"
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
	Deadline  time.Time       `json:"deadline" exhaustruct:"optional"`
}

// workflowWaiting records that a workflow is waiting for an external event.
type workflowWaiting struct {
	StepIndex     int           `json:"stepIndex"`
	AwaitingEvent string        `json:"awaitingEvent"`
	WorkflowName  string        `json:"workflowName,omitempty" exhaustruct:"optional"`
	Timeout       time.Duration `json:"timeout,omitempty"`
	Deadline      time.Time     `json:"deadline"`
	StartedAt     time.Time     `json:"startedAt,omitempty" exhaustruct:"optional"`
	Name          string        `json:"name,omitempty" exhaustruct:"optional"`
}

func (*workflowWaiting) EventName() string { return eventNameWorkflowWaiting }
func (*workflowWaiting) isWorkflowEvent()  {}

// workflowEventReceived records that an awaited event has been received.
type workflowEventReceived struct {
	StepIndex     int             `json:"stepIndex"`
	ReceivedEvent string          `json:"receivedEvent"`
	Payload       json.RawMessage `json:"payload"`
	WorkflowName  string          `json:"workflowName,omitempty"` // populated since v2; empty in old events
	PublishedAt   time.Time       `json:"publishedAt,omitempty" exhaustruct:"optional"`
	ReceivedAt    time.Time       `json:"receivedAt,omitempty" exhaustruct:"optional"`
}

func (*workflowEventReceived) EventName() string { return eventNameWorkflowEventReceived }
func (*workflowEventReceived) isWorkflowEvent()  {}

// WaitForEventOptions configures an WaitForEvent call.
type WaitForEventOptions struct {
	// Timeout is the maximum time to wait for the event.
	// If zero, the workflow waits indefinitely.
	Timeout time.Duration `exhaustruct:"optional"`
	// Name labels this wait in the monitor.
	Name string `exhaustruct:"optional"`
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
func WaitForEvent[T any](wctx *Context, eventName string, opts ...WaitForEventOptions) (T, error) {
	var zero T
	if eventName == "" || !utf8.ValidString(eventName) {
		return zero, infrastructureError(errors.New("event name must be nonempty valid UTF-8"))
	}
	if len(opts) > 1 {
		return zero, infrastructureError(errors.New("only one wait configuration is allowed"))
	}
	var options WaitForEventOptions
	if len(opts) == 1 {
		options = opts[0]
	}
	if options.Timeout < 0 {
		return zero, infrastructureError(errors.New("event timeout must be nonnegative"))
	}
	config, err := configureStep([]StepOption{WithStepName(options.Name)})
	if err != nil {
		return zero, infrastructureError(err)
	}
	stepIndex := wctx.stepCount
	wctx.stepCount++
	if err := validateStep(wctx, stepIndex, stepKindWait, config); err != nil {
		return zero, err
	}
	instance := wctx.instance
	var wait waitForEventResult
	//nolint:nestif // A cached wait and a new wait require different durable state.
	if cached, ok := instance.stepResults[stepIndex]; ok {
		if err := json.Unmarshal(cached, &wait); err != nil {
			return zero, infrastructureError(fmt.Errorf("decode event wait: %w", err))
		}
		if wait.EventName != eventName {
			return zero, infrastructureError(
				fmt.Errorf(
					"step %d changed awaited event from %q to %q",
					stepIndex,
					wait.EventName,
					eventName,
				),
			)
		}
	} else {
		now := wctx.runner.now()
		wait = waitForEventResult{EventName: eventName, Timeout: options.Timeout}
		if options.Timeout > 0 {
			wait.Deadline = now.Add(options.Timeout)
		}
		encoded, err := json.Marshal(wait)
		if err != nil {
			return zero, infrastructureError(err)
		}
		if err := instance.recordThat(&workflowWaiting{StepIndex: stepIndex, AwaitingEvent: eventName, WorkflowName: instance.workflowName, Timeout: options.Timeout, Deadline: wait.Deadline, StartedAt: now, Name: config.name}); err != nil {
			return zero, infrastructureError(err)
		}
		if err := instance.recordThat(&stepCompleted{StepIndex: stepIndex, Result: encoded, CompletedAt: now, WorkflowName: instance.workflowName, Name: config.name, Kind: stepKindWait}); err != nil {
			return zero, infrastructureError(err)
		}
		if err := saveOrConflictAbort(wctx.ctx, wctx.runner.repo, instance); err != nil {
			return zero, err
		}
	}
	if wait.Resolved {
		var result T
		if err := json.Unmarshal(wait.Payload, &result); err != nil {
			return zero, infrastructureError(fmt.Errorf("decode event payload: %w", err))
		}
		return result, nil
	}
	if wait.TimedOut {
		return zero, wctx.rememberStepError(stepIndex, ErrEventTimeout)
	}
	wctx.runner.waitStore.Register(wctx.ctx, wctx.instanceID, eventName, instance.workflowName)
	return resumeEventWait[T](wctx, stepIndex, wait, config)
}

func resumeEventWait[T any](
	wctx *Context,
	index int,
	wait waitForEventResult,
	config stepConfig,
) (T, error) {
	var zero T
	event, found, err := wctx.runner.waitStore.GetEvent(wctx.ctx, wctx.instanceID, wait.EventName)
	if err != nil {
		return zero, infrastructureError(fmt.Errorf("read awaited event: %w", err))
	}
	now := wctx.runner.now()
	if !found && !wait.Deadline.IsZero() && !now.Before(wait.Deadline) {
		// Publication may have completed between the first read and the clock
		// sample. Do not turn that on-time event into a timeout.
		event, found, err = wctx.runner.waitStore.GetEvent(
			wctx.ctx,
			wctx.instanceID,
			wait.EventName,
		)
		if err != nil {
			return zero, infrastructureError(fmt.Errorf("recheck awaited event: %w", err))
		}
	}
	if found && eventPublicationOnTime(event.PublishedAt, wait.Deadline) {
		var result T
		if err := json.Unmarshal(event.Payload, &result); err != nil {
			return zero, infrastructureError(fmt.Errorf("decode event payload: %w", err))
		}
		wait.Payload = event.Payload
		wait.Resolved = true
		if err := wctx.instance.recordThat(&workflowEventReceived{StepIndex: index, ReceivedEvent: wait.EventName, Payload: event.Payload, WorkflowName: wctx.instance.workflowName, PublishedAt: event.PublishedAt, ReceivedAt: now}); err != nil {
			return zero, infrastructureError(err)
		}
		if err := saveEventWait(wctx, index, wait, config); err != nil {
			return zero, err
		}
		return result, nil
	}
	if !wait.Deadline.IsZero() && !now.Before(wait.Deadline) {
		wait.TimedOut = true
		if err := saveEventWait(wctx, index, wait, config); err != nil {
			return zero, err
		}
		return zero, wctx.rememberStepError(index, ErrEventTimeout)
	}
	if !wait.Deadline.IsZero() {
		if err := wctx.runner.enqueueTask(wctx.ctx, QueuedTask{InstanceID: wctx.instanceID, WorkflowName: wctx.instance.workflowName, RunAfter: wait.Deadline}); err != nil {
			return zero, err
		}
	}
	wctx.runner.logger.DebugContext(
		wctx.ctx,
		"workflow awaiting event",
		"instanceID",
		wctx.instanceID,
		"step",
		index,
		"event",
		wait.EventName,
		"deadline",
		wait.Deadline,
	)
	return zero, ErrWorkflowWaiting
}

func saveEventWait(wctx *Context, index int, wait waitForEventResult, config stepConfig) error {
	encoded, err := json.Marshal(wait)
	if err != nil {
		return infrastructureError(err)
	}
	if err := wctx.instance.recordThat(&stepCompleted{StepIndex: index, Result: encoded, CompletedAt: wctx.runner.now(), WorkflowName: wctx.instance.workflowName, Name: config.name, Kind: stepKindWait}); err != nil {
		return infrastructureError(err)
	}
	return saveOrConflictAbort(wctx.ctx, wctx.runner.repo, wctx.instance)
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
		return infrastructureError(fmt.Errorf("marshal event payload: %w", err))
	}

	// Store the event and get any waiting workflows
	if eventName == "" || !utf8.ValidString(eventName) {
		return errors.New("event name must be nonempty valid UTF-8")
	}
	waiters, err := runner.waitStore.Publish(ctx, eventName, payloadJSON, runner.now())
	if err != nil {
		return infrastructureError(fmt.Errorf("publish event %q: %w", eventName, err))
	}

	// Persistent publication schedules wake-ups in its own transaction.
	if runner.tasksInTx {
		return nil
	}
	var enqueueErr error
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
			enqueueErr = errors.Join(
				enqueueErr,
				fmt.Errorf("enqueue event for %s: %w", w.InstanceID, err),
			)
		}
	}

	runner.logger.InfoContext(ctx,
		"event published",
		"eventName", eventName,
		"waitersWoken", len(waiters),
	)

	if enqueueErr != nil {
		return infrastructureError(enqueueErr)
	}
	return nil
}

// waitingWorkflow tracks a workflow that is waiting for an event.
type waitingWorkflow struct {
	InstanceID   InstanceID
	WorkflowName string
}

type publishedEvent struct {
	Payload     json.RawMessage
	PublishedAt time.Time
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
	Register(ctx context.Context, instanceID InstanceID, eventName string, workflowName string)

	// Publish stores an event payload and returns the list of workflows that were waiting.
	// First-write-wins. Repeated publication can repair a failed wake-up.
	Publish(
		ctx context.Context,
		eventName string,
		payload json.RawMessage,
		publishedAt time.Time,
	) ([]waitingWorkflow, error)

	// GetEvent checks if an event has been received for a specific workflow instance.
	GetEvent(
		ctx context.Context,
		instanceID InstanceID,
		eventName string,
	) (publishedEvent, bool, error)
}

// waitStore is the in-memory implementation of eventWaitStore.
// For distributed systems, use persistentWaitStore instead.
type waitStore struct {
	mu sync.Mutex

	// eventName → list of waiting workflows
	waiters map[string][]waitingWorkflow

	// eventName → payload (first-write-wins)
	published map[string]publishedEvent

	// instanceID:eventName → payload (for replay lookups)
	received map[string]publishedEvent
}

func newWaitStore() *waitStore {
	return &waitStore{
		waiters:   make(map[string][]waitingWorkflow),
		published: make(map[string]publishedEvent),
		received:  make(map[string]publishedEvent),
		mu:        sync.Mutex{},
	}
}

// Register records that a workflow is waiting for an event.
// If the event was already published, Register makes it available immediately.
// WaitForEvent checks it in the same execution.
func (ws *waitStore) Register(
	_ context.Context,
	instanceID InstanceID,
	eventName string,
	workflowName string,
) {
	ws.mu.Lock()
	defer ws.mu.Unlock()

	// If event already published, store for this instance
	if payload, ok := ws.published[eventName]; ok {
		key := string(instanceID) + ":" + eventName
		ws.received[key] = payload
		return
	}

	for _, waiter := range ws.waiters[eventName] {
		if waiter.InstanceID == instanceID {
			return
		}
	}
	ws.waiters[eventName] = append(ws.waiters[eventName], waitingWorkflow{
		InstanceID:   instanceID,
		WorkflowName: workflowName,
	})
}

// Publish stores an event payload and returns the list of workflows that were waiting.
// First-write-wins: if the event has already been published, returns nil.
func (ws *waitStore) Publish(
	_ context.Context,
	eventName string,
	payload json.RawMessage,
	publishedAt time.Time,
) ([]waitingWorkflow, error) {
	ws.mu.Lock()
	defer ws.mu.Unlock()

	// First-write-wins
	if _, ok := ws.published[eventName]; !ok {
		ws.published[eventName] = publishedEvent{Payload: payload, PublishedAt: publishedAt}
	}

	// Move all waiters to received
	waiters := ws.waiters[eventName]
	for _, w := range waiters {
		key := string(w.InstanceID) + ":" + eventName
		ws.received[key] = ws.published[eventName]
	}

	return waiters, nil
}

// GetEvent checks if an event has been received for a specific workflow instance.
func (ws *waitStore) GetEvent(
	_ context.Context,
	instanceID InstanceID,
	eventName string,
) (publishedEvent, bool, error) {
	ws.mu.Lock()
	defer ws.mu.Unlock()

	key := string(instanceID) + ":" + eventName
	payload, ok := ws.received[key]
	return payload, ok, nil
}

// Compile-time interface check.
var _ eventWaitStore = (*waitStore)(nil)
