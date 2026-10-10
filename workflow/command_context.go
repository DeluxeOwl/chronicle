package workflow

import "context"

// Commands can target a stream other than the invoking worker's stream. Keep
// cancellation and deadlines, but do not apply that worker's checkpoint receipt
// to the target. Its optimistic version check still protects concurrent writes.
func workflowCommandContext(ctx context.Context, target InstanceID) context.Context {
	lease, ok := ctx.Value(workerLeaseContextKey{}).(*workerLease)
	if ok && lease != nil && lease.task != nil && lease.task.InstanceID == target {
		return ctx // A self-targeted command is still subject to receipt fencing.
	}
	return context.WithValue(ctx, workerLeaseContextKey{}, struct{}{})
}
