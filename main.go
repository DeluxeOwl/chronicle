package main

import (
	"context"

	"github.com/DeluxeOwl/eventuallynow/aggregate"
	"github.com/DeluxeOwl/eventuallynow/event"
	memoryadapter "github.com/DeluxeOwl/eventuallynow/event/adapter"
	"github.com/DeluxeOwl/eventuallynow/person"
	"github.com/DeluxeOwl/eventuallynow/serde"
	"github.com/DeluxeOwl/eventuallynow/version"
	"github.com/sanity-io/litter"
)

func printSerializedAndDeserialized(p *person.Person) {
	personToSnapshot := person.NewPersonSnapshotSerde()
	snapshotToJSON := serde.NewJSON(func() *person.PersonSnapshot { return new(person.PersonSnapshot) })

	serde := serde.Chain(
		personToSnapshot,
		snapshotToJSON,
	)

	serialized, err := serde.Serialize(p)
	if err != nil {
		panic(err)
	}

	litter.Dump(serialized)

	deserialized, err := serde.Deserialize(serialized)
	if err != nil {
		panic(err)
	}

	litter.Dump(deserialized)
}

func main() {
	ctx := context.Background()

	litter.Config.HidePrivateFields = false

	memoryStore := memoryadapter.NewMemoryStore()
	// Typically you'd pass the serde config to the event sourced repository
	repo := aggregate.NewEventSourcedRepository(memoryStore, person.NewEmpty)

	id := person.PersonID("some-generated-id")
	p, err := person.New(id.String(), "Johnny")
	if err != nil {
		panic(err)
	}

	for range 2 {
		err = p.Age()
		if err != nil {
			panic(err)
		}
	}

	printSerializedAndDeserialized(p)

	litter.Dump(p)

	err = repo.Save(ctx, p)
	if err != nil {
		panic(err)
	}

	p, err = repo.Get(ctx, id)
	if err != nil {
		panic(err)
	}

	litter.Dump(p)

	for evt := range memoryStore.ReadEvents(ctx, event.LogID(id.String()), version.SelectFromBeginning) {
		litter.Dump(evt)
	}
}
