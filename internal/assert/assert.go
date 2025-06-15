package assert

import (
	"log"
)

func That(truth bool, format string, a ...any) {
	if !truth {
		log.Fatalf(format, a...)
	}
}
