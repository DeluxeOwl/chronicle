package workflow

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/DeluxeOwl/chronicle/event"
	"github.com/DeluxeOwl/chronicle/version"
)

type instanceView struct {
	root       *WorkflowInstance
	steps      map[int]*StepInfo
	history    []HistoryEvent
	version    version.Version
	updatedAt  time.Time
	finishedAt *time.Time
	nextRunAt  *time.Time
	waitingFor string
}

func newInstanceView() *instanceView {
	return &instanceView{
		root:       NewEmpty(),
		steps:      make(map[int]*StepInfo),
		history:    make([]HistoryEvent, 0),
		version:    0,
		updatedAt:  time.Time{},
		finishedAt: nil,
		nextRunAt:  nil,
		waitingFor: "",
	}
}

func decodeWorkflowEvent(record *event.Record) (WorkflowEvent, error) {
	var evt WorkflowEvent
	switch record.EventName() {
	case eventNameWorkflowStarted:
		evt = new(workflowStarted)
	case eventNameStepStarted:
		evt = new(stepStarted)
	case eventNameStepCompleted:
		evt = new(stepCompleted)
	case "workflow/step_failed":
		evt = new(stepFailed)
	case eventNameWorkflowCompleted:
		evt = new(workflowCompleted)
	case eventNameWorkflowFailed:
		evt = new(workflowFailed)
	case eventNameWorkflowRetried:
		evt = new(workflowRetried)
	case eventNameWorkflowWaiting:
		evt = new(workflowWaiting)
	case eventNameWorkflowEventReceived:
		evt = new(workflowEventReceived)
	case eventNameWorkflowCancelled:
		evt = new(workflowCancelled)
	default:
		return nil, fmt.Errorf("unsupported workflow event %q", record.EventName())
	}
	if err := json.Unmarshal(record.Data(), evt); err != nil {
		return nil, fmt.Errorf("decode workflow event %s: %w", record.EventName(), err)
	}
	return evt, nil
}

func (v *instanceView) step(index int) *StepInfo {
	if step := v.steps[index]; step != nil {
		return step
	}
	step := &StepInfo{
		Index:  index,
		Name:   fmt.Sprintf("Step %d", index+1),
		Kind:   stepKindStep,
		Status: string(StatusRunning),
	}
	v.steps[index] = step
	return step
}

func (v *instanceView) apply(record *event.Record, keepHistory bool) error {
	evt, err := decodeWorkflowEvent(record)
	if err != nil {
		return err
	}
	if err := v.root.Apply(evt); err != nil {
		return err
	}
	v.version = record.Version()
	recordedAt := v.applyMetadata(evt)
	if !recordedAt.IsZero() {
		v.updatedAt = recordedAt
	}
	if keepHistory {
		v.history = append(
			v.history,
			HistoryEvent{
				Version:    record.Version(),
				EventName:  record.EventName(),
				RecordedAt: timePointer(recordedAt),
				Data:       json.RawMessage(record.Data()),
			},
		)
	}
	return nil
}

//nolint:funlen // Every event has independent timeline metadata.
func (v *instanceView) applyMetadata(evt WorkflowEvent) time.Time {
	switch e := evt.(type) {
	case *workflowStarted:
		return e.StartedAt
	case *stepStarted:
		v.resumeSleeps(e.StepIndex, e.StartedAt, timePointer(e.StartedAt))
		step := v.step(e.StepIndex)
		step.Name = e.Name
		step.Kind = e.Kind
		step.Status = string(StatusRunning)
		step.StartedAt = timePointer(e.StartedAt)
		step.CompletedAt = nil
		step.Error = ""
		return e.StartedAt
	case *stepCompleted:
		v.resumeSleeps(e.StepIndex, e.CompletedAt, timePointer(e.CompletedAt))
		v.completeStep(e)
		return e.CompletedAt
	case *stepFailed:
		v.resumeSleeps(e.StepIndex, e.FailedAt, nil)
		step := v.step(e.StepIndex)
		step.Status = string(StatusFailed)
		step.Error = e.Error
		step.CompletedAt = timePointer(e.FailedAt)
		return e.FailedAt
	case *workflowCompleted:
		v.resumeSleeps(int(^uint(0)>>1), e.CompletedAt, timePointer(e.CompletedAt))
		v.finish(e.CompletedAt)
		return e.CompletedAt
	case *workflowFailed:
		v.resumeSleeps(v.root.currentStep, e.FailedAt, nil)
		v.finish(e.FailedAt)
		return e.FailedAt
	case *workflowCancelled:
		v.finish(e.CancelledAt)
		for _, step := range v.steps {
			if step.Status == string(StatusRunning) || step.Status == string(StatusWaiting) || step.Status == string(StatusSleeping) {
				step.Status = string(StatusCancelled)
			}
		}
		return e.CancelledAt
	case *workflowRetried:
		before := v.root.currentStep
		if e.FailedStepIndex != nil {
			before = *e.FailedStepIndex
		}
		v.resumeSleeps(before, e.ScheduledAt, nil)
		v.nextRunAt = timePointer(e.NextRunAfter)
		v.waitingFor = ""
		if e.FailedStepIndex != nil {
			step := v.step(*e.FailedStepIndex)
			step.Status = string(StatusFailed)
			step.Error = e.PreviousError
		}
		return e.ScheduledAt
	case *workflowWaiting:
		v.resumeSleeps(e.StepIndex, e.StartedAt, timePointer(e.StartedAt))
		step := v.step(e.StepIndex)
		step.Kind = stepKindWait
		step.Name = "Wait for event"
		if e.Name != "" {
			step.Name = e.Name
		}
		step.StartedAt = timePointer(e.StartedAt)
		step.Status = string(StatusWaiting)
		step.WaitingFor = e.AwaitingEvent
		step.WakeAt = timePointer(e.Deadline)
		v.waitingFor = e.AwaitingEvent
		v.nextRunAt = step.WakeAt
		return e.StartedAt
	case *workflowEventReceived:
		v.waitingFor = ""
		v.nextRunAt = nil
		return e.ReceivedAt
	}
	return time.Time{}
}

func (v *instanceView) completeStep(e *stepCompleted) {
	step := v.step(e.StepIndex)
	if e.Name != "" {
		step.Name = e.Name
	}
	if e.Kind != "" {
		step.Kind = e.Kind
	}
	step.Result = e.Result
	step.Error = ""
	step.Status = string(StatusCompleted)
	step.CompletedAt = timePointer(e.CompletedAt)
	var sleep sleepResult
	if (e.Kind == stepKindSleep || e.Kind == "") && json.Unmarshal(e.Result, &sleep) == nil &&
		!sleep.WakeAt.IsZero() {
		step.Kind = stepKindSleep
		if e.Name == "" {
			step.Name = "Sleep"
		}
		step.StartedAt = timePointer(e.CompletedAt)
		step.CompletedAt = nil
		step.Status = string(StatusSleeping)
		step.WakeAt = timePointer(sleep.WakeAt)
		v.nextRunAt = step.WakeAt
		return
	}
	var wait waitForEventResult
	if (e.Kind == stepKindWait || e.Kind == "") && json.Unmarshal(e.Result, &wait) == nil &&
		wait.EventName != "" {
		step.Kind = stepKindWait
		step.WaitingFor = wait.EventName
		step.WakeAt = timePointer(wait.Deadline)
		if !wait.Resolved && !wait.TimedOut {
			step.Status = string(StatusWaiting)
			step.CompletedAt = nil
			v.waitingFor = wait.EventName
			v.nextRunAt = step.WakeAt
		} else {
			v.waitingFor = ""
			v.nextRunAt = nil
			if wait.TimedOut {
				step.Status = "timed_out"
				step.Error = ErrEventTimeout.Error()
			}
		}
		return
	}
	v.nextRunAt = nil
}

func (v *instanceView) resumeSleeps(before int, at time.Time, completedAt *time.Time) {
	if at.IsZero() {
		return
	}
	for index, step := range v.steps {
		if index < before && step.Kind == stepKindSleep && step.Status == string(StatusSleeping) &&
			step.WakeAt != nil && !at.Before(*step.WakeAt) {
			step.Status = string(StatusCompleted)
			// Failure and retry decisions prove progress, but record no exact
			// sleep resumption time. Their callers leave completedAt unknown.
			step.CompletedAt = completedAt
		}
	}
}

func (v *instanceView) finish(at time.Time) {
	v.finishedAt = timePointer(at)
	v.nextRunAt = nil
	v.waitingFor = ""
}

func (v *instanceView) summary(now time.Time) InstanceSummary {
	status := v.root.status
	if status == StatusRunning && now.Before(v.root.nextRunAfter) {
		if v.root.deferredKind == stepKindSleep {
			status = StatusSleeping
		}
		if v.root.deferredKind == deferredRetry {
			status = StatusRetrying
		}
	}
	completed := 0
	for _, step := range v.steps {
		if step.Status == string(StatusCompleted) {
			completed++
		}
	}
	return InstanceSummary{
		InstanceID:     v.root.id,
		WorkflowName:   v.root.workflowName,
		Status:         status,
		StartedAt:      v.root.startedAt,
		UpdatedAt:      v.updatedAt,
		FinishedAt:     v.finishedAt,
		NextRunAt:      v.nextRunAt,
		WaitingFor:     v.waitingFor,
		Attempt:        v.root.attempt,
		CurrentStep:    v.root.currentStep,
		CompletedSteps: completed,
		Error:          v.root.failureError,
		Version:        v.version,
	}
}

func timePointer(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}
