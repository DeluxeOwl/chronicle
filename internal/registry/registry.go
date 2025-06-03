package registry

var eventFactories = make(map[string]func() any)

// TODO: concurrency safe
// TODO: naming

func Register(eventName string, kind any) {
	eventFactories[eventName] = func() any { return kind }
}
