# Testing

The event logs are tested all at once (since they satisfy the same interface) in [./eventlog/eventlog_test.go](../eventlog/eventlog_test.go).

A sample aggregate is used for testing in [./aggregate/aggregate_test.go](../aggregate/aggregate_test.go).

This framework uses `go:generate` with the [`moq`](https://github.com/matryer/moq) library to create mock implementations of the core interfaces. You can see this in files like [./aggregate/repository.go](../aggregate/repository.go).

```go
// in aggregate/repository.go
//go:generate go run github.com/matryer/moq@latest -pkg aggregate_test -skip-ensure -rm -out repository_mock_test.go . Repository
```
