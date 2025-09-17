package aggregate_test

import (
	"context"
	"testing"

	"github.com/DeluxeOwl/chronicle"
	"github.com/DeluxeOwl/chronicle/aggregate"
	"github.com/DeluxeOwl/chronicle/event"
	"github.com/DeluxeOwl/chronicle/eventlog"
	"github.com/DeluxeOwl/chronicle/serde"
	"github.com/DeluxeOwl/chronicle/version"
	"github.com/stretchr/testify/require"
)

// upcasterV1toV2 transforms an old `nameAndAgeSetV1` event into two new events.
type upcasterV1toV2 struct{}

var _ event.Transformer[PersonEvent] = (*upcasterV1toV2)(nil)

// TransformForWrite is a pass-through; we don't write V1 events anymore.
func (u *upcasterV1toV2) TransformForWrite(
	_ context.Context,
	events []PersonEvent,
) ([]PersonEvent, error) {
	return events, nil
}

// TransformForRead performs the up-casting from V1 to V2.
func (u *upcasterV1toV2) TransformForRead(
	_ context.Context,
	events []PersonEvent,
) ([]PersonEvent, error) {
	newEvents := make([]PersonEvent, 0, len(events))
	for _, e := range events {
		if oldEvent, ok := e.(*nameAndAgeSetV1); ok {
			// A V1 event is found, split it into two V2 events
			newEvents = append(newEvents, &nameSetV2{Name: oldEvent.Name})
			newEvents = append(newEvents, &ageSetV2{Age: oldEvent.Age})
		} else {
			// This is not a V1 event, pass it through unchanged
			newEvents = append(newEvents, e)
		}
	}
	return newEvents, nil
}

func Test_EventTransformation_UpcastingSplitsEvent(t *testing.T) {
	ctx := t.Context()
	personID := PersonID("person-upcast")
	serializer := serde.NewJSONBinary()
	memlog := eventlog.NewMemory()

	// 1. Simulate old data being in the event log.
	// We start with a `personWasBorn` event.
	bornEvt := &personWasBorn{ID: personID, BornName: "Doc Brown"}
	rawBorn, err := serializer.SerializeBinary(bornEvt)
	require.NoError(t, err)

	// Then, we manually add a raw `nameAndAgeSetV1` event, as if it was written long ago.
	v1Evt := &nameAndAgeSetV1{Name: "Marty McFly", Age: 17}
	rawV1, err := serializer.SerializeBinary(v1Evt)
	require.NoError(t, err)

	// Append both to the log. The aggregate will be at version 2.
	_, err = memlog.AppendEvents(ctx, event.LogID(personID), version.CheckExact(0), []event.Raw{
		event.NewRaw(bornEvt.EventName(), rawBorn),
		event.NewRaw(v1Evt.EventName(), rawV1),
	})
	require.NoError(t, err)

	// 2. Set up the repository with the up-casting transformer.
	upcaster := &upcasterV1toV2{}
	registry := chronicle.NewEventRegistry[PersonEvent]()
	err = registry.RegisterEvents(NewEmpty()) // Register all person events
	require.NoError(t, err)

	// 3. Load the aggregate from the store.
	loadedPerson := NewEmpty()
	err = aggregate.ReadAndLoadFromStore(
		ctx,
		loadedPerson,
		memlog,
		registry,
		serializer,
		[]event.Transformer[PersonEvent]{upcaster},
		personID,
		version.SelectFromBeginning,
	)
	require.NoError(t, err)

	// The state should reflect the *transformed* V2 events.
	require.Equal(t, "Marty McFly", loadedPerson.name)
	require.Equal(t, 17, loadedPerson.age)
	require.Equal(t, personID, loadedPerson.id)

	// The version must match the last *persisted* record's version,
	// even though more events were applied in memory due to transformation.
	require.EqualValues(t, 2, loadedPerson.Version())
}

// ageBatchingTransformer merges multiple `personAgedOneYear` events into one.
type ageBatchingTransformer struct{}

var _ event.Transformer[PersonEvent] = (*ageBatchingTransformer)(nil)

// TransformForWrite finds all `personAgedOneYear` events and merges them.
func (a *ageBatchingTransformer) TransformForWrite(
	_ context.Context,
	events []PersonEvent,
) ([]PersonEvent, error) {
	totalYears := 0
	otherEvents := make([]PersonEvent, 0)

	for _, e := range events {
		if _, ok := e.(*personAgedOneYear); ok {
			totalYears++
		} else {
			otherEvents = append(otherEvents, e)
		}
	}

	if totalYears > 0 {
		// Add the single merged event to the list of other events.
		mergedEvent := &multipleYearsAged{Years: totalYears}
		return append(otherEvents, mergedEvent), nil
	}

	return events, nil
}

// TransformForRead splits a merged event back into individual `personAgedOneYear` events.
func (a *ageBatchingTransformer) TransformForRead(
	_ context.Context,
	events []PersonEvent,
) ([]PersonEvent, error) {
	newEvents := make([]PersonEvent, 0, len(events))
	for _, e := range events {
		if merged, ok := e.(*multipleYearsAged); ok {
			// Split the merged event back into individual year events
			for range merged.Years {
				newEvents = append(newEvents, &personAgedOneYear{})
			}
		} else {
			newEvents = append(newEvents, e)
		}
	}
	return newEvents, nil
}

func Test_EventTransformation_MergingBatchesEvents(t *testing.T) {
	ctx := t.Context()
	memlog := eventlog.NewMemory()

	// 1. Set up repository with the merging transformer.
	batcher := &ageBatchingTransformer{}
	repo, err := chronicle.NewEventSourcedRepository(
		memlog,
		NewEmpty,
		[]event.Transformer[PersonEvent]{batcher},
	)
	require.NoError(t, err)

	// 2. Create an aggregate and record multiple events that can be merged.
	p, err := New(PersonID("person-batch"), "Biff Tannen")
	require.NoError(t, err)
	require.EqualValues(t, 1, p.Version()) // Version 1 after `personWasBorn`

	// Record 5 `personAgedOneYear` events.
	for range 5 {
		err = p.Age()
		require.NoError(t, err)
	}
	require.EqualValues(t, 6, p.Version()) // In-memory version is 1 + 5 = 6

	// 3. Save the aggregate. The transformer should merge the 5 age events.
	_, _, err = repo.Save(ctx, p)
	require.NoError(t, err)

	// 4. Assert that only one merged event was actually persisted.
	records, err := memlog.ReadEvents(ctx, event.LogID(p.ID()), version.SelectFromBeginning).
		Collect()
	require.NoError(t, err)

	// We expect 2 records: `personWasBorn` and the single `multipleYearsAged`.
	require.Len(t, records, 2)
	require.Equal(t, new(personWasBorn).EventName(), records[0].EventName())
	require.Equal(t, new(multipleYearsAged).EventName(), records[1].EventName())
	require.EqualValues(t, 2, records[1].Version(), "Persisted version should be 2")

	// 5. Load the aggregate from the store to test the read path.
	loadedPerson, err := repo.Get(ctx, p.ID())
	require.NoError(t, err)

	// Assert the state is correct (meaning the merged event was correctly split back on read).
	require.Equal(t, 5, loadedPerson.age) // initial age is 0
	require.Equal(t, "Biff Tannen", loadedPerson.name)

	// Assert the version reflects the persisted reality.
	require.EqualValues(t, 2, loadedPerson.Version())
}
