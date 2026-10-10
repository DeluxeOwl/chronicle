//go:build !windows

package workflow_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/DeluxeOwl/chronicle/workflow"
	"github.com/stretchr/testify/require"
)

func distributedHeartbeatsKeepLongActivitiesExclusive(t *testing.T, backend, database string) {
	db, wf, _ := newDistributedFixture(t, backend, database)
	id, err := wf.Start(t.Context(), &distributedParams{Mode: distributedWork, Sleep: 0})
	require.NoError(t, err)
	config := childWorker(backend, database, "heartbeat-owner", true, true)
	if backend == distributedPostgres {
		// Postgres ownership uses the database clock, not a worker's clock.
		config.ClockOffset = -time.Hour
	}
	first := startDistributedProcess(t, config)
	first.waitMessage(t, "ready")
	first.waitMessage(t, "entered")
	second := startDistributedProcess(t, childWorker(backend, database, "competitor", false, true))
	second.waitMessage(t, "ready")
	timer := time.NewTimer(3 * distributedLease)
	defer timer.Stop()
	select {
	case message := <-second.messages:
		t.Fatalf("competitor entered a live activity: %+v\n%s", message, second.log())
	case <-second.finished:
		t.Fatalf("competing worker exited: %v\n%s", second.waitErr, second.log())
	case <-timer.C:
	}
	first.release(t)
	out := distributedResult(t, wf, id)
	require.Equal(t, "heartbeat-owner", out.CompletedBy)
	require.Equal(
		t,
		1,
		distributedCalls(t, db, backend, id, "finish"),
		"heartbeats must prevent redelivery while the callback owns its claim",
	)
}

func distributedCancellationFencesAnActiveWorker(t *testing.T, backend, database string) {
	db, wf, inspector := newDistributedFixture(t, backend, database)
	id, err := wf.Start(t.Context(), &distributedParams{Mode: distributedWork, Sleep: 0})
	require.NoError(t, err)
	// Keep the claim alive so lease expiry cannot masquerade as cancellation fencing.
	worker := startDistributedProcess(t, childWorker(backend, database, "active", true, true))
	worker.waitMessage(t, "ready")
	worker.waitMessage(t, "entered")
	canceller := startDistributedProcess(
		t,
		distributedChildConfig{
			Backend:    backend,
			Database:   database,
			Tag:        "canceller",
			Role:       distributedCanceller,
			HoldFinish: false,
			Heartbeat:  false,
			Delay:      0,
			Payload:    "",
			InstanceID: id,
		},
	)
	canceller.waitMessage(t, "cancelled")
	<-canceller.finished
	require.NoError(t, canceller.waitErr)
	worker.release(t)
	returned := worker.waitMessage(t, "returned")
	require.NotEmpty(
		t,
		returned.Error,
		"a running callback must not commit after persisted cancellation",
	)
	detail, err := inspector.Get(t.Context(), id)
	require.NoError(t, err)
	require.Equal(t, workflow.StatusCancelled, detail.Summary.Status)
	require.Equal(t, 1, detail.Summary.CompletedSteps)
	_, err = wf.GetResult(t.Context(), id)
	require.Error(t, err)
	require.Equal(
		t,
		1,
		distributedCalls(t, db, backend, id, "finish"),
		"cancellation cannot undo an already visible effect",
	)
}

func distributedPublicationAndWaitRegistrationCannotStrandWork(
	t *testing.T,
	backend, database string,
) {
	db, wf, _ := newDistributedFixture(t, backend, database)
	for i := range 2 {
		worker := startDistributedProcess(
			t,
			childWorker(backend, database, fmt.Sprintf("worker-%d", i), false, true),
		)
		worker.waitMessage(t, "ready")
	}
	const count = 24
	names := make([]string, count)
	for i := range names {
		names[i] = fmt.Sprintf("distributed:race:%d", count-1-i)
	}
	publisher := startDistributedProcess(
		t,
		distributedChildConfig{
			Backend:      backend,
			Database:     database,
			Tag:          "racing-publisher",
			Role:         distributedPublisher,
			HoldFinish:   false,
			Heartbeat:    false,
			Delay:        0,
			Payload:      "published",
			Publications: names,
		},
	)
	publisher.waitMessage(t, "ready")
	start := make(chan struct{})
	type startedInstance struct {
		id  workflow.InstanceID
		err error
	}
	ids := make(chan startedInstance, count)
	var producers sync.WaitGroup
	for i := range count {
		producers.Go(func() {
			<-start
			id, err := wf.Start(
				t.Context(),
				&distributedParams{
					Mode:      distributedWait,
					Sleep:     0,
					EventName: fmt.Sprintf("distributed:race:%d", i),
				},
			)
			ids <- startedInstance{id: id, err: err}
		})
	}
	close(start)
	publisher.release(t)
	producers.Wait()
	close(ids)
	publisher.waitMessage(t, "published")
	<-publisher.finished
	require.NoError(t, publisher.waitErr)
	for result := range ids {
		require.NoError(t, result.err)
		out := distributedResult(t, wf, result.id)
		require.Equal(
			t,
			"published",
			out.Approval,
			"pre-publication and wait registration must not miss each other",
		)
		require.Equal(t, 1, distributedCalls(t, db, backend, result.id, "prepare"))
		require.Equal(t, 1, distributedCalls(t, db, backend, result.id, "finish"))
	}
}

func distributedStoragePartitionDoesNotLoseWork(t *testing.T, backend, database string) {
	if backend != distributedPostgres {
		t.Skip("SQLite has no TCP storage connection")
	}
	db, wf, inspector := newDistributedFixture(t, backend, database)
	proxied, proxy := startDistributedPartitionProxy(t, database)
	id, err := wf.Start(t.Context(), &distributedParams{Mode: distributedWork, Sleep: 0})
	require.NoError(t, err)
	isolated := startDistributedProcess(t, childWorker(backend, proxied, "partitioned", true, true))
	isolated.waitMessage(t, "ready")
	isolated.waitMessage(t, "entered")
	// Drop every established database socket and reject reconnections, while
	// another process can still reach the same durable database directly.
	proxy.setBlocked(true)
	healthy := startDistributedProcess(t, childWorker(backend, database, "healthy", false, true))
	healthy.waitMessage(t, "ready")
	out := distributedResult(t, wf, id)
	require.Equal(t, "partitioned", out.PreparedBy)
	require.Equal(t, "healthy", out.CompletedBy)
	proxy.setBlocked(false)
	isolated.release(t)
	returned := isolated.waitMessage(t, "returned")
	require.NotEmpty(t, returned.Error)
	detail, err := inspector.Get(t.Context(), id)
	require.NoError(t, err)
	require.Equal(t, workflow.StatusCompleted, detail.Summary.Status)
	require.Equal(t, 1, detail.Summary.Attempt, "storage failures are not domain retry attempts")
	require.Equal(t, 1, distributedCalls(t, db, backend, id, "prepare"))
	require.Equal(t, 2, distributedCalls(t, db, backend, id, "finish"))
}

type distributedPartitionProxy struct {
	ctx         context.Context
	cancel      context.CancelFunc
	listener    net.Listener
	target      string
	mu          sync.Mutex
	blocked     bool
	connections map[net.Conn]struct{}
	acceptDone  chan struct{}
	forwarders  sync.WaitGroup
}

func startDistributedPartitionProxy(t *testing.T, dsn string) (string, *distributedPartitionProxy) {
	t.Helper()
	databaseURL, err := url.Parse(dsn)
	require.NoError(t, err)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	proxy := &distributedPartitionProxy{
		ctx:         ctx,
		cancel:      cancel,
		listener:    listener,
		target:      databaseURL.Host,
		mu:          sync.Mutex{},
		blocked:     false,
		connections: make(map[net.Conn]struct{}),
		acceptDone:  make(chan struct{}),
		forwarders:  sync.WaitGroup{},
	}
	go func() {
		defer close(proxy.acceptDone)
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			proxy.forwarders.Go(func() { proxy.forward(client) })
		}
	}()
	t.Cleanup(func() {
		cancel()
		_ = listener.Close()
		proxy.setBlocked(true)
		<-proxy.acceptDone
		proxy.forwarders.Wait()
	})
	databaseURL.Host = listener.Addr().String()
	query := databaseURL.Query()
	query.Set("connect_timeout", "1")
	databaseURL.RawQuery = query.Encode()
	return databaseURL.String(), proxy
}

func (p *distributedPartitionProxy) setBlocked(blocked bool) {
	p.mu.Lock()
	p.blocked = blocked
	connections := make([]net.Conn, 0, len(p.connections))
	if blocked {
		for connection := range p.connections {
			connections = append(connections, connection)
		}
	}
	p.mu.Unlock()
	for _, connection := range connections {
		_ = connection.Close()
	}
}

func (p *distributedPartitionProxy) track(connection net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.blocked {
		return false
	}
	p.connections[connection] = struct{}{}
	return true
}

func (p *distributedPartitionProxy) forget(connection net.Conn) {
	p.mu.Lock()
	delete(p.connections, connection)
	p.mu.Unlock()
}

func (p *distributedPartitionProxy) forward(client net.Conn) {
	defer client.Close()
	if !p.track(client) {
		return
	}
	defer p.forget(client)
	//nolint:exhaustruct // The remaining network settings use their standard defaults.
	dialer := net.Dialer{Timeout: time.Second}
	server, err := dialer.DialContext(p.ctx, "tcp", p.target)
	if err != nil {
		return
	}
	defer server.Close()
	if !p.track(server) {
		return
	}
	defer p.forget(server)
	done := make(chan struct{})
	go func() { defer close(done); _, _ = io.Copy(server, client); _ = server.Close(); _ = client.Close() }()
	_, _ = io.Copy(client, server)
	_ = client.Close()
	_ = server.Close()
	<-done
}
