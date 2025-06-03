package registry

var eventFactories = make(map[string]func() any)

type EventRegistrar struct{}

// TODO: concurrency safe
// TODO: naming
var Registrar = &EventRegistrar{}

func (er *EventRegistrar) Register(eventName string, kind any) {
	eventFactories[eventName] = func() any { return kind }
}
