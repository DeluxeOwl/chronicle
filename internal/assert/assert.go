package assert

import (
	"fmt"
)

func That(truth bool, format string, a ...any) {
	if !truth {
		panic(fmt.Sprintf(format, a...))
	}
}

func Never(format string, a ...any) {
	That(false, format, a...)
}
