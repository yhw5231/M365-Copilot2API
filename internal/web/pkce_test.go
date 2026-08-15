package web

import (
	"encoding/json"
	"m365-copilot2api/internal/outbound"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestStartPKCEUsesBrowserClientDefaults(t *testing.T) {
	t.Setenv("M365_CLIENT_ID", "")
	t.Setenv("M365_AUTHORITY", "")
	t.Setenv("M365_REDIRECT_URI", "")

	s := &Server{pkce: map[string]pendingPKCE{}}
	rr := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/auth/start", nil)
	r.Host = "172.30.0.214"
	r.Header.Set("X-Forwarded-Host", "unregistered.example")
	r.Header.Set("X-Forwarded-Proto", "https")
	s.startPKCE(rr, r)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var response struct {
		State       string `json:"state"`
		URL         string `json:"url"`
		RedirectURI string `json:"redirectUri"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response.State == "" {
		t.Fatal("response omitted state")
	}
	if got, want := response.RedirectURI, "https://login.microsoftonline.com/common/oauth2/nativeclient"; got != want {
		t.Fatalf("redirect URI = %q, want %q", got, want)
	}
	u, err := url.Parse(response.URL)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := u.Query().Get("client_id"), "c0ab8ce9-e9a0-42e7-b064-33d422df41f1"; got != want {
		t.Fatalf("client_id = %q, want %q", got, want)
	}
	if got := u.Query().Get("redirect_uri"); got != response.RedirectURI {
		t.Fatalf("authorization redirect URI = %q, response redirect URI = %q", got, response.RedirectURI)
	}
}

func TestStartPKCEUsesConfiguredRedirectURIExactly(t *testing.T) {
	const redirectURI = "https://app.example.test/api/auth/callback"
	t.Setenv("M365_REDIRECT_URI", redirectURI)
	t.Setenv("M365_PUBLIC_URL", "https://other.example.test")

	s := &Server{pkce: map[string]pendingPKCE{}}
	rr := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/auth/start", nil)
	r.Host = "172.30.0.214"
	r.Header.Set("X-Forwarded-Host", "unregistered.example")
	r.Header.Set("X-Forwarded-Proto", "https")
	s.startPKCE(rr, r)

	var response struct {
		URL         string `json:"url"`
		RedirectURI string `json:"redirectUri"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if got := response.RedirectURI; got != redirectURI {
		t.Fatalf("redirect URI = %q, want %q", got, redirectURI)
	}
	u, err := url.Parse(response.URL)
	if err != nil {
		t.Fatal(err)
	}
	if got := u.Query().Get("redirect_uri"); got != redirectURI {
		t.Fatalf("authorization redirect URI = %q, want %q", got, redirectURI)
	}
}

func TestPKCEStatusReportsPendingAndExpired(t *testing.T) {
	s := &Server{pkce: map[string]pendingPKCE{
		"pending": {Created: time.Now(), Status: "pending"},
		"expired": {Created: time.Now().Add(-11 * time.Minute), Status: "pending"},
	}}

	for _, tc := range []struct {
		state string
		want  string
	}{
		{state: "pending", want: "pending"},
		{state: "expired", want: "expired"},
	} {
		t.Run(tc.state, func(t *testing.T) {
			rr := httptest.NewRecorder()
			s.pkceStatus(rr, httptest.NewRequest(http.MethodGet, "/api/auth/status?state="+tc.state, nil))
			var response map[string]any
			if err := json.NewDecoder(rr.Body).Decode(&response); err != nil {
				t.Fatal(err)
			}
			if got := response["status"]; got != tc.want {
				t.Fatalf("status = %v, want %q", got, tc.want)
			}
		})
	}
}

// TestCallbackPKCEExchangesInBackground guards against the timeout regression
// seen on slow / ARM64 deployments: pasting the callback URL used to block the
// HTTP handler on Microsoft's token endpoint, so any fronting reverse proxy or
// browser with a read deadline reported an HTTP timeout. The exchange must run
// off the request path and the handler must return immediately.
func TestCallbackPKCEExchangesInBackground(t *testing.T) {
	// Token-endpoint traffic goes through a proxy that never answers; if the
	// handler were synchronous it would be stuck here far past the assertion
	// window.
	blocked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(5 * time.Second)
	}))
	defer blocked.Close()
	if err := outbound.Configure(blocked.URL); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = outbound.Configure("") }()

	s := &Server{pkce: map[string]pendingPKCE{
		"async-state": {Verifier: "verifier", Created: time.Now(), Status: "pending"},
	}}
	rr := httptest.NewRecorder()
	start := time.Now()
	r := httptest.NewRequest(http.MethodGet, "/api/auth/callback?state=async-state&code=invalid-code", nil)
	r.Header.Set("Accept", "*/*")
	s.callbackPKCE(rr, r)
	if latency := time.Since(start); latency >= 3*time.Second {
		t.Fatalf("callbackPKCE blocked for %v; token exchange must run in the background", latency)
	}
	if rr.Code != http.StatusOK {
		t.Fatalf("status code = %d, body = %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.Status != "exchanging" {
		t.Fatalf("status = %q, want %q", resp.Status, "exchanging")
	}
	s.mu.Lock()
	p, ok := s.pkce["async-state"]
	s.mu.Unlock()
	if !ok || p.Status != "exchanging" {
		t.Fatalf("session present=%v status=%q, want %q", ok, p.Status, "exchanging")
	}
}

// TestCallbackPKCEServesBrowserPage ensures a direct browser navigation to the
// callback endpoint (M365_REDIRECT_URI pointing at this server) gets the
// polling completion page instead of a raw JSON blob, and still returns fast.
func TestCallbackPKCEServesBrowserPage(t *testing.T) {
	blocked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(5 * time.Second)
	}))
	defer blocked.Close()
	if err := outbound.Configure(blocked.URL); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = outbound.Configure("") }()

	s := &Server{pkce: map[string]pendingPKCE{
		"browser-state": {Verifier: "verifier", Created: time.Now(), Status: "pending"},
	}}
	rr := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/auth/callback?state=browser-state&code=invalid-code", nil)
	r.Header.Set("Accept", "text/html,application/xhtml+xml")
	s.callbackPKCE(rr, r)
	if rr.Code != http.StatusOK {
		t.Fatalf("status code = %d, body = %s", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("Content-Type = %q, want text/html page for browser callbacks", ct)
	}
	if body := rr.Body.String(); !strings.Contains(body, "api/auth/status") {
		t.Fatalf("completion page must poll /api/auth/status, got: %.200s", body)
	}
}
