package assert

import (
	"log"
)

func That(truth bool, format string, a ...any) {
	if !truth {
		log.Fatalf(format, a...)
	}
}

func Never(format string, a ...any) {
	That(false, format, a...)
}
