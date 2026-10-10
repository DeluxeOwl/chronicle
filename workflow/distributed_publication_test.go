//go:build !windows

package workflow_test

import (
	"testing"
	"time"

	"github.com/DeluxeOwl/chronicle/workflow"
	"github.com/stretchr/testify/require"
)

func distributedPublicationFailureRollsBackPayloadAndWakeup(
	t *testing.T,
	backend, database string,
) {
	db, wf, inspector := newDistributedFixture(t, backend, database)
	id, err := wf.Start(t.Context(), &distributedParams{Mode: distributedWait, Sleep: 0})
	require.NoError(t, err)
	worker := startDistributedProcess(t, childWorker(backend, database, "waiter", false, true))
	worker.waitMessage(t, "ready")
	require.Eventually(t, func() bool {
		detail, err := inspector.Get(t.Context(), id)
		return err == nil && detail.Summary.Status == workflow.StatusWaiting
	}, 8*time.Second, 10*time.Millisecond)
	worker.kill()
	var reject, repair string
	if backend == distributedPostgres {
		reject = `CREATE FUNCTION reject_wakeup() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected wakeup failure'; END; $$;
			CREATE TRIGGER reject_wakeup BEFORE INSERT ON workflow_ready_tasks FOR EACH ROW EXECUTE FUNCTION reject_wakeup();`
		repair = `DROP TRIGGER reject_wakeup ON workflow_ready_tasks; DROP FUNCTION reject_wakeup();`
	} else {
		reject = `CREATE TRIGGER reject_wakeup BEFORE INSERT ON workflow_ready_tasks BEGIN SELECT RAISE(ABORT,'injected wakeup failure'); END;`
		repair = `DROP TRIGGER reject_wakeup;`
	}
	_, err = db.ExecContext(t.Context(), reject)
	require.NoError(t, err)
	runner, err := newDistributedRunner(t.Context(), db, backend)
	require.NoError(t, err)
	require.ErrorIs(
		t,
		workflow.PublishEvent(t.Context(), runner, distributedEventName, "uncommitted"),
		workflow.ErrWorkflowInfrastructure,
	)
	_, err = db.ExecContext(t.Context(), repair)
	require.NoError(t, err)
	publisher := startDistributedProcess(
		t,
		distributedChildConfig{
			Backend:    backend,
			Database:   database,
			Tag:        "publisher",
			Role:       distributedPublisher,
			HoldFinish: false,
			Heartbeat:  false,
			Delay:      0,
			Payload:    "committed",
		},
	)
	publisher.waitMessage(t, "published")
	<-publisher.finished
	require.NoError(t, publisher.waitErr)
	recovery := startDistributedProcess(
		t,
		childWorker(backend, database, "after-rollback", false, true),
	)
	recovery.waitMessage(t, "ready")
	out := distributedResult(t, wf, id)
	require.Equal(
		t,
		"committed",
		out.Approval,
		"a failed wake transaction must not consume the first publication",
	)
	require.Equal(t, "after-rollback", out.CompletedBy)
}
