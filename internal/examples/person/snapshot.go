package person

import (
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
