package workflow

import (
	"fmt"
	"time"
)

// Heartbeat extends the lease for the current workflow execution.
// Call this from long-running steps to signal that the worker is still alive
// and prevent the task queue from re-assigning the task to another worker.
//
// The lease duration specifies how much additional time this worker claims
// for the task. For persistent queues with lease-based claiming, this
// prevents the task from being picked up by another worker if the step
// takes longer than the original claim timeout.
//
// For the in-memory queue, this is a no-op.
//
// Example:
//
//	result, err := workflow.Step(wctx, func(ctx context.Context) (string, error) {
//	    for i := range 100 {
//	        // Long-running work
//	        processChunk(i)
//	        // Keep the lease alive
//	        if err := workflow.Heartbeat(wctx, 5*time.Minute); err != nil {
//	            return "", err
//	        }
//	    }
//	    return "done", nil
//	})
func Heartbeat(wctx *Context, d time.Duration) error {
	if err := wctx.runner.queue.ExtendLease(wctx.ctx, wctx.instanceID, d); err != nil {
		return fmt.Errorf("heartbeat: %w", err)
	}
	return nil
}
