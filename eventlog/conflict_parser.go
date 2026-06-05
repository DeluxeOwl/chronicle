package eventlog

import (
	"strconv"
	"strings"

	"github.com/DeluxeOwl/chronicle/version"
)

const conflictErrorPrefix = "_chronicle_version_conflict: "

// parseConflictError attempts to parse a driver-agnostic version conflict error.
// It works by looking for a specific substring ("_chronicle_version_conflict: ")
// in the error message, which both the Postgres and SQLite triggers are configured to produce.
// If the substring is found, it parses the number that immediately follows it.
//
// It returns the parsed actual version and 'true' if successful.
// Otherwise, it returns version.Zero and 'false'.
func parseConflictError(err error) (version.Version, bool) {
	if err == nil {
		return version.Zero, false
	}

	errMsg := err.Error()

	// Find the start of our unique error message prefix.
	// This is more robust than SplitN as it ignores driver prefixes.
	_, after, ok := strings.Cut(errMsg, conflictErrorPrefix)
	if !ok {
		// Our special error message is not in the string.
		return version.Zero, false
	}

	// The version number starts right after our prefix.
	payload := after

	// Extract the numeric part, stopping at the first non-digit.
	// This handles cases where there might be trailing text or parentheses.
	var versionStr strings.Builder
	for _, r := range payload {
		if r >= '0' && r <= '9' {
			versionStr.WriteRune(r)
		} else {
			break
		}
	}

	if versionStr.String() == "" {
		return version.Zero, false
	}

	actual, parseErr := strconv.ParseUint(versionStr.String(), 10, 64)
	if parseErr != nil {
		// We found the prefix but the following text wasn't a valid number.
		return version.Zero, false
	}

	return version.Version(actual), true
}
