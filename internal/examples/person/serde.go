package person

import (
	"github.com/DeluxeOwl/eventuallynow/serde"
	"github.com/DeluxeOwl/zerrors"
)

type PersonSnapshot struct {
	ID   PersonID `json:"id"`
	Name string   `json:"name"`
	Age  int      `json:"age"`
}

var Serde = NewPersonSnapshotSerde()

type personSnapshotSerde struct{}

func NewPersonSnapshotSerde() serde.Serde[*Person, *PersonSnapshot] {
	return &personSnapshotSerde{}
}

func (s *personSnapshotSerde) Serialize(p *Person) (*PersonSnapshot, error) {
	if p == nil {
		return nil, zerrors.New(ErrNilPersonSerialize)
	}

	return &PersonSnapshot{
		ID:   p.id,
		Name: p.name,
		Age:  p.age,
	}, nil
}

func (s *personSnapshotSerde) Deserialize(snap *PersonSnapshot) (*Person, error) {
	personInstance := NewEmpty()

	personInstance.id = snap.ID
	personInstance.name = snap.Name
	personInstance.age = snap.Age

	return personInstance, nil
}
