package registry

import (
	"sync"

	"github.com/DeluxeOwl/eventuallynow/event"
)

// TODO: what if not global, but injected

var (
	eventFactories = make(map[string]func() event.EventAny)
	registryMu     sync.RWMutex // Mutex to protect eventFactories
)

func Register(eventName string, kindPrototype event.EventAny) {
	registryMu.Lock()
	defer registryMu.Unlock()

	eventFactories[eventName] = func() event.EventAny { return kindPrototype }
}

func GetFactory(eventName string) (func() event.EventAny, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	factory, ok := eventFactories[eventName]
	return factory, ok
}
