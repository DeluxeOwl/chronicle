package workflow

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/DeluxeOwl/chronicle"
	"github.com/DeluxeOwl/chronicle/aggregate"
	"github.com/DeluxeOwl/chronicle/event"
	"github.com/DeluxeOwl/chronicle/eventlog"
)

// WorkflowInstance represents a running workflow as an aggregate.
// It tracks the workflow state and completed steps.
type WorkflowInstance struct {
	aggregate.Base

	id           InstanceID
	workflowName string
	status       Status
	params       []byte
	output       []byte
	stepResults  map[int][]byte // step_index -> serialized result
	currentStep  int
}

type InstanceID string

func (i InstanceID) String() string { return string(i) }

func (w *WorkflowInstance) ID() InstanceID {
	return w.id
}

type Status string

const (
	StatusPending   Status = "pending"
	StatusRunning   Status = "running"
	StatusSleeping  Status = "sleeping"
	StatusCompleted Status = "completed"
	StatusFailed    Status = "failed"
)

//sumtype:decl
type WorkflowEvent interface {
	event.Any
	isWorkflowEvent()
}

type workflowStarted struct {
	InstanceID   string          `json:"instanceID"`
	WorkflowName string          `json:"workflowName"`
	Params       json.RawMessage `json:"params"`
	StartedAt    time.Time       `json:"startedAt"`
}

func (*workflowStarted) EventName() string { return "workflow/started" }
func (*workflowStarted) isWorkflowEvent()  {}

type stepCompleted struct {
	StepIndex int             `json:"stepIndex"`
	Result    json.RawMessage `json:"result"`
}

func (*stepCompleted) EventName() string { return "workflow/step_completed" }
func (*stepCompleted) isWorkflowEvent()  {}

type stepFailed struct {
	StepIndex int    `json:"stepIndex"`
	Error     string `json:"error"`
}

func (*stepFailed) EventName() string { return "workflow/step_failed" }
func (*stepFailed) isWorkflowEvent()  {}

type workflowCompleted struct {
	Result json.RawMessage `json:"result"`
}

func (*workflowCompleted) EventName() string { return "workflow/completed" }
func (*workflowCompleted) isWorkflowEvent()  {}

type workflowFailed struct {
	Error string `json:"error"`
}

func (*workflowFailed) EventName() string { return "workflow/failed" }
func (*workflowFailed) isWorkflowEvent()  {}

func (w *WorkflowInstance) EventFuncs() event.FuncsFor[WorkflowEvent] {
	return event.FuncsFor[WorkflowEvent]{
		func() WorkflowEvent { return new(workflowStarted) },
		func() WorkflowEvent { return new(stepCompleted) },
		func() WorkflowEvent { return new(stepFailed) },
		func() WorkflowEvent { return new(workflowCompleted) },
		func() WorkflowEvent { return new(workflowFailed) },
	}
}

func (w *WorkflowInstance) Apply(evt WorkflowEvent) error {
	switch e := evt.(type) {
	case *workflowStarted:
		w.id = InstanceID(e.InstanceID)
		w.workflowName = e.WorkflowName
		w.params = e.Params
		w.status = StatusRunning
		if w.stepResults == nil {
			w.stepResults = make(map[int][]byte)
		}
	case *stepCompleted:
		w.stepResults[e.StepIndex] = e.Result
		w.currentStep = e.StepIndex + 1
	case *stepFailed:
		w.status = StatusFailed
	case *workflowCompleted:
		w.output = e.Result
		w.status = StatusCompleted
	case *workflowFailed:
		w.status = StatusFailed
	default:
		return fmt.Errorf("unexpected event kind: %T", evt)
	}
	return nil
}

func (w *WorkflowInstance) recordThat(event WorkflowEvent) error {
	return aggregate.RecordEvent(w, event)
}

func NewEmpty() *WorkflowInstance {
	//nolint:exhaustruct // not needed.
	return &WorkflowInstance{
		stepResults: make(map[int][]byte),
	}
}

// Runner manages workflow execution, it requires an event log.
type Runner struct {
	repo     aggregate.Repository[InstanceID, WorkflowEvent, *WorkflowInstance]
	logger   *slog.Logger
	registry event.Registry[event.Any]
}

// NewRunner creates a new workflow runner with the given event log.
func NewRunner(eventLog event.Log, logger *slog.Logger) (*Runner, error) {
	if logger == nil {
		logger = slog.Default()
	}

	// Use a fresh registry to avoid duplicate registration issues
	registry := event.NewRegistry[event.Any]()

	// Register events using a wrapper
	emptyInstance := NewEmpty()
	wrapper := &eventFuncWrapper{funcs: emptyInstance.EventFuncs()}
	if err := registry.RegisterEvents(wrapper); err != nil {
		return nil, fmt.Errorf("register workflow events: %w", err)
	}

	repo, err := chronicle.NewEventSourcedRepository(
		eventLog,
		NewEmpty,
		nil, // no transformers for now
		aggregate.DontRegisterRoot(),
		aggregate.AnyEventRegistry(registry),
	)
	if err != nil {
		return nil, fmt.Errorf("create workflow repository: %w", err)
	}

	return &Runner{
		repo:     repo,
		logger:   logger,
		registry: registry,
	}, nil
}

// NewSqliteRunner creates a new workflow runner backed by SQLite.
func NewSqliteRunner(db *sql.DB) (*Runner, error) {
	eventLog, err := eventlog.NewSqlite(db)
	if err != nil {
		return nil, fmt.Errorf("create sqlite event log: %w", err)
	}

	return NewRunner(eventLog, slog.Default())
}

// Context is the workflow execution context passed to workflow functions.
// It implements the context.Context interface.
type Context struct {
	ctx        context.Context
	instanceID InstanceID
	runner     *Runner
}

func (c *Context) Value(key any) any {
	return c.ctx.Value(key)
}

func (c *Context) Done() <-chan struct{} {
	return c.ctx.Done()
}

func (c *Context) Err() error {
	return c.ctx.Err()
}

func (c *Context) Deadline() (time.Time, bool) {
	return c.ctx.Deadline()
}

// Workflow represents a registered workflow that can be executed.
type Workflow[Params any, Output any] struct {
	runner *Runner
	name   string
	fn     func(Context, *Params) (*Output, error)
	logger *slog.Logger
}

// New registers a new workflow with the given name and function.
func New[Params any, Output any](
	runner *Runner,
	name string,
	fn func(Context, *Params) (*Output, error),
) *Workflow[Params, Output] {
	return &Workflow[Params, Output]{
		runner: runner,
		name:   name,
		fn:     fn,
		logger: runner.logger.With("workflow", name),
	}
}

// Start begins a new workflow instance with the given parameters.
// Returns the instance ID that can be used to track the workflow.
func (w *Workflow[Params, Output]) Start(ctx context.Context, params *Params) (InstanceID, error) {
	instanceID := InstanceID(generateInstanceID())
	w.logger.InfoContext(ctx, "starting workflow", "instanceID", instanceID)

	paramsJSON, err := json.Marshal(params)
	if err != nil {
		return "", fmt.Errorf("marshal params: %w", err)
	}

	instance := NewEmpty()
	instance.id = instanceID

	// Record the started event
	if err := instance.recordThat(&workflowStarted{
		InstanceID:   string(instanceID),
		WorkflowName: w.name,
		Params:       paramsJSON,
		StartedAt:    time.Now(),
	}); err != nil {
		return "", fmt.Errorf("record workflow started: %w", err)
	}

	// Save to repository
	if _, _, err := w.runner.repo.Save(ctx, instance); err != nil {
		return "", fmt.Errorf("save workflow instance: %w", err)
	}

	w.logger.InfoContext(ctx, "workflow started", "instanceID", instanceID)

	return instanceID, nil
}

// Run executes a workflow instance to completion.
// If the instance is new, it runs from the beginning.
// If the instance has already partially executed, it resumes from the last completed step.
func (w *Workflow[Params, Output]) Run(
	ctx context.Context,
	instanceID InstanceID,
) (*Output, error) {
	// Load workflow instance fresh from the repository
	instance, err := w.runner.repo.Get(ctx, instanceID)
	if err != nil {
		return nil, fmt.Errorf("load workflow instance: %w", err)
	}

	// Check if already completed
	if instance.status == StatusCompleted {
		var output Output
		if err := json.Unmarshal(instance.output, &output); err != nil {
			return nil, fmt.Errorf("unmarshal completed output: %w", err)
		}
		return &output, nil
	}

	// Check if failed
	if instance.status == StatusFailed {
		return nil, fmt.Errorf("workflow instance %s has failed", instanceID)
	}

	w.logger.DebugContext(
		ctx,
		"running workflow",
		"instanceID",
		instanceID,
		"step",
		instance.currentStep,
	)

	// Deserialize params
	var params Params
	if err := json.Unmarshal(instance.params, &params); err != nil {
		return nil, fmt.Errorf("unmarshal params: %w", err)
	}

	// Create workflow context
	wctx := Context{
		ctx:        ctx,
		instanceID: instanceID,
		runner:     w.runner,
	}

	// Execute workflow function
	output, err := w.fn(wctx, &params)
	if err != nil {
		// Reload instance to get latest state
		instance, loadErr := w.runner.repo.Get(ctx, instanceID)
		if loadErr != nil {
			return nil, fmt.Errorf(
				"workflow failed and failed to reload: %w (original error: %w)",
				loadErr,
				err,
			)
		}
		// Record failure
		if err := instance.recordThat(&workflowFailed{
			Error: err.Error(),
		}); err != nil {
			return nil, fmt.Errorf("record workflow failure: %w", err)
		}
		if _, _, saveErr := w.runner.repo.Save(ctx, instance); saveErr != nil {
			return nil, fmt.Errorf(
				"workflow failed and failed to save: %w (original error: %w)",
				saveErr,
				err,
			)
		}
		return nil, err
	}

	// Reload instance to get latest state (steps may have been recorded)
	instance, err = w.runner.repo.Get(ctx, instanceID)
	if err != nil {
		return nil, fmt.Errorf("reload instance after workflow completion: %w", err)
	}

	// Serialize output
	outputJSON, err := json.Marshal(output)
	if err != nil {
		return nil, fmt.Errorf("marshal output: %w", err)
	}

	// Record completion
	if err := instance.recordThat(&workflowCompleted{
		Result: outputJSON,
	}); err != nil {
		return nil, fmt.Errorf("record workflow completion: %w", err)
	}

	// Save final state
	if _, _, err := w.runner.repo.Save(ctx, instance); err != nil {
		return nil, fmt.Errorf("save completed workflow: %w", err)
	}

	w.logger.InfoContext(ctx, "workflow completed", "instanceID", instanceID)

	return output, nil
}

// GetResult retrieves the result of a completed workflow instance.
func (w *Workflow[Params, Output]) GetResult(
	ctx context.Context,
	instanceID InstanceID,
) (*Output, error) {
	instance, err := w.runner.repo.Get(ctx, instanceID)
	if err != nil {
		return nil, fmt.Errorf("load workflow instance: %w", err)
	}

	if instance.status != StatusCompleted {
		return nil, fmt.Errorf("workflow not completed, current status: %s", instance.status)
	}

	var output Output
	if err := json.Unmarshal(instance.output, &output); err != nil {
		return nil, fmt.Errorf("unmarshal output: %w", err)
	}

	return &output, nil
}

// Step executes a workflow step function and caches its result.
// If the step has already been executed for this workflow instance,
// the cached result is returned instead of re-executing the function.
func Step[Result any](wctx Context, fn func(context.Context) (Result, error)) (Result, error) {
	var zero Result

	// Always reload the instance to get the latest state
	instance, err := wctx.runner.repo.Get(wctx.ctx, wctx.instanceID)
	if err != nil {
		return zero, fmt.Errorf("reload instance for step: %w", err)
	}

	// Determine the step index based on how many steps have been completed
	stepIndex := instance.currentStep

	// Check if step already completed (replay mode)
	if cachedResult, ok := instance.stepResults[stepIndex]; ok {
		wctx.runner.logger.Debug("step replay", "instanceID", wctx.instanceID, "step", stepIndex)
		var result Result
		if err := json.Unmarshal(cachedResult, &result); err != nil {
			return zero, fmt.Errorf("unmarshal cached step result: %w", err)
		}
		return result, nil
	}

	// Execute the step function
	result, err := fn(wctx.ctx)
	if err != nil {
		// Record step failure
		if err := instance.recordThat(&stepFailed{
			StepIndex: stepIndex,
			Error:     err.Error(),
		}); err != nil {
			return zero, fmt.Errorf("record step failure: %w", err)
		}
		if _, _, saveErr := wctx.runner.repo.Save(wctx.ctx, instance); saveErr != nil {
			return zero, fmt.Errorf(
				"step failed and failed to save: %w (original error: %w)",
				saveErr,
				err,
			)
		}
		return zero, err
	}

	// Serialize and store result
	resultJSON, err := json.Marshal(result)
	if err != nil {
		return zero, fmt.Errorf("marshal step result: %w", err)
	}

	// Record step completion
	if err := instance.recordThat(&stepCompleted{
		StepIndex: stepIndex,
		Result:    resultJSON,
	}); err != nil {
		return zero, fmt.Errorf("record step completion: %w", err)
	}

	// Save after each step for durability
	if _, _, err := wctx.runner.repo.Save(wctx.ctx, instance); err != nil {
		return zero, fmt.Errorf("save step result: %w", err)
	}

	return result, nil
}

// Step2 executes a workflow step function that returns only an error.
// Like Step, it caches the result to allow replay without re-execution.
func Step2(wctx Context, fn func(context.Context) error) error {
	// Always reload the instance to get the latest state
	instance, err := wctx.runner.repo.Get(wctx.ctx, wctx.instanceID)
	if err != nil {
		return fmt.Errorf("reload instance for step2: %w", err)
	}

	// Determine the step index based on how many steps have been completed
	stepIndex := instance.currentStep

	// Check if step already completed (replay mode)
	if _, ok := instance.stepResults[stepIndex]; ok {
		wctx.runner.logger.Debug(
			"step2 already completed, skipping",
			"instanceID",
			wctx.instanceID,
			"stepIndex",
			stepIndex,
		)
		return nil
	}

	// Execute the step function
	wctx.runner.logger.Debug(
		"executing step2 function",
		"instanceID",
		wctx.instanceID,
		"stepIndex",
		stepIndex,
	)
	if err := fn(wctx.ctx); err != nil {
		wctx.runner.logger.Error(
			"step2 function failed",
			"instanceID",
			wctx.instanceID,
			"stepIndex",
			stepIndex,
			"error",
			err,
		)
		// Record step failure
		if err := instance.recordThat(&stepFailed{
			StepIndex: stepIndex,
			Error:     err.Error(),
		}); err != nil {
			return fmt.Errorf("record step failure: %w", err)
		}
		if _, _, saveErr := wctx.runner.repo.Save(wctx.ctx, instance); saveErr != nil {
			return fmt.Errorf(
				"step failed and failed to save: %w (original error: %w)",
				saveErr,
				err,
			)
		}
		return err
	}

	// Record step completion with empty result
	if err := instance.recordThat(&stepCompleted{
		StepIndex: stepIndex,
		Result:    []byte("null"),
	}); err != nil {
		return fmt.Errorf("record step completion: %w", err)
	}

	// Save after each step for durability
	_, _, err = wctx.runner.repo.Save(wctx.ctx, instance)
	if err != nil {
		return fmt.Errorf("save step result: %w", err)
	}

	return nil
}

func generateInstanceID() string {
	return fmt.Sprintf("wf_%d", time.Now().UnixNano())
}

// eventFuncWrapper wraps WorkflowEvent funcs to work with event.Any
type eventFuncWrapper struct {
	funcs event.FuncsFor[WorkflowEvent]
}

func (w *eventFuncWrapper) EventFuncs() event.FuncsFor[event.Any] {
	anyFuncs := make(event.FuncsFor[event.Any], len(w.funcs))
	for i, fn := range w.funcs {
		anyFuncs[i] = func() event.Any { return fn() }
	}
	return anyFuncs
}

var (
	// Compile-time interface checks
	_ aggregate.Aggregate[InstanceID, WorkflowEvent] = (*WorkflowInstance)(nil)
	_ aggregate.Root[InstanceID, WorkflowEvent]      = (*WorkflowInstance)(nil)
	_ aggregate.IDer[InstanceID]                     = (*WorkflowInstance)(nil)

	// Context interface checks
	_ context.Context = (*Context)(nil)
)
