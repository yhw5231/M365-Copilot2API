package outbound

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/net/proxy"
)

const EnvProxy = "M365_OUTBOUND_PROXY"

// Proxy mode constants. The mode is a three-state policy:
//   - ProxyModeDirect: never use the proxy pool, every request connects
//     directly (configured proxies are ignored);
//   - ProxyModeLoose: prefer the proxy pool, but fall back to a direct
//     connection when no healthy proxy node is available;
//   - ProxyModeStrict (default): once a pool is configured, requests must go
//     through a healthy proxy node, otherwise they fail with ErrNoProxyNode.
const (
	ProxyModeDirect = "direct"
	ProxyModeLoose  = "loose"
	ProxyModeStrict = "strict"

	// EnvProxyMode selects the proxy mode at startup. Values: direct, loose,
	// strict (default). The legacy EnvEnforceProxy boolean is still honored
	// when EnvProxyMode is absent.
	EnvProxyMode = "M365_PROXY_MODE"
	// EnvEnforceProxy is the legacy boolean switch: "1"/"true"/"yes"/"on"
	// map to strict, "0"/"false"/"no"/"off" map to loose.
	EnvEnforceProxy = "M365_ENFORCE_PROXY"
)

type Clients struct {
	HTTP      *http.Client
	WebSocket *websocket.Dialer
}

var (
	clientsMu sync.RWMutex
	clients   = directClients()
	proxyPool *Pool
	// proxyMode holds the current three-state proxy policy; it starts as
	// strict (backward compatible) unless the environment says otherwise.
	proxyMode atomic.Value // string
)

func init() { proxyMode.Store(ProxyModeStrict) }

// directClients builds the shared direct (proxy-free) client pair. The HTTP
// transport and the WebSocket dialer share one TLS ClientSessionCache so TLS
// session resumption benefits both layers (upstream perf: ~30-80ms saved per
// warm request). The WS dialer also pins ALPN to http/1.1: negotiating h2 on
// the WebSocket upgrade produces "protocol h2 not supported" failures (upstream
// h2 ALPN fix), and smaller buffers cut per-connection memory (35MB -> 19MB).
func directClients() *Clients {
	tlsCache := tls.NewLRUClientSessionCache(32)
	httpTLSConf := &tls.Config{ClientSessionCache: tlsCache}
	wsTLSConf := &tls.Config{ClientSessionCache: tlsCache, NextProtos: []string{"http/1.1"}}
	t := &http.Transport{
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     true,
		TLSClientConfig:       httpTLSConf,
	}
	return &Clients{
		HTTP: &http.Client{Transport: t},
		WebSocket: &websocket.Dialer{
			HandshakeTimeout: 20 * time.Second,
			ReadBufferSize:   256 * 1024,
			WriteBufferSize:  16 * 1024,
			NetDialContext:   t.DialContext,
			TLSClientConfig:  wsTLSConf,
		},
	}
}

// normalizeProxyMode maps any input to one of the three supported modes,
// defaulting unknown or empty values to strict.
func normalizeProxyMode(m string) string {
	switch strings.ToLower(strings.TrimSpace(m)) {
	case ProxyModeDirect:
		return ProxyModeDirect
	case ProxyModeLoose:
		return ProxyModeLoose
	}
	return ProxyModeStrict
}

// ProxyMode returns the current proxy policy: direct, loose or strict.
func ProxyMode() string {
	if v := proxyMode.Load(); v != nil {
		return v.(string)
	}
	return ProxyModeStrict
}

// SetProxyMode updates the proxy policy. Unknown values are normalized to
// strict.
func SetProxyMode(m string) { proxyMode.Store(normalizeProxyMode(m)) }

// parseBoolEnv reads a boolean environment variable; empty means unset.
func parseBoolEnv(name string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true, true
	case "0", "false", "no", "off":
		return false, true
	}
	return false, false
}

// loadProxyModeFromEnv applies M365_PROXY_MODE, falling back to the legacy
// M365_ENFORCE_PROXY boolean switch.
func loadProxyModeFromEnv() {
	if raw := strings.TrimSpace(os.Getenv(EnvProxyMode)); raw != "" {
		SetProxyMode(raw)
		return
	}
	if v, ok := parseBoolEnv(EnvEnforceProxy); ok {
		if v {
			SetProxyMode(ProxyModeStrict)
		} else {
			SetProxyMode(ProxyModeLoose)
		}
	}
}

func ConfigureFromEnv() error {
	loadProxyModeFromEnv()
	raw := strings.TrimSpace(os.Getenv("M365_PROXY_POOL"))
	if raw != "" {
		return ConfigurePool(strings.FieldsFunc(raw, func(r rune) bool { return r == '\n' || r == '\r' || r == ',' }))
	}
	return Configure(os.Getenv(EnvProxy))
}
func Configure(raw string) error {
	c, e := New(raw)
	if e != nil {
		return e
	}
	clientsMu.Lock()
	clients = c
	proxyPool = nil
	clientsMu.Unlock()
	return nil
}
func ConfigurePool(raw []string) error {
	// An empty proxy list means "no proxy": reset to the direct pool instead of
	// installing an empty pool, which would force every request to fail with
	// ErrNoProxyNode even though no proxy was ever configured.
	if len(raw) == 0 {
		clientsMu.Lock()
		proxyPool = nil
		clientsMu.Unlock()
		return nil
	}
	p, e := NewPool(raw)
	if e != nil {
		return e
	}
	if len(p.entries) == 0 {
		// Every configured entry was blank or a duplicate: nothing to proxy
		// through, so stay on the direct pool.
		clientsMu.Lock()
		proxyPool = nil
		clientsMu.Unlock()
		return nil
	}
	clientsMu.Lock()
	proxyPool = p
	clientsMu.Unlock()
	return nil
}

func CurrentPool() *Pool { clientsMu.RLock(); defer clientsMu.RUnlock(); return proxyPool }

func ProxyPoolStatus() []map[string]any {
	clientsMu.RLock()
	p := proxyPool
	clientsMu.RUnlock()
	if p == nil {
		return []map[string]any{}
	}
	return p.List()
}

func AddProxy(raw string) error {
	clientsMu.RLock()
	p := proxyPool
	clientsMu.RUnlock()
	if p == nil {
		return ConfigurePool([]string{raw})
	}
	items := make([]string, 0)
	for _, item := range p.List() {
		if v, ok := item["url"].(string); ok {
			items = append(items, v)
		}
	}
	items = append(items, raw)
	return ConfigurePool(items)
}

func RemoveProxy(raw string) error {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	clientsMu.RLock()
	p := proxyPool
	clientsMu.RUnlock()
	if p == nil {
		return nil
	}
	items := make([]string, 0)
	found := false
	for _, item := range p.List() {
		if v, ok := item["url"].(string); ok {
			if strings.TrimRight(strings.TrimSpace(v), "/") == raw {
				found = true
				continue
			}
			items = append(items, v)
		}
	}
	if !found {
		return fmt.Errorf("proxy not found: %s", raw)
	}
	return ConfigurePool(items)
}
func HTTPClient() *http.Client {
	clientsMu.RLock()
	p, c := proxyPool, clients.HTTP
	clientsMu.RUnlock()
	if p != nil {
		return p.HTTPClient()
	}
	return c
}

// HTTPClientFor returns the pool HTTP client pinned to the account's bound
// proxy node. Without a pool the shared direct client is returned.
func HTTPClientFor(accountID string) *http.Client {
	clientsMu.RLock()
	p, c := proxyPool, clients.HTTP
	clientsMu.RUnlock()
	if p != nil {
		return p.HTTPClientFor(accountID)
	}
	return c
}
func WebSocketDialer() *websocket.Dialer {
	clientsMu.RLock()
	p, c := proxyPool, clients.WebSocket
	clientsMu.RUnlock()
	if p != nil {
		return p.WebSocketDialer()
	}
	d := *c
	return &d
}

// WebSocketDialerFor returns the pool WebSocket dialer pinned to the account's
// bound proxy node (account-node binding; see Pool.pickFor). Without a pool the
// shared direct dialer is returned.
func WebSocketDialerFor(accountID string) *websocket.Dialer {
	clientsMu.RLock()
	p, c := proxyPool, clients.WebSocket
	clientsMu.RUnlock()
	if p != nil {
		return p.WebSocketDialerFor(accountID)
	}
	d := *c
	return &d
}
func ValidateProxyURL(raw string) error { _, e := New(raw); return e }
func New(raw string) (*Clients, error) {
	c := directClients()
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return c, nil
	}
	u, e := url.Parse(raw)
	if e != nil || u.Host == "" {
		return nil, fmt.Errorf("outbound proxy must be a complete socks5://, http://, or https:// URL")
	}
	if u.Fragment != "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" {
		return nil, fmt.Errorf("outbound proxy URL must not include a path, query, or fragment")
	}
	switch strings.ToLower(u.Scheme) {
	case "http":
		c.HTTP.Transport.(*http.Transport).Proxy = http.ProxyURL(u)
		c.WebSocket.Proxy = http.ProxyURL(u)
	case "https":
		// Do not use Transport.Proxy here: Go's standard transport performs its
		// own proxy TLS handshake and bypasses our IP-certificate compatibility.
		transport := c.HTTP.Transport.(*http.Transport)
		transport.Proxy = nil
		transport.DialContext = httpsProxyDialer{proxyURL: u}.DialContext
		c.WebSocket.NetDialContext = httpsProxyDialer{proxyURL: u}.DialContext
	case "socks5":
		var a *proxy.Auth
		if u.User != nil {
			pw, _ := u.User.Password()
			a = &proxy.Auth{User: u.User.Username(), Password: pw}
		}
		d, e := proxy.SOCKS5("tcp", u.Host, a, proxy.Direct)
		if e != nil {
			return nil, fmt.Errorf("configure SOCKS5 proxy: %w", e)
		}
		x := socksContextDialer{dialer: d}
		c.HTTP.Transport.(*http.Transport).DialContext = x.DialContext
		c.WebSocket.NetDialContext = x.DialContext
	default:
		return nil, fmt.Errorf("outbound proxy scheme %q is unsupported; use socks5, http, or https", u.Scheme)
	}
	return c, nil
}

type httpsProxyDialer struct{ proxyURL *url.URL }

func (d httpsProxyDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if network != "tcp" {
		return nil, fmt.Errorf("HTTPS proxy only supports tcp, got %q", network)
	}
	a := d.proxyURL.Host
	if d.proxyURL.Port() == "" {
		a = net.JoinHostPort(d.proxyURL.Hostname(), "443")
	}
	raw, e := (&net.Dialer{}).DialContext(ctx, network, a)
	if e != nil {
		return nil, e
	}
	// Proxy endpoints commonly present a certificate for their hostname while users
	// configure an IP address. This option affects only the TLS hop to the proxy;
	// target-site certificate verification remains enabled.
	insecureProxyTLS := os.Getenv("M365_PROXY_INSECURE_TLS") == "1" || os.Getenv("M365_PROXY_INSECURE_TLS") == "true" || net.ParseIP(d.proxyURL.Hostname()) != nil
	conn := tls.Client(raw, &tls.Config{ServerName: d.proxyURL.Hostname(), MinVersion: tls.VersionTLS12, InsecureSkipVerify: insecureProxyTLS}) // #nosec G402 -- explicitly scoped to configured proxy TLS

	if e = conn.HandshakeContext(ctx); e != nil {
		raw.Close()
		return nil, e
	}
	q := &http.Request{Method: http.MethodConnect, URL: &url.URL{Opaque: address}, Host: address, Header: make(http.Header)}
	if d.proxyURL.User != nil {
		pw, _ := d.proxyURL.User.Password()
		q.SetBasicAuth(d.proxyURL.User.Username(), pw)
	}
	if e = q.Write(conn); e != nil {
		conn.Close()
		return nil, e
	}
	rd := bufio.NewReader(conn)
	resp, e := http.ReadResponse(rd, q)
	if e != nil {
		conn.Close()
		return nil, e
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		conn.Close()
		return nil, fmt.Errorf("HTTPS proxy CONNECT %s: %s", address, resp.Status)
	}
	return &bufferedConn{Conn: conn, reader: rd}, nil
}

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.reader.Read(p) }

type socksContextDialer struct{ dialer proxy.Dialer }

func (d socksContextDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	ch := make(chan struct {
		c net.Conn
		e error
	}, 1)
	go func() {
		c, e := d.dialer.Dial(network, address)
		ch <- struct {
			c net.Conn
			e error
		}{c, e}
	}()
	select {
	case r := <-ch:
		return r.c, r.e
	case <-ctx.Done():
		go func() {
			r := <-ch
			if r.c != nil {
				r.c.Close()
			}
		}()
		return nil, ctx.Err()
	}
}
