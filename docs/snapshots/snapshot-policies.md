# Snapshot policies

If you type `aggregate.SnapPolicyFor[*account.Account]().` you will get autocomplete for various snapshot policies:
- `EveryNEvents(n)`: Takes a snapshot every n times the aggregate is saved.
- `AfterCommit()`: Takes a snapshot after every successful save.
- `OnEvents(eventNames...)`: Takes a snapshot only if one or more of the specified event types were part of the save.
- `AllOf(policies...)`: A composite policy that triggers only if all of its child policies match.
- `AnyOf(policies...)`: A composite policy that triggers if any of its child policies match.     

Or a `Custom(...)` policy, which gives you complete control by allowing you to provide your own function. This function receives the aggregate's state, its versions, and the list of committed events, so you can decide when a snapshot should be taken:
```go
aggregate.SnapPolicyFor[*account.Account]().Custom(
			func(ctx context.Context, root *account.Account, previousVersion, newVersion version.Version, committedEvents aggregate.CommittedEvents[account.AccountEvent]) bool {
				return true // always snapshot
			}),
```

An example in [account_snapshot.go](../examples/internal/account/account_snapshot.go):
```go
func CustomSnapshot(
	ctx context.Context,
	root *Account,
	_, _ version.Version,
	_ aggregate.CommittedEvents[AccountEvent],
) bool {
	return root.balance%250 == 0 // Only snapshot if the balance is a multiple of 250
}
```

**Important**: Regardless of the snapshot policy chosen, saving the snapshot happens after the new events are successfully committed to the event log. This means the two operations are not atomic. It is possible for the events to be saved successfully but for the subsequent snapshot save to fail. 

By default, an error during a snapshot save will be returned by the Save method. You can customize this behavior with the `aggregate.OnSnapshotError` option, allowing you to log the error and continue, or ignore it entirely.

Since snapshots are purely a performance optimization, ignoring a failed snapshot save can be a safe and reasonable strategy. The aggregate can always be rebuilt from the event log, which remains the single source of truth.
