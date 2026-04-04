# Benchmarks

```sh
task bench-eventlogs

task: [bench-eventlogs] go test ./eventlog -bench=. -benchmem -run=^$ ./...
goos: darwin
goarch: arm64
pkg: github.com/DeluxeOwl/chronicle/eventlog
cpu: Apple M3 Pro
```

Appending 5 events for each goroutine (100 multiple goroutines):
```
BenchmarkAppendEventsConcurrent/memory-12                           5812            181562 ns/op          154978 B/op       1198 allocs/op
BenchmarkAppendEventsConcurrent/sqlite-12                              2        1144747208 ns/op          687896 B/op      14906 allocs/op
BenchmarkAppendEventsConcurrent/postgres-12                           10         105682996 ns/op         3750732 B/op      39086 allocs/op
```

Appending events (single threaded):
```
BenchmarkAppendEvents/BatchSize1/memory-12       1657644               683.4 ns/op           469 B/op          7 allocs/op
BenchmarkAppendEvents/BatchSize1/sqlite-12          4828            276286 ns/op            1757 B/op         48 allocs/op
BenchmarkAppendEvents/BatchSize1/postgres-12                1161           1026385 ns/op            2199 B/op         46 allocs/op

BenchmarkAppendEvents/BatchSize10/memory-12              1000000              1539 ns/op            2616 B/op         16 allocs/op
BenchmarkAppendEvents/BatchSize10/sqlite-12                 3483            349211 ns/op            7894 B/op        192 allocs/op
BenchmarkAppendEvents/BatchSize10/postgres-12                477           2662430 ns/op            7031 B/op        154 allocs/op

BenchmarkAppendEvents/BatchSize100/memory-12              188637              6477 ns/op           23935 B/op        106 allocs/op
BenchmarkAppendEvents/BatchSize100/sqlite-12                1497            745225 ns/op           69269 B/op       1632 allocs/op
BenchmarkAppendEvents/BatchSize100/postgres-12                51          22129525 ns/op           55418 B/op       1234 allocs/op
```

Reading events:
```
BenchmarkReadEvents/StreamLength100/memory-12             312548              3728 ns/op            8760 B/op        115 allocs/op
BenchmarkReadEvents/StreamLength100/sqlite-12               6889            179535 ns/op           42867 B/op       1149 allocs/op
BenchmarkReadEvents/StreamLength100/postgres-12             3472            368551 ns/op           26991 B/op        948 allocs/op
BenchmarkReadEvents/StreamLength1000/memory-12             33950             31921 ns/op           81722 B/op       1018 allocs/op
BenchmarkReadEvents/StreamLength1000/sqlite-12               738           1633509 ns/op          424197 B/op      12697 allocs/op
BenchmarkReadEvents/StreamLength1000/postgres-12            1658            746168 ns/op          264324 B/op      10696 allocs/op
```

Reading the global event log:
```
BenchmarkReadAllEvents/Total1000_Events/memory-12           7255            146797 ns/op          302663 B/op       1026 allocs/op
BenchmarkReadAllEvents/Total1000_Events/sqlite-12            565           2125557 ns/op          488207 B/op      16710 allocs/op
BenchmarkReadAllEvents/Total1000_Events/postgres-12         1366            921196 ns/op          328453 B/op      14711 allocs/op
BenchmarkReadAllEvents/Total10000_Events/memory-12           753           1399182 ns/op         3715825 B/op      10036 allocs/op
BenchmarkReadAllEvents/Total10000_Events/sqlite-12            57          21337020 ns/op         5088790 B/op     186170 allocs/op
BenchmarkReadAllEvents/Total10000_Events/postgres-12         198           6017864 ns/op         3489125 B/op     166171 allocs/op
```
