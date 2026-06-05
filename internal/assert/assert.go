package assert

import (
	"fmt"
)

func Thatf(truth bool, format string, a ...any) {
	if !truth {
		panic(fmt.Sprintf(format, a...))
	}
}

func Neverf(format string, a ...any) {
	Thatf(false, format, a...)
}
