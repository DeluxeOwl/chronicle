package person

import (
	"context"

	"github.com/DeluxeOwl/chronicle/aggregate"
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

func (p *Person) ToSnapshot(person *Person) (*PersonSnapshot, error) {
	return &PersonSnapshot{
		// Important: store the version
		SnapshotVersion: person.Version(),
		SnapshotID:      person.id,
		Name:            person.name,
		Age:             person.age,
	}, nil
}

func (p *Person) FromSnapshot(snapshot *PersonSnapshot) (*Person, error) {
	return &Person{
		id:   snapshot.SnapshotID,
		name: snapshot.Name,
		age:  snapshot.Age,
	}, nil
}

func CustomSnapshot(
	ctx context.Context,
	root *Person,
	previousVersion, newVersion version.Version,
	committedEvents aggregate.CommitedEvents[PersonEvent],
) bool {
	for evt := range committedEvents.All() {
		// This is exhaustive.
		switch evt.(type) {
		case *personAgedOneYear:
			return true
		case *personWasBorn:
			continue
		default:
			continue
		}
	}

	return false
}
