//go:build (!unix && !windows) || aix

package migrations

import (
	"errors"
	"os"
)

func trySQLiteMigrationLock(_ *os.File) (bool, error) {
	return false, errors.New("SQLite migration file locking is unsupported on this platform")
}
