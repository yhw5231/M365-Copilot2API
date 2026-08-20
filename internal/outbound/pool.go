package outbound

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type poolEntry struct {
	raw       string
	clients   *Clients
	failures  int
	cooldown  time.Time
	lastCheck time.Time
	latency   time.Duration
	lastError string
	health    string
}
type Pool struct {
	mu      sync.Mutex
	entries []*poolEntry
	next    int
	// bound pins each account ID (binding key) to the proxy node it uses. A
	// bound node is exclusive to its account while healthy; on failure the
	// account moves to an unbound node first and finally to any healthy node.
	bound map[string]*poolEntry
	// lastUsed records the latest activity for each runtime account-to-proxy
	// binding so idle bindings can be released for another account.
	lastUsed map[string]time.Time
}

func NewPool(raw []string) (*Pool, error) {
	p := &Pool{bound: map[string]*poolEntry{}, lastUsed: map[string]time.Time{}}
	seen := map[string]bool{}
	for _, v := range raw {
		v = strings.TrimSpace(v)
		if v == "" || seen[v] {
			continue
		}
		c, err := New(v)
		if err != nil {
			return nil, fmt.Errorf("proxy %q: %w", v, err)
		}
		seen[v] = true
		p.entries = append(p.entries, &poolEntry{raw: v, clients: c})
	}
	return p, nil
}
func (e *poolEntry) healthy(now time.Time) bool { return !now.Before(e.cooldown) }
func (p *Pool) pick() *poolEntry {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.entries) == 0 {
		return nil
	}
	now := time.Now()
	for i := 0; i < len(p.entries); i++ {
		e := p.entries[(p.next+i)%len(p.entries)]
		if e.healthy(now) {
			p.next = (p.next + i + 1) % len(p.entries)
			return e
		}
	}
	e := p.entries[p.next%len(p.entries)]
	p.next = (p.next + 1) % len(p.entries)
	return e
}

// ErrNoProxyNode reports that the account has no usable proxy node: its bound
// node is unhealthy and no unbound healthy node is left. The caller must not
// reuse the account; it should switch the request to an account whose bound
// node is healthy (see Pool.FailoverTarget).
var ErrNoProxyNode = errors.New("no healthy proxy node available for account")

// pickFor returns the proxy node for the given binding key (account ID):
//  1. the account's bound node while it is healthy — the account keeps using
//     the same node across requests and no other account can take it;
//  2. an unbound healthy node, which becomes the account's new binding
//     (preferred failover target when the bound node breaks).
//
// When no unbound healthy node remains the pool returns nil instead of
// borrowing a node bound to another account: per the account-node binding
// rules the request must then be re-routed to an account whose bound node is
// healthy (FailoverTarget). Nodes in cooldown (recent failure) are never
// selected; pickFor advances the round-robin cursor so concurrent callers
// spread across unbound nodes.
const proxyBindingIdleTimeout = 5 * time.Minute

func (p *Pool) releaseIdleBindingsLocked(now time.Time) {
	for account, lastUsed := range p.lastUsed {
		if now.Sub(lastUsed) > proxyBindingIdleTimeout {
			delete(p.bound, account)
			delete(p.lastUsed, account)
		}
	}
}

// Unbind releases an account's runtime proxy-pool binding without modifying
// any manually configured persistent proxy URL for that account.
func (p *Pool) Unbind(account string) {
	if p == nil || account == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.bound, account)
	delete(p.lastUsed, account)
}

func (p *Pool) pickFor(account string) *poolEntry {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.entries) == 0 {
		return nil
	}
	now := time.Now()
	p.releaseIdleBindingsLocked(now)
	if e := p.bound[account]; e != nil {
		for _, en := range p.entries {
			if en == e {
				if en.healthy(now) {
					p.lastUsed[account] = now
					return e
				}
				break
			}
		}
		// Bound node removed or unhealthy: release the binding and fall through.
		delete(p.bound, account)
		delete(p.lastUsed, account)
	}
	for i := 0; i < len(p.entries); i++ {
		e := p.entries[(p.next+i)%len(p.entries)]
		if e.healthy(now) && !p.boundTo(e) {
			p.bound[account] = e
			p.lastUsed[account] = now
			p.next = (p.next + i + 1) % len(p.entries)
			return e
		}
	}
	return nil
}

func (p *Pool) boundTo(e *poolEntry) bool {
	for _, b := range p.bound {
		if b == e {
			return true
		}
	}
	return false
}

// FailoverTarget returns another account whose bound node is currently
// healthy, excluding the given account. It is the recommended request target
// when pickFor returns nil (no unbound healthy node left): instead of reusing
// another account's node, the request itself moves to that account.
func (p *Pool) FailoverTarget(account string) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	for acct, e := range p.bound {
		if acct == account {
			continue
		}
		for _, en := range p.entries {
			if en == e && en.healthy(now) {
				return acct, true
			}
		}
	}
	return "", false
}
func (p *Pool) mark(raw string, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, e := range p.entries {
		if e.raw == raw {
			if err == nil {
				e.failures = 0
				e.cooldown = time.Time{}
			} else {
				e.failures++
				d := time.Duration(e.failures) * 2 * time.Second
				if d > 2*time.Minute {
					d = 2 * time.Minute
				}
				e.cooldown = time.Now().Add(d)
			}
			return
		}
	}
}
func (p *Pool) HTTPClient() *http.Client {
	if e := p.pick(); e != nil {
		return &http.Client{Transport: &poolRoundTripper{pool: p, entry: e, base: e.clients.HTTP.Transport}}
	}
	return directClients().HTTP
}

// HTTPClientFor returns an HTTP client pinned to the proxy node bound to the
// account. A failed request is replayed once on an unbound healthy node; when
// no usable node is left the request fails with ErrNoProxyNode so the caller
// can switch the request to an account whose bound node is healthy. In direct
// mode the pool is bypassed entirely; in loose mode a missing node falls back
// to a direct client instead of failing.
func (p *Pool) HTTPClientFor(account string) *http.Client {
	if ProxyMode() == ProxyModeDirect {
		return directClients().HTTP
	}
	if e := p.pickFor(account); e != nil {
		return &http.Client{Transport: &poolRoundTripper{pool: p, entry: e, base: e.clients.HTTP.Transport, account: account}}
	}
	if ProxyMode() == ProxyModeStrict {
		return &http.Client{Transport: errTripper{err: ErrNoProxyNode}}
	}
	return directClients().HTTP
}
func (p *Pool) WebSocketDialer() *websocket.Dialer {
	base := directClients().WebSocket
	baseDialer := &net.Dialer{}
	var mu sync.Mutex
	var sticky *poolEntry
	base.NetDialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		mu.Lock()
		e := sticky
		if e == nil {
			e = p.pick()
			sticky = e
		}
		mu.Unlock()
		if e == nil {
			return baseDialer.DialContext(ctx, network, address)
		}
		dial := e.clients.WebSocket.NetDialContext
		if dial == nil {
			dial = baseDialer.DialContext
		}
		conn, err := dial(ctx, network, address)
		p.mark(e.raw, err)
		if err != nil {
			mu.Lock()
			if sticky == e {
				sticky = nil
			}
			mu.Unlock()
		}
		return conn, err
	}
	return base
}

// WebSocketDialerFor returns a WebSocket dialer pinned to the proxy node bound
// to the account. A failed dial is retried once on an unbound healthy node
// (preferred failover); if no usable node is left the dial returns
// ErrNoProxyNode so the caller can switch the request to an account whose
// bound node is healthy. In direct mode the pool is bypassed entirely; in
// loose mode a missing node falls back to a direct connection instead.
func (p *Pool) WebSocketDialerFor(account string) *websocket.Dialer {
	if ProxyMode() == ProxyModeDirect {
		d := *directClients().WebSocket
		return &d
	}
	base := directClients().WebSocket
	baseDialer := &net.Dialer{}
	base.NetDialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		for attempt := 0; attempt < 2; attempt++ {
			e := p.pickFor(account)
			if e == nil {
				if ProxyMode() != ProxyModeStrict {
					return baseDialer.DialContext(ctx, network, address)
				}
				return nil, ErrNoProxyNode
			}
			dial := e.clients.WebSocket.NetDialContext
			if dial == nil {
				dial = baseDialer.DialContext
			}
			conn, err := dial(ctx, network, address)
			p.mark(e.raw, err)
			if err == nil {
				return conn, nil
			}
			// mark() put the node in cooldown, so the next pickFor moves the
			// account to an unbound healthy node.
		}
		return nil, ErrNoProxyNode
	}
	return base
}
func (p *Pool) List() []map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]map[string]any, 0, len(p.entries))
	for _, e := range p.entries {
		bounds := make([]string, 0, 2)
		for acct, b := range p.bound {
			if b == e {
				bounds = append(bounds, acct)
			}
		}
		out = append(out, map[string]any{"url": e.raw, "failures": e.failures, "cooldownUntil": e.cooldown, "lastCheck": e.lastCheck, "latencyMs": e.latency.Milliseconds(), "lastError": e.lastError, "health": e.health, "boundAccounts": bounds})
	}
	return out
}
func (p *Pool) Remove(raw string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for acct, b := range p.bound {
		if b.raw == raw {
			delete(p.bound, acct)
		}
	}
	for i, e := range p.entries {
		if e.raw == raw {
			p.entries = append(p.entries[:i], p.entries[i+1:]...)
			return
		}
	}
}

type poolRoundTripper struct {
	pool    *Pool
	entry   *poolEntry
	base    http.RoundTripper
	account string // binding key; non-empty replays only on the account's usable nodes
}

// errTripper fails every request with a fixed error (no proxy node available).
type errTripper struct{ err error }

func (t errTripper) RoundTrip(r *http.Request) (*http.Response, error) { return nil, t.err }

func (t *poolRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(r)
	t.pool.mark(t.entry.raw, err)
	if err == nil {
		return resp, nil
	}
	// Replay the request on the next usable proxy once (body must be replayable).
	if r.Body != nil && r.GetBody == nil {
		return resp, err
	}
	for i := 0; i < len(t.pool.entries)+1; i++ {
		var next *poolEntry
		if t.account != "" {
			// Account-bound replay: only unbound healthy nodes count; when
			// none is left the request must move to another account instead
			// of borrowing that account's node. In loose mode the replay
			// simply stops and the original error is returned.
			next = t.pool.pickFor(t.account)
			if next == nil {
				if ProxyMode() != ProxyModeStrict {
					break
				}
				return nil, ErrNoProxyNode
			}
		} else {
			next = t.pool.pick()
		}
		if next == t.entry {
			break
		}
		body, berr := r.GetBody()
		if berr != nil {
			break
		}
		retry := r.Clone(r.Context())
		retry.Body = body
		resp2, err2 := next.clients.HTTP.Transport.RoundTrip(retry)
		t.pool.mark(next.raw, err2)
		if err2 == nil {
			return resp2, nil
		}
	}
	return resp, err
}
