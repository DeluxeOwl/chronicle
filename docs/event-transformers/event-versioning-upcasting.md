# Event versioning and upcasting

You can find this example in [aggregate_upcasting_test.go](../aggregate/aggregate_upcasting_test.go).

As your application evolves, so will your events. You might need to rename fields, change data types, or split one event into several more granular ones. Since the event log is immutable, you can't go back and change historical events. This is where event upcasting comes in. 

Upcasting is the process of transforming an older version of an event into its newer equivalent, on-the-fly, as it's read from the event store. `chronicle` handles this using the same `event.Transformer` interface.

Imagine we started with a single event to update a person's name and age: 
```go
// V1 event - written in the past
type nameAndAgeSetV1 struct {
    Name string `json:"name"`
    Age  int    `json:"age"`
}
func (*nameAndAgeSetV1) EventName() string { return "person/name_and_age_set_v1" }
func (*nameAndAgeSetV1) isPersonEvent()    {}
```

Later, we decide it's better to have separate events for changing the name and age: 
```go
// V2 events - what our new code uses
type nameSetV2 struct {
    Name string `json:"name"`
}
func (*nameSetV2) EventName() string { return "person/name_set_v2" }
func (*nameSetV2) isPersonEvent()    {}

type ageSetV2 struct {
    Age int `json:"age"`
}
func (*ageSetV2) EventName() string { return "person/age_set_v2" }
func (*ageSetV2) isPersonEvent()    {}
```

How do we load an aggregate that has old `nameAndAgeSetV1` events in its history? We create an "upcaster" transformer.
```go
type upcasterV1toV2 struct{}

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
            // Not a V1 event, pass it through unchanged
            newEvents = append(newEvents, e)
        }
    }
    return newEvents, nil
}

// Write is a pass-through; new code doesn't produce V1 events.
func (u *upcasterV1toV2) TransformForWrite(
    _ context.Context,
    events []PersonEvent,
) ([]PersonEvent, error) {
    return events, nil
}
```

When this transformer is added to the repository, here's what happens during a `repo.Get()` call: 
1. The repository reads the raw event records from the event log (e.g., `personWasBorn` at version 1, `nameAndAgeSetV1` at version 2).
2. The event data is decoded into the Go structs. Your `EventFuncs` must include constructors for both old and new event types so they can be decoded.
3. The `upcasterV1toV2.TransformForRead` method is called.
4. It sees the `nameAndAgeSetV1` event and replaces it in memory with a `nameSetV2` and an `ageSetV2` event.
5. The aggregate's `Apply` method is then called with the transformed list of events. The aggregate's state is built correctly using the new event types.

> [!IMPORTANT]
> The aggregate's version always reflects the version of the last **persisted** event in the log. Even if an upcaster creates more events in memory, the version number remains consistent with the source of truth. In the example above, the aggregate's final version would be `2`, corresponding to the `nameAndAgeSetV1` event, not `3`.

## Merging events for compaction

Transformers can also work in the other direction: merging multiple events into a single, more compact event before writing them to the log. This can be a useful optimization to reduce the number of records for high-frequency events. 

For example, imagine we have a `personAgedOneYear` event that gets recorded frequently. We can create a transformer to batch these up.
```go
// Merges multiple personAgedOneYear events on write
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
        mergedEvent := &multipleYearsAged{Years: totalYears}
        return append(otherEvents, mergedEvent), nil
    }
    return events, nil
}

// Splits the merged event back up on read
func (a *ageBatchingTransformer) TransformForRead(
    _ context.Context,
    events []PersonEvent,
) ([]PersonEvent, error) {
    newEvents := make([]PersonEvent, 0, len(events))
    for _, e := range events {
        if merged, ok := e.(*multipleYearsAged); ok {
            for range merged.Years {
                newEvents = append(newEvents, &personAgedOneYear{})
            }
        } else {
            newEvents = append(newEvents, e)
        }
    }
    return newEvents, nil
}
```

When you save an aggregate that has recorded five `personAgedOneYear` events, the `TransformForWrite` hook will replace them with a single `multipleYearsAged{Years: 5}` event. This single event is what gets written to the log. 

When you later load the aggregate, `TransformForRead` does the reverse, ensuring that your aggregate's `Apply` method sees the five individual `personAgedOneYear` events it expects, keeping your domain logic clean and unaware of this persistence optimization. 
