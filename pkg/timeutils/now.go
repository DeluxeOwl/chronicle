package timeutils

import "time"

//go:generate go run github.com/matryer/moq@latest -pkg timeutils -skip-ensure -rm -out now_mock.go . TimeProvider
type TimeProvider interface {
	Now() time.Time
}

type RealTimeProvider struct{}

func (r *RealTimeProvider) Now() time.Time {
	return time.Now()
}

func NewRealTimeProvider() *RealTimeProvider {
	return &RealTimeProvider{}
}
