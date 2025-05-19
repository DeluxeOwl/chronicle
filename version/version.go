package version

import "fmt"

type Version uint64

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

type ConflictError struct {
	Expected Version
	Actual   Version
}

func (err ConflictError) Error() string {
	return fmt.Sprintf(
		"version.Check: conflict detected; expected stream version: %d, actual: %d",
		err.Expected,
		err.Actual,
	)
}
