package person

import (
	"github.com/DeluxeOwl/eventuallynow/serde"
	"github.com/pkg/errors"
)

type PersonSnapshot struct {
	ID   PersonID `json:"id"`
	Name string   `json:"name"`
	Age  int      `json:"age"`
}

type personSnapshotSerde struct{}

func NewPersonSnapshotSerde() serde.Serde[*Person, *PersonSnapshot] {
	return &personSnapshotSerde{}
}

func (s *personSnapshotSerde) Serialize(p *Person) (*PersonSnapshot, error) {
	if p == nil {
		return nil, errors.New("person.Serialize: cannot serialize nil Person to PersonSnapshot")
	}

	return &PersonSnapshot{
		ID:   p.id,
		Name: p.name,
		Age:  p.age,
	}, nil
}

func (s *personSnapshotSerde) Deserialize(snap *PersonSnapshot) (*Person, error) {
	personInstance := Type.New()

	personInstance.id = snap.ID
	personInstance.name = snap.Name
	personInstance.age = snap.Age

	return personInstance, nil
}
