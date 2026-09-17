package outbound

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// hangingDial returns a NetDialContext that blocks until the passed context
// is done — a dead node whose TCP connect hangs forever (SYN dropped by a
// firewall, unresponsive proxy). Without a per-attempt timeout this would
// consume the caller's whole context.
func hangingDial() func(ctx context.Context, network, address string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
}

// newPoolWithDialers builds a Pool whose node clients use the given wire
// dialers (one per raw entry, in order). raw labels are fixed so the test can
// assert which node the pool failed over to.
func newPoolWithDialers(t *testing.T, dials ...func(ctx context.Context, network, address string) (net.Conn, error)) *Pool {
	t.Helper()
	p := &Pool{bound: map[string]*poolEntry{}, lastUsed: map[string]time.Time{}}
	for i, d := range dials {
		p.entries = append(p.entries, &poolEntry{
			raw:     "node-" + string(rune('a'+i)),
			clients: &Clients{WebSocket: &websocket.Dialer{NetDialContext: d}},
		})
	}
	return p
}

// TestPoolWebSocketDialerForBoundedAttempt verifies the per-attempt dial
// window: a hanging node fails within its own budget (not the caller's whole
// context), gets marked in cooldown, and the second attempt fails over to a
// healthy node with a fresh window.
func TestPoolWebSocketDialerForBoundedAttempt(t *testing.T) {
	prior := dialAttemptTimeout
	dialAttemptTimeout = 200 * time.Millisecond
	t.Cleanup(func() { dialAttemptTimeout = prior })

	var wiredOk sync.Mutex
	okCalled := false
	okDial := func(ctx context.Context, network, address string) (net.Conn, error) {
		wiredOk.Lock()
		okCalled = true
		wiredOk.Unlock()
		server, client := net.Pipe()
		_ = server // keep ref so gc does not close the pipe mid-test
		return client, nil
	}
	p := newPoolWithDialers(t, hangingDial(), okDial)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	d := p.WebSocketDialerFor("account-1")
	conn, err := d.NetDialContext(ctx, "tcp", "m365.example")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("NetDialContext failed after hang+failover: %v", err)
	}
	defer conn.Close()
	if !okCalled {
		t.Fatal("healthy node dial was never attempted after the first node hung")
	}
	// The first node must have consumed only its own attempt window, so the
	// whole call finishes far below the 5s outer deadline.
	if elapsed > 3*time.Second {
		t.Fatalf("failover took %v, want well under the 5s caller deadline", elapsed)
	}
	// The hung node should now be in cooldown.
	if p.entries[0].cooldown.IsZero() {
		t.Fatal("hung node was not marked down after its attempt window expired")
	}
}

// TestPoolWebSocketDialerForAllDeadReturnsFast verifies that when every node
// hangs, the dialer gives up with ErrNoProxyNode instead of blocking until the
// caller's context expires.
func TestPoolWebSocketDialerForAllDeadReturnsFast(t *testing.T) {
	prior := dialAttemptTimeout
	dialAttemptTimeout = 150 * time.Millisecond
	t.Cleanup(func() { dialAttemptTimeout = prior })

	p := newPoolWithDialers(t, hangingDial(), hangingDial())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	d := p.WebSocketDialerFor("account-1")
	_, err := d.NetDialContext(ctx, "tcp", "m365.example")
	elapsed := time.Since(start)
	if !errors.Is(err, ErrNoProxyNode) {
		t.Fatalf("expected ErrNoProxyNode when all nodes hang, got %v", err)
	}
	// Two bounded attempts (150ms each) must finish nowhere near the 5s deadline.
	if elapsed > 3*time.Second {
		t.Fatalf("all-dead dial took %v, want ~2 bounded attempts", elapsed)
	}
}

// TestAttemptContextLeavesRoomForFailover verifies the window halving: when
// the caller has a deadline, each attempt is capped at half the remaining
// budget so the second (failover) attempt still has a window to dial.
func TestAttemptContextLeavesRoomForFailover(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	attemptCtx, attemptCancel := attemptContext(ctx)
	defer attemptCancel()
	deadline, ok := attemptCtx.Deadline()
	if !ok {
		t.Fatal("attempt context has no deadline")
	}
	// Half of 1s = 500ms; with the default 30s attempt timeout the window must
	// be the smaller half.
	if d := time.Until(deadline); d > 600*time.Millisecond {
		t.Fatalf("attempt window %v, want <= half the remaining budget (500ms)", d)
	}
}
