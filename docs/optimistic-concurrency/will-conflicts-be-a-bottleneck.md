# Will conflicts be a bottleneck?

A common question is: "Will my app constantly handle conflict errors and retries? Won't that be a bottleneck?".

With well designed aggregates, the answer is **no**. Conflicts are the exception, not the rule.

The most important thing to remember is that version conflicts happen **per aggregate**. 

A `version.ConflictError` for `AccountID("acc-123")` has absolutely no effect on a concurrent operation for `AccountID("acc-456")`. The aggregate itself defines the consistency boundary.

Because aggregates are typically designed to represent a single, cohesive entity that is most often manipulated by a single user at a time (like _your_ shopping cart, or _your_ user profile), the opportunity for conflicts is naturally low. 

This fine-grained concurrency model is what allows event-sourced systems to achieve high throughput, as the vast majority of operations on different aggregates can proceed in parallel without any contention.
