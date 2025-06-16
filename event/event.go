package event

type Any interface {
	EventName() string
}

func AnyToConcrete[E Any](event Any) (E, bool) {
	concrete, ok := event.(E)
	if !ok {
		var empty E
		return empty, false
	}

	return concrete, true
}
