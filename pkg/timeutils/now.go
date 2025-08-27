package timeutils

import "time"

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
