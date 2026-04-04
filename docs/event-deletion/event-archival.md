# Event archival

Chronicle doesn't provide a way to handle event archival since it can be done in lots of ways.  

Conceptually, it's similar to deletion, but instead of losing the events, they are instead moved to a different storage (cold storage, e.g. aws glacier).

At a high level, it might look like this:
- The aggregate generates a `HistoryArchived` event with the state, last archived version, location of archive etc.
- The "archivist" process: a background job, separate process etc.
  - identifies aggregates that meet an archival policy (e.g., "more than 2000 events and the last 1000 are older than 1 year")
  - reads the old events (e.g. version 1 to 1000), bundles these events (e.g. into json format) and uploads them to cold storage (e.g. s3)
  - deletes events up to `HistoryArchived`

Then, you'd have to implement a custom archive-aware repository or event log, which scans for the history archived event, and in special cases, pulls that history (e.g. for audit purposes).
