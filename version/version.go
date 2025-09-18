package version

import "fmt"

// Version represents the sequential number of an event within an aggregate's stream,
// or the state of an aggregate after a sequence of events has been applied.
// It is the base of optimistic concurrency control in the framework.
// An aggregate with no events has version 0. The first event applied results in version 1.
type Version uint64

// Zero represents the initial version of an aggregate before any events have been recorded.
// A new aggregate that has not yet been saved has version 0.
var Zero Version = 0

// SelectFromBeginning is a convenience selector used to read an aggregate's entire
// event history from the very first event.
//
// Usage:
//
//	repo.GetVersion(ctx, myID, version.SelectFromBeginning)
//
//nolint:gochecknoglobals // It's a helper.
var SelectFromBeginning = Selector{From: 0}

// Selector specifies a range of events to retrieve from the event log.
//
// Usage:
//
//	// Get events from version 11 onwards
//	selector := version.Selector{From: 11}
//	repo.GetVersion(ctx, myID, selector)
//
//	// Get events up to version 25
//	selector := version.Selector{To: 25}
//	repo.GetVersion(ctx, myID, selector)
//
//	// Get events from version 11 to version 25. ⚠️ Only use this if you know what you're doing.
//	selector := version.Selector{From: 11, To: 25}
//	repo.GetVersion(ctx, myID, selector)
type Selector struct {
	From Version `exhaustruct:"optional"`
	To   Version `exhaustruct:"optional"`
}

func SelectInterval(from Version, to Version) Selector {
	return Selector{
		From: from,
		To:   to,
	}
}

func SelectExact(to Version) Selector {
	return Selector{
		From: 0,
		To:   to,
	}
}

// Check defines the contract for an optimistic concurrency version check. Implementations
// of this interface are passed to the `AppendEvents` method of an event log to ensure
// that the state of the aggregate has not changed since it was last read.
type Check interface {
	isVersionCheck()
}

// CheckExact is an implementation of the Check interface that requires the aggregate's
// current version in the event log to exactly match the expected version. This is the
// most common strategy for optimistic concurrency.
//
// Usage:
//
//	// Expects the aggregate to be at version 5 before appending new events.
//	expected := version.CheckExact(5)
//	store.AppendEvents(ctx, id, expected, rawEvents)
type CheckExact Version

func (CheckExact) isVersionCheck() {}

// CheckExact performs the validation against the actual version from the store.
//
// Returns a `*ConflictError` if the versions do not match, or nil if they do.
func (expected CheckExact) CheckExact(actualVersion Version) error {
	if actualVersion != Version(expected) {
		return NewConflictError(Version(expected), actualVersion)
	}
	return nil
}

// NewConflictError is a constructor for creating a ConflictError.
// It is used internally by event log implementations when an optimistic
// concurrency check fails.
func NewConflictError(expected Version, actual Version) *ConflictError {
	return &ConflictError{
		Expected: expected,
		Actual:   actual,
	}
}

// ConflictError is the error returned when an attempt to save an aggregate fails
// due to an optimistic concurrency violation. This means that another process has
// modified the aggregate between the read and write operations.
//
// The 'Expected' field holds the version the application thought the aggregate was at,
// and 'Actual' holds the version that was found in the event log. This information
// is useful for handling the conflict, for example by retrying the operation.
type ConflictError struct {
	Expected Version
	Actual   Version
}

func (err ConflictError) Error() string {
	return fmt.Sprintf(
		"version conflict error: expected log version: %d, actual: %d",
		err.Expected,
		err.Actual,
	)
}
