# TODO

- Note that index creating is up to the user
  - e.g. on event name `CREATE INDEX IF NOT EXISTS idx_chronicle_events_event_name ON chronicle_events (event_name);`
  - e.g. on jsonb data
- Namespaces in tables like https://github.com/earendil-works/absurd/blob/main/sql/absurd.sql (schemas?)
- Add an example e2e with CQRS, postgres and NATS. Something users can consider "prod ready". 
- How would you provide this to Go (and not only Go) developers "as a service"? Maybe by having a nice UI interface.
- First class support for sagas/workflows, but it can be a separate package.
	- This also gets into the territory of "jobs" (workflows are jobs on steroids)
- More testing in general
- CI/CD
- Observability: especially logs and otel.
- An interesting thing would be to implement differential dataflow or CRDTs. Would help with joins.
- Optional filters for ReadEvents - the implementation can decide to use native filters (e.g. sql filters) or normal filters
- refactor to allow injecting functions instead, e.g. in workflow pass as params the Save(...) functions for the repo, registry etc
  - Maybe even for the rest
- fumapress docs and https://github.com/alialaee/logfile backend
