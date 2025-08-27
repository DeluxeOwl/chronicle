package shared

import (
	"time"

	"github.com/DeluxeOwl/chronicle/internal/assert"
	"github.com/DeluxeOwl/chronicle/pkg/timeutils"
	"github.com/gofrs/uuid/v5"
)

type EventMeta interface {
	isEventMeta()
}

type EventMetadata struct {
	EventID   string    `json:"eventID"`
	OccuredAt time.Time `json:"occuredAt"`
}

func (em *EventMetadata) isEventMeta() {}

// We're getting the timestamp from the id
// An easier way would be to return the OccuredAt field
func (em *EventMetadata) Time() time.Time {
	id := uuid.Must(uuid.FromString(em.EventID))
	timestamp, err := uuid.TimestampFromV7(id)
	if err != nil {
		assert.Never("error getting timestamp from id: %v", err)
	}

	time, err := timestamp.Time()
	if err != nil {
		assert.Never("error getting time from timestamp: %v", err)
	}

	return time
}

func NewEventMetaGenerator(provider timeutils.TimeProvider) *EventMetaGenerator {
	return &EventMetaGenerator{
		gen: func() EventMetadata {
			now := provider.Now()

			return EventMetadata{
				// The uuidv7 contains the timestamp
				EventID: uuid.Must(uuid.NewV7AtTime(now)).String(),
				// Or just same a simple timestamp
				OccuredAt: now,
			}
		},
	}
}

type EventMetaGenerator struct {
	gen func() EventMetadata
}

func (gen *EventMetaGenerator) NewEventMeta() EventMetadata {
	return gen.gen()
}
