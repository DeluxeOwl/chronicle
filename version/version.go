package version

import "fmt"

type Version uint64

//nolint:gochecknoglobals // It's a helper.
var SelectFromBeginning = Selector{From: 0}

type Selector struct {
	From Version
}

type Check interface {
	isVersionCheck()
}

type CheckAny struct{}

func (CheckAny) isVersionCheck() {}

type CheckExact Version

func (CheckExact) isVersionCheck() {}

func NewConflictError(expected Version, actual Version) *ConflictError {
	return &ConflictError{
		expected: expected,
		actual:   actual,
	}
}

type ConflictError struct {
	expected Version
	actual   Version
}

func (err ConflictError) Error() string {
	return fmt.Sprintf(
		"version conflict error: expected log version: %d, actual: %d",
		err.expected,
		err.actual,
	)
}
