package workflow

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	stepKindWait  = "wait"
	stepKindSleep = "sleep"
	deferredRetry = "retry"
)

// StepOption configures a step without changing its positional replay identity.
type StepOption func(*stepConfig)

type stepConfig struct{ name string }

// WithStepName records a human-readable name and a durable step-start event.
// Keep names and step order stable across deployments. Unnamed steps retain the
// existing completion-only event format for backward compatibility.
func WithStepName(name string) StepOption {
	return func(c *stepConfig) { c.name = name }
}

func configureStep(opts []StepOption) (stepConfig, error) {
	config := stepConfig{name: ""}
	for _, opt := range opts {
		if opt == nil {
			return config, errors.New("step option cannot be nil")
		}
		opt(&config)
	}
	if config.name != "" &&
		(!utf8.ValidString(config.name) || strings.TrimSpace(config.name) == "") {
		return config, errors.New("step name must be valid UTF-8 and contain non-space characters")
	}
	return config, nil
}

type stepStarted struct {
	StepIndex int       `json:"stepIndex"`
	Name      string    `json:"name"`
	Kind      string    `json:"kind"`
	StartedAt time.Time `json:"startedAt"`
}

func (*stepStarted) EventName() string { return eventNameStepStarted }
func (*stepStarted) isWorkflowEvent()  {}

func validateStep(wctx *Context, index int, kind string, config stepConfig) error {
	if err := wctx.ctx.Err(); err != nil {
		return err
	}
	if previous := wctx.instance.stepNames[index]; previous != "" && config.name != previous {
		return infrastructureError(
			fmt.Errorf("step %d name changed from %q to %q", index, previous, config.name),
		)
	}
	if previous := wctx.instance.stepKinds[index]; previous != "" && kind != previous {
		return infrastructureError(
			fmt.Errorf("step %d changed kind from %q to %q", index, previous, kind),
		)
	}
	return nil
}

func beginStep(wctx *Context, index int, kind string, config stepConfig) error {
	if err := validateStep(wctx, index, kind, config); err != nil {
		return err
	}
	if config.name == "" {
		return nil
	}
	if err := wctx.instance.recordThat(&stepStarted{StepIndex: index, Name: config.name, Kind: kind, StartedAt: wctx.runner.now()}); err != nil {
		return infrastructureError(err)
	}
	return saveOrConflictAbort(wctx.ctx, wctx.runner.repo, wctx.instance)
}
