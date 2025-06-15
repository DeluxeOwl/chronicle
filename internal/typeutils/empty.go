package typeutils

func Zero[T any]() T {
	var empty T
	return empty
}
