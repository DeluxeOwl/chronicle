package timeutils

import (
	"sync"
	"time"
)

//go:generate go run github.com/matryer/moq@latest -pkg timeutils -skip-ensure -rm -out now_mock.go . TimeProvider
type TimeProvider interface {
	Now() time.Time
}

var RealTimeProvider = sync.OnceValue(func() *realTimeProvider {
	return &realTimeProvider{}
})

type realTimeProvider struct{}

func (r *realTimeProvider) Now() time.Time {
	return time.Now()
}
