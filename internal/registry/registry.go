package registry

import "sync"

var (
	eventFactories = make(map[string]func() any)
	registryMu     sync.RWMutex // Mutex to protect eventFactories
)

func Register(eventName string, kindPrototype any) {
	registryMu.Lock()
	defer registryMu.Unlock()

	eventFactories[eventName] = func() any { return kindPrototype }
}

func GetFactory(eventName string) (func() any, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	factory, ok := eventFactories[eventName]
	return factory, ok
}
