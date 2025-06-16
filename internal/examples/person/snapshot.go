package person

import (
	"context"

	"github.com/DeluxeOwl/chronicle/aggregate"
	"github.com/DeluxeOwl/chronicle/event"
	"github.com/DeluxeOwl/chronicle/version"
)

var _ aggregate.Snapshotter[PersonID, PersonEvent, *Person, *PersonSnapshot] = (*Person)(nil)

type PersonSnapshot struct {
	SnapshotVersion version.Version `json:"snapshotVersion"`
	SnapshotID      PersonID        `json:"snapshotID"`
	Name            string          `json:"name"`
	Age             int             `json:"age"`
}

func NewSnapshot() *PersonSnapshot {
	return new(PersonSnapshot)
}

func (ps *PersonSnapshot) Version() version.Version {
	return ps.SnapshotVersion
}

func (ps *PersonSnapshot) ID() PersonID {
	return ps.SnapshotID
}

func (p *Person) ToSnapshot(person *Person) *PersonSnapshot {
	return &PersonSnapshot{
		// Important: store the version
		SnapshotVersion: person.Version(),
		SnapshotID:      person.id,
		Name:            person.name,
		Age:             person.age,
	}
}

func (p *Person) FromSnapshot(snapshot *PersonSnapshot) *Person {
	return &Person{
		id:   snapshot.SnapshotID,
		name: snapshot.Name,
		age:  snapshot.Age,
	}
}

func CustomSnapshot(ctx context.Context, root *Person, previousVersion, newVersion version.Version, committedEvents event.CommitedEvents[PersonEvent]) bool {
	for evt := range committedEvents.All() {
		//nolint:gochecksumtype // This is exhaustive but we don't need it here.
		switch evt.Unwrap().(type) {
		case *personAgedOneYear:
			return true
		default:
			continue
		}
	}

	return false
}
