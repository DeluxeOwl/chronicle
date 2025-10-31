package event

import "time"

type CheckpointInfo struct {
	// EventsSinceLastSave is the number of events processed since the last checkpoint was saved.
	EventsSinceLastSave int
	// TimeSinceLastSave is the duration that has passed since the last checkpoint was saved.
	TimeSinceLastSave time.Duration
}

// CheckpointPolicy determines when a checkpoint should be saved.
type CheckpointPolicy interface {
	// ShouldCheckpoint is called after each event is processed to decide if it's time to save a checkpoint.
	ShouldCheckpoint(info CheckpointInfo) bool
}

type eventCountPolicy struct {
	count int
}

func (p *eventCountPolicy) ShouldCheckpoint(info CheckpointInfo) bool {
	return info.EventsSinceLastSave >= p.count
}

// EveryNEvents returns a policy that triggers a checkpoint after N events have been processed.
// A checkpoint is also always saved at the end of a processed batch.
func EveryNEvents(n int) CheckpointPolicy {
	return &eventCountPolicy{count: n}
}

// durationPolicy triggers a checkpoint after a certain amount of time has passed.
type durationPolicy struct {
	duration time.Duration
}

func (p *durationPolicy) ShouldCheckpoint(info CheckpointInfo) bool {
	return info.TimeSinceLastSave >= p.duration
}

// AfterDuration returns a policy that triggers a checkpoint after a given duration has passed
// since the last checkpoint.
// A checkpoint is also always saved at the end of a processed batch.
func AfterDuration(d time.Duration) CheckpointPolicy {
	return &durationPolicy{duration: d}
}

type compositePolicy struct {
	policies []CheckpointPolicy
}

func (p *compositePolicy) ShouldCheckpoint(info CheckpointInfo) bool {
	for _, policy := range p.policies {
		if policy.ShouldCheckpoint(info) {
			return true
		}
	}
	return false
}

// AnyOf returns a policy that triggers a checkpoint if any of the provided policies trigger.
// This allows you to combine policies, e.g., "Every 100 events OR after 5 seconds".
func AnyOf(policies ...CheckpointPolicy) CheckpointPolicy {
	return &compositePolicy{policies: policies}
}

// batchEndPolicy is the default policy that only triggers a checkpoint at the end of a batch.
type batchEndPolicy struct{}

func (p *batchEndPolicy) ShouldCheckpoint(info CheckpointInfo) bool {
	return false // Never trigger mid-batch, only at the end.
}
