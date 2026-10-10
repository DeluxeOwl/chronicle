package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/DeluxeOwl/chronicle/aggregate"
	"github.com/DeluxeOwl/chronicle/event"
	"github.com/DeluxeOwl/chronicle/version"
)

// Inspector reads workflow state from persisted events without executing code.
// Listing requires a finite, non-tailing global log. Each List call scans history, so use a separate
// projection for large installations rather than treating this as a search index.
type Inspector struct {
	log     event.GlobalLog
	nowFunc func() time.Time
}

type InspectorOption func(*Inspector)

func WithInspectorNowFunc(fn func() time.Time) InspectorOption {
	return func(i *Inspector) { i.nowFunc = fn }
}

func NewInspector(log event.GlobalLog, opts ...InspectorOption) (*Inspector, error) {
	if log == nil {
		return nil, errors.New("inspector event log cannot be nil")
	}
	i := &Inspector{log: log, nowFunc: time.Now}
	for _, opt := range opts {
		if opt == nil {
			return nil, errors.New("inspector option cannot be nil")
		}
		opt(i)
	}
	if i.nowFunc == nil {
		return nil, errors.New("inspector clock cannot be nil")
	}
	return i, nil
}

// ListOptions filters persisted instances before applying pagination.
type ListOptions struct {
	WorkflowName string `exhaustruct:"optional"`
	Status       Status `exhaustruct:"optional"`
	Search       string `exhaustruct:"optional"`
	Limit        int    `exhaustruct:"optional"`
	Offset       int    `exhaustruct:"optional"`
}

type InstancePage struct {
	Instances []InstanceSummary `json:"instances"`
	Total     int               `json:"total"`
}

type InstanceSummary struct {
	InstanceID     InstanceID      `json:"instanceID"`
	WorkflowName   string          `json:"workflowName"`
	Status         Status          `json:"status"`
	StartedAt      time.Time       `json:"startedAt"`
	UpdatedAt      time.Time       `json:"updatedAt"`
	FinishedAt     *time.Time      `json:"finishedAt,omitempty" exhaustruct:"optional"`
	NextRunAt      *time.Time      `json:"nextRunAt,omitempty" exhaustruct:"optional"`
	WaitingFor     string          `json:"waitingFor,omitempty" exhaustruct:"optional"`
	Attempt        int             `json:"attempt"`
	CurrentStep    int             `json:"currentStep"`
	CompletedSteps int             `json:"completedSteps"`
	Error          string          `json:"error,omitempty" exhaustruct:"optional"`
	Version        version.Version `json:"version"`
}

type InstanceDetail struct {
	Summary InstanceSummary `json:"summary"`
	Params  json.RawMessage `json:"params"`
	Output  json.RawMessage `json:"output,omitempty" exhaustruct:"optional"`
	Steps   []StepInfo      `json:"steps"`
	History []HistoryEvent  `json:"history"`
}

type StepInfo struct {
	Index       int             `json:"index"`
	Name        string          `json:"name"`
	Kind        string          `json:"kind"`
	Status      string          `json:"status"`
	StartedAt   *time.Time      `json:"startedAt,omitempty" exhaustruct:"optional"`
	CompletedAt *time.Time      `json:"completedAt,omitempty" exhaustruct:"optional"`
	WakeAt      *time.Time      `json:"wakeAt,omitempty" exhaustruct:"optional"`
	WaitingFor  string          `json:"waitingFor,omitempty" exhaustruct:"optional"`
	Error       string          `json:"error,omitempty" exhaustruct:"optional"`
	Result      json.RawMessage `json:"result,omitempty" exhaustruct:"optional"`
}

type HistoryEvent struct {
	Version    version.Version `json:"version"`
	EventName  string          `json:"eventName"`
	RecordedAt *time.Time      `json:"recordedAt,omitempty" exhaustruct:"optional"`
	Data       json.RawMessage `json:"data"`
}

//nolint:gocognit // The scan and filter share a single read-consistent view.
func (i *Inspector) List(ctx context.Context, opts ListOptions) (InstancePage, error) {
	if opts.Status != "" && opts.Status != StatusRunning && opts.Status != StatusWaiting &&
		opts.Status != StatusSleeping &&
		opts.Status != StatusRetrying &&
		opts.Status != StatusCompleted &&
		opts.Status != StatusFailed &&
		opts.Status != StatusCancelled {
		return InstancePage{}, errors.New("invalid workflow status filter")
	}
	if opts.Limit < 0 || opts.Limit > 200 || opts.Offset < 0 {
		return InstancePage{}, errors.New(
			"inspector limit must be between 0 and 200 and offset must be nonnegative",
		)
	}
	if opts.Limit == 0 {
		opts.Limit = 50
	}
	if err := ctx.Err(); err != nil {
		return InstancePage{}, err
	}
	states := make(map[event.LogID]*instanceView)
	for record, err := range i.log.ReadAllEvents(ctx, version.SelectFromBeginning) {
		if err != nil {
			return InstancePage{}, fmt.Errorf("list workflow events: %w", err)
		}
		if record.EventName() == eventNameWorkflowStarted {
			states[record.LogID()] = newInstanceView()
		}
		view := states[record.LogID()]
		if view != nil {
			if err := view.apply(event.NewRecord(record.Version(), record.LogID(), record.EventName(), record.Data()), false); err != nil {
				return InstancePage{}, err
			}
		}
	}
	instances := make([]InstanceSummary, 0, len(states))
	now := i.nowFunc()
	search := strings.ToLower(opts.Search)
	for _, view := range states {
		summary := view.summary(now)
		if opts.WorkflowName != "" && summary.WorkflowName != opts.WorkflowName {
			continue
		}
		if opts.Status != "" && summary.Status != opts.Status {
			continue
		}
		if search != "" &&
			!strings.Contains(
				strings.ToLower(string(summary.InstanceID)+" "+summary.WorkflowName),
				search,
			) {
			continue
		}
		instances = append(instances, summary)
	}
	slices.SortFunc(instances, func(a, b InstanceSummary) int {
		if c := b.StartedAt.Compare(a.StartedAt); c != 0 {
			return c
		}
		return strings.Compare(string(a.InstanceID), string(b.InstanceID))
	})
	total := len(instances)
	from := min(opts.Offset, total)
	to := from + min(opts.Limit, total-from)
	return InstancePage{Instances: instances[from:to], Total: total}, nil
}

func (i *Inspector) Get(ctx context.Context, id InstanceID) (InstanceDetail, error) {
	if err := ctx.Err(); err != nil {
		return InstanceDetail{}, err
	}
	view := newInstanceView()
	for record, err := range i.log.ReadEvents(ctx, event.LogID(id), version.SelectFromBeginning) {
		if err != nil {
			return InstanceDetail{}, fmt.Errorf("get workflow events: %w", err)
		}
		if view.root.id == "" && record.EventName() != eventNameWorkflowStarted {
			return InstanceDetail{}, fmt.Errorf("workflow %s: %w", id, aggregate.ErrRootNotFound)
		}
		if err := view.apply(record, true); err != nil {
			return InstanceDetail{}, err
		}
	}
	if view.root.id == "" {
		return InstanceDetail{}, fmt.Errorf("workflow %s: %w", id, aggregate.ErrRootNotFound)
	}
	steps := make([]StepInfo, 0, len(view.steps))
	for _, step := range view.steps {
		steps = append(steps, *step)
	}
	slices.SortFunc(steps, func(a, b StepInfo) int { return a.Index - b.Index })
	return InstanceDetail{
		Summary: view.summary(i.nowFunc()),
		Params:  view.root.params,
		Output:  view.root.output,
		Steps:   steps,
		History: view.history,
	}, nil
}
