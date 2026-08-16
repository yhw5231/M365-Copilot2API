package outbound

import (
	"context"
	"errors"
	"testing"
)

func TestRemoveProxyNormalizesAndRejectsMissing(t *testing.T) {
	if err := ConfigurePool([]string{"http://example.com/"}); err != nil {
		t.Fatal(err)
	}
	if err := RemoveProxy("http://example.com"); err != nil {
		t.Fatal(err)
	}
	if len(ProxyPoolStatus()) != 0 {
		t.Fatalf("pool not empty: %#v", ProxyPoolStatus())
	}
	if CurrentPool() != nil {
		t.Fatal("removing the last proxy must reset to the direct pool")
	}
	// Once the pool is gone the delete is a no-op (idempotent), matching the
	// "no proxy configured" state.
	if err := RemoveProxy("http://missing.example"); err != nil {
		t.Fatalf("removing after the pool reset should be idempotent: %v", err)
	}
	// With an active pool, deleting an unknown proxy still reports the error.
	if err := ConfigurePool([]string{"http://example.com/"}); err != nil {
		t.Fatal(err)
	}
	if err := RemoveProxy("http://missing.example"); err == nil {
		t.Fatal("expected missing proxy error")
	}
}

// TestConfigurePoolEmptyResetsToDirect pins the root-cause fix: configuring an
// empty (or blank-only) proxy list must restore the direct pool. Before the
// fix an empty pool object was installed, which forced every request to fail
// with ErrNoProxyNode even though no proxy was ever configured.
func TestConfigurePoolEmptyResetsToDirect(t *testing.T) {
	if err := ConfigurePool([]string{"http://example.com/"}); err != nil {
		t.Fatal(err)
	}
	if CurrentPool() == nil {
		t.Fatal("expected a pool after configuring one proxy")
	}
	if err := ConfigurePool(nil); err != nil {
		t.Fatal(err)
	}
	if CurrentPool() != nil {
		t.Fatal("ConfigurePool(nil) must reset to the direct pool")
	}
	if err := ConfigurePool([]string{"", " "}); err != nil {
		t.Fatal(err)
	}
	if CurrentPool() != nil {
		t.Fatal("blank-only ConfigurePool must reset to the direct pool")
	}
	if p := HTTPClientFor("acc-empty"); p == nil {
		t.Fatal("HTTPClientFor must still return a client with no pool")
	}
}

// TestProxyModeFallback verifies the three-state proxy policy semantics with a
// pool whose every node is in cooldown:
//   - strict: failures with ErrNoProxyNode;
//   - loose:  fall back to a direct connection (real network error, not
//     ErrNoProxyNode);
//   - direct: the pool is bypassed entirely (real network error, not
//     ErrNoProxyNode) and the pool binding is never touched.
func TestProxyModeFallback(t *testing.T) {
	proxyURL := "http://127.0.0.1:1" // closed port: any attempted dial fails fast
	if err := ConfigurePool([]string{proxyURL}); err != nil {
		t.Fatal(err)
	}
	p := CurrentPool()
	if p == nil {
		t.Fatal("pool expected")
	}
	p.mark(proxyURL, errors.New("dial failed"))
	if p.pickFor("acc-1") != nil {
		t.Fatal("pool node must be unhealthy after mark")
	}

	defer func() {
		_ = ConfigurePool(nil)
		SetProxyMode(ProxyModeStrict)
	}()

	// Strict: both HTTP and WebSocket must fail with ErrNoProxyNode.
	SetProxyMode(ProxyModeStrict)
	if _, err := p.HTTPClientFor("acc-1").Get("http://example.com"); !errors.Is(err, ErrNoProxyNode) {
		t.Fatalf("strict HTTP must fail with ErrNoProxyNode, got: %v", err)
	}
	if _, _, err := p.WebSocketDialerFor("acc-1").DialContext(context.Background(), "ws://example.com/", nil); !errors.Is(err, ErrNoProxyNode) {
		t.Fatalf("strict WebSocket must fail with ErrNoProxyNode, got: %v", err)
	}

	// Loose: fall back to direct; the real network error is returned instead.
	SetProxyMode(ProxyModeLoose)
	if _, err := p.HTTPClientFor("acc-1").Get("http://127.0.0.1:1"); errors.Is(err, ErrNoProxyNode) {
		t.Fatalf("loose HTTP must not report ErrNoProxyNode, got: %v", err)
	}
	if _, _, err := p.WebSocketDialerFor("acc-1").DialContext(context.Background(), "ws://127.0.0.1:1/", nil); errors.Is(err, ErrNoProxyNode) {
		t.Fatalf("loose WebSocket must not report ErrNoProxyNode, got: %v", err)
	}

	// Direct: the pool is bypassed; a direct dial fails with the network error.
	SetProxyMode(ProxyModeDirect)
	if _, err := p.HTTPClientFor("acc-1").Get("http://127.0.0.1:1"); errors.Is(err, ErrNoProxyNode) {
		t.Fatalf("direct HTTP must not report ErrNoProxyNode, got: %v", err)
	}
	if _, _, err := p.WebSocketDialerFor("acc-1").DialContext(context.Background(), "ws://127.0.0.1:1/", nil); errors.Is(err, ErrNoProxyNode) {
		t.Fatalf("direct WebSocket must not report ErrNoProxyNode, got: %v", err)
	}
	// Direct mode must not bind the account to any pool node.
	if len(p.List()) > 0 && len(p.List()[0]["boundAccounts"].([]string)) != 0 {
		t.Fatalf("direct mode must not bind accounts: %#v", p.List())
	}
}

// TestProxyModeEnvParsing covers M365_PROXY_MODE and the legacy
// M365_ENFORCE_PROXY startup parsing.
func TestProxyModeEnvParsing(t *testing.T) {
	// M365_PROXY_MODE wins over the legacy switch.
	t.Setenv(EnvProxyMode, "")
	if _, ok := parseBoolEnv(EnvEnforceProxy); ok {
		t.Fatal("legacy env must be unset for this test")
	}
	cases := map[string]string{
		"direct": ProxyModeDirect, "loose": ProxyModeLoose, "strict": ProxyModeStrict,
		"DIRECT": ProxyModeDirect, " Loose ": ProxyModeLoose,
		"bogus":  ProxyModeStrict, // unknown values normalize to strict
	}
	for raw, want := range cases {
		t.Setenv(EnvProxyMode, raw)
		ConfigureFromEnv()
		if ProxyMode() != want {
			t.Fatalf("M365_PROXY_MODE=%q: want %v, got %v", raw, want, ProxyMode())
		}
	}
	// An empty M365_PROXY_MODE leaves the current mode untouched.
	SetProxyMode(ProxyModeLoose)
	t.Setenv(EnvProxyMode, "")
	ConfigureFromEnv()
	if ProxyMode() != ProxyModeLoose {
		t.Fatalf("empty M365_PROXY_MODE must keep the current mode, got %v", ProxyMode())
	}
	// The legacy boolean switch maps to strict / loose.
	SetProxyMode(ProxyModeStrict)
	t.Setenv(EnvProxyMode, "")
	for raw, want := range map[string]string{"1": ProxyModeStrict, "true": ProxyModeStrict, "0": ProxyModeLoose, "false": ProxyModeLoose} {
		t.Setenv(EnvEnforceProxy, raw)
		ConfigureFromEnv()
		if ProxyMode() != want {
			t.Fatalf("M365_ENFORCE_PROXY=%q: want %v, got %v", raw, want, ProxyMode())
		}
	}
}