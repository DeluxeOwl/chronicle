//go:build !windows

package workflow_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Only the selected connection loses server responses. CancelRequest opens a
// separate connection and still reaches PostgreSQL, where it cannot end an
// idle transaction. Neither frontend EOF nor Terminate ends the trapped one.
type distributedBlackholeProxy struct {
	ctx         context.Context
	listener    net.Listener
	target      string
	mu          sync.Mutex
	marker      []byte
	markerTag   byte
	connections map[net.Conn]struct{}
	trapped     chan *distributedBlackholeTrap
	acceptDone  chan struct{}
	forwarders  sync.WaitGroup
}

type distributedBlackholeTrap struct {
	idleTimeout  chan struct{}
	serverClosed chan struct{}
}

func startDistributedBlackholeProxy(
	t *testing.T,
	dsn, applicationName string,
) (string, *distributedBlackholeProxy) {
	t.Helper()
	databaseURL, err := url.Parse(dsn)
	require.NoError(t, err)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	proxy := &distributedBlackholeProxy{
		ctx: ctx, listener: listener, target: databaseURL.Host, mu: sync.Mutex{}, marker: nil, markerTag: 0,
		connections: make(map[net.Conn]struct{}), trapped: make(chan *distributedBlackholeTrap, 1),
		acceptDone: make(chan struct{}), forwarders: sync.WaitGroup{},
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
		proxy.mu.Lock()
		for connection := range proxy.connections {
			_ = connection.Close()
		}
		proxy.mu.Unlock()
		<-proxy.acceptDone
		proxy.forwarders.Wait()
	})
	databaseURL.Host = listener.Addr().String()
	query := databaseURL.Query()
	query.Set("sslmode", "disable")
	query.Set("connect_timeout", "1")
	query.Set("application_name", applicationName)
	databaseURL.RawQuery = query.Encode()
	return databaseURL.String(), proxy
}

func (p *distributedBlackholeProxy) arm(marker string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.marker = []byte(marker)
	p.markerTag = 'D'
}

// A COMMIT completion proves PostgreSQL accepted the transaction even when
// its result cannot reach the client. This is not an idle-transaction fault.
func (p *distributedBlackholeProxy) armCommit() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.marker = []byte("COMMIT\x00")
	p.markerTag = 'C'
}

func (p *distributedBlackholeProxy) trapRow(tag byte, payload []byte) *distributedBlackholeTrap {
	p.mu.Lock()
	defer p.mu.Unlock()
	if tag != p.markerTag || len(p.marker) == 0 || !bytes.Contains(payload, p.marker) {
		return nil
	}
	p.marker = nil
	return &distributedBlackholeTrap{
		idleTimeout:  make(chan struct{}),
		serverClosed: make(chan struct{}),
	}
}

func (p *distributedBlackholeProxy) track(connection net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ctx.Err() != nil {
		return false
	}
	p.connections[connection] = struct{}{}
	return true
}

func (p *distributedBlackholeProxy) forget(connection net.Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.connections, connection)
}

func (p *distributedBlackholeProxy) forward(client net.Conn) {
	defer client.Close()
	if !p.track(client) {
		return
	}
	defer p.forget(client)
	//nolint:exhaustruct // Standard dialer defaults apart from the connection bound.
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
	var trapped atomic.Bool
	clientDone := make(chan struct{})
	go func() {
		defer close(clientDone)
		_, _ = io.Copy(distributedBlackholeUpstream{server: server, trapped: &trapped}, client)
		if !trapped.Load() {
			_ = server.Close()
			_ = client.Close()
		}
	}()
	trap := p.forwardRows(client, server, &trapped)
	if trap != nil {
		close(trap.serverClosed)
		// Do not forward server EOF either. The caller's own transaction bound
		// must end its API call; an idle timeout alone only frees backend locks.
		<-p.ctx.Done()
	}
	_ = client.Close()
	_ = server.Close()
	<-clientDone
}

// SSL is disabled, so all backend packets have the PostgreSQL v3 tag and
// four-byte length (including the length field, excluding the tag). Startup
// authentication packets use the same framing and pass through untouched.
func (p *distributedBlackholeProxy) forwardRows(
	client, server net.Conn,
	trapped *atomic.Bool,
) *distributedBlackholeTrap {
	var trap *distributedBlackholeTrap
	idleTimeoutObserved := false
	for {
		var header [5]byte
		if _, err := io.ReadFull(server, header[:]); err != nil {
			return trap
		}
		length := binary.BigEndian.Uint32(header[1:])
		if length < 4 || length > 64<<20 {
			return trap
		}
		payload := make([]byte, int(length)-4)
		if _, err := io.ReadFull(server, payload); err != nil {
			return trap
		}
		if trap == nil {
			trap = p.trapRow(header[0], payload)
			if trap != nil {
				trapped.Store(true)
				p.trapped <- trap
			}
		}
		if trap != nil {
			// 25P03 identifies idle_in_transaction_session_timeout, not client
			// cancellation, statement timeout, or an unrelated server shutdown.
			if !idleTimeoutObserved && header[0] == 'E' &&
				bytes.Contains(payload, []byte("C25P03\x00")) {
				idleTimeoutObserved = true
				close(trap.idleTimeout)
			}
			continue
		}
		if _, err := client.Write(header[:]); err != nil {
			return nil
		}
		if _, err := client.Write(payload); err != nil {
			return nil
		}
	}
}

type distributedBlackholeUpstream struct {
	server  net.Conn
	trapped *atomic.Bool
}

func (w distributedBlackholeUpstream) Write(data []byte) (int, error) {
	if w.trapped.Load() {
		// pgx may send Terminate on context cancellation. Discard it rather
		// than letting a client deadline masquerade as a server transaction bound.
		return len(data), nil
	}
	return w.server.Write(data)
}
