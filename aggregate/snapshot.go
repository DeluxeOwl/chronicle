package aggregate

type Snapshotter interface {
	WithSnapshot() WithSnapshot
}

func FromSnapshot[TheReceiverOfThisMethod, PointerToYourSnapshotType any](
	from func(PointerToYourSnapshotType) TheReceiverOfThisMethod,
) *snapshotWithFrom[TheReceiverOfThisMethod, PointerToYourSnapshotType] {
	return &snapshotWithFrom[TheReceiverOfThisMethod, PointerToYourSnapshotType]{
		from: from,
	}
}

type snapshotWithFrom[TA, TS any] struct {
	from func(TS) TA
}

func (s *snapshotWithFrom[TA, TS]) To(to func() TS) *snapshotWithTo[TA, TS] {
	return &snapshotWithTo[TA, TS]{
		from: s.from,
		to:   to,
	}
}

type snapshotWithTo[TA, TS any] struct {
	from func(TS) TA
	to   func() TS
}

type WithSnapshot interface {
	aggregateToSnapshot() any
	aggregateFromSnapshot(any) any
}

func (s snapshotWithTo[TA, TS]) aggregateToSnapshot() any {
	return s.to()
}

func (s snapshotWithTo[TA, TS]) aggregateFromSnapshot(val any) any {
	asTS, ok := val.(TS)
	if !ok {
		panic("this shouldn't happen")
	}
	return s.from(asTS)
}
