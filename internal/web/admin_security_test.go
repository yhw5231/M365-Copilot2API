package web

import (
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func adminTestClient(t *testing.T, h http.Handler) (*httptest.Server, *http.Client) {
	t.Helper()
	ts := httptest.NewTLSServer(h)
	jar, _ := cookiejar.New(nil)
	c := ts.Client()
	c.Jar = jar
	t.Cleanup(ts.Close)
	return ts, c
}

func postJSON(t *testing.T, c *http.Client, url, body string) *http.Response {
	t.Helper()
	r, err := c.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestDefaultPasswordWhenNothingConfigured(t *testing.T) {
	t.Setenv("M365_ADMIN_PASSWORD", "")
	t.Setenv("M365_ADMIN_PASSWORD_FILE", t.TempDir()+"/admin-password")
	t.Setenv("M365_ADMIN_PASSWORD_BOOTSTRAP_FILE", "")
	got, mustChange := loadAdminPassword()
	if mustChange != true {
		t.Fatalf("mustChange=%v, want true for the default password", mustChange)
	}
	if got != defaultAdminPassword {
		t.Fatalf("default admin password=%q, want %q", got, defaultAdminPassword)
	}
}

func TestDefaultPasswordForcesChangeAndRotatesSessions(t *testing.T) {
	// A leftover persisted file holding the legacy default (e.g. a clone of a
	// previously initialized data directory) must still force a change and
	// rotate sessions after the change.
	path := t.TempDir() + "/admin-password"
	if err := os.WriteFile(path, []byte("admin123\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("M365_ADMIN_PASSWORD", "")
	t.Setenv("M365_ADMIN_PASSWORD_FILE", path)
	s, err := New()
	if err != nil {
		t.Fatal(err)
	}
	ts, c := adminTestClient(t, s.Routes())

	r := postJSON(t, c, ts.URL+"/api/admin/login", `{"password":"admin123"}`)
	if r.StatusCode != 200 {
		t.Fatalf("login=%d", r.StatusCode)
	}
	var login map[string]any
	_ = json.NewDecoder(r.Body).Decode(&login)
	r.Body.Close()
	if login["must_change_password"] != true {
		t.Fatalf("login=%#v", login)
	}

	r, _ = c.Get(ts.URL + "/api/accounts")
	r.Body.Close()
	if r.StatusCode != http.StatusForbidden {
		t.Fatalf("protected status=%d", r.StatusCode)
	}

	r = postJSON(t, c, ts.URL+"/api/admin/change-password", `{"current_password":"admin123","new_password":"a-new-password-123"}`)
	if r.StatusCode != 200 {
		t.Fatalf("change=%d", r.StatusCode)
	}
	r.Body.Close()

	r, _ = c.Get(ts.URL + "/api/accounts")
	r.Body.Close()
	if r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("old session status=%d", r.StatusCode)
	}

	r = postJSON(t, c, ts.URL+"/api/admin/login", `{"password":"a-new-password-123"}`)
	r.Body.Close()
	if r.StatusCode != 200 {
		t.Fatalf("new login=%d", r.StatusCode)
	}
	r, _ = c.Get(ts.URL + "/api/accounts")
	r.Body.Close()
	if r.StatusCode != 200 {
		t.Fatalf("new session status=%d", r.StatusCode)
	}
}

func TestAdminLoginLocksAfterFiveFailures(t *testing.T) {
	t.Setenv("M365_ADMIN_PASSWORD", "correct-password")
	s, err := New()
	if err != nil {
		t.Fatal(err)
	}
	ts, c := adminTestClient(t, s.Routes())
	for i := 0; i < 5; i++ {
		r := postJSON(t, c, ts.URL+"/api/admin/login", `{"password":"wrong"}`)
		r.Body.Close()
		if r.StatusCode != 401 {
			t.Fatalf("attempt %d=%d", i+1, r.StatusCode)
		}
	}
	r := postJSON(t, c, ts.URL+"/api/admin/login", `{"password":"correct-password"}`)
	defer r.Body.Close()
	if r.StatusCode != 429 || r.Header.Get("Retry-After") == "" {
		t.Fatalf("locked=%d retry=%q", r.StatusCode, r.Header.Get("Retry-After"))
	}
}

func TestPersistedPasswordOverridesBootstrapEnv(t *testing.T) {
	path := t.TempDir() + "/admin-password"
	t.Setenv("M365_ADMIN_PASSWORD_FILE", path)
	t.Setenv("M365_ADMIN_PASSWORD", "old-bootstrap-password")
	if err := saveAdminPassword("persisted-new-password"); err != nil {
		t.Fatal(err)
	}
	got, mustChange := loadAdminPassword()
	if got != "persisted-new-password" || mustChange {
		t.Fatalf("got=%q mustChange=%v", got, mustChange)
	}
}

func TestExpiredLoginWindowResets(t *testing.T) {
	s := &Server{loginAttempts: map[string]loginAttempt{"x": {Failures: 4, WindowStart: time.Now().Add(-16 * time.Minute)}}}
	if ok, _ := s.loginAllowed("x", time.Now()); !ok {
		t.Fatal("expired window remained locked")
	}
}

func chdirRepoRoot(t *testing.T) {
	t.Helper()
	original, _ := os.Getwd()
	var root string
	dir, _ := os.Getwd()
	for {
		if _, err := os.Stat(filepath.Join(dir, "web", "index.html")); err == nil {
			root = dir
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Skip("repo root with web/ not found")
		}
		dir = parent
	}
	t.Cleanup(func() { _ = os.Chdir(original) })
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
}

func TestRootPageServesLoginPageForLoginPath(t *testing.T) {
	// Regression: /login must serve web/login.html (the forced-password-change
	// flow), not the dashboard shell. Previously rootPage always served
	// index.html, so users stuck on the default password hit 403
	// password_change_required on every save with no way to change it.
	chdirRepoRoot(t)
	s := &Server{}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/login", nil)
	s.rootPage(w, r)
	if w.Code != 200 {
		t.Fatalf("/login status=%d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "请立即修改默认密码") && !strings.Contains(body, "Change the default password now") {
		t.Fatal("/login did not serve login.html (missing forced change UI)")
	}
	if strings.Contains(body, "pageTitle") {
		t.Fatal("/login served index.html instead of login.html")
	}
}

func TestRootPageServesIndexForRootPath(t *testing.T) {
	chdirRepoRoot(t)
	s := &Server{}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	s.rootPage(w, r)
	if w.Code != 200 {
		t.Fatalf("/ status=%d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "pageTitle") {
		t.Fatal("/ did not serve index.html dashboard shell")
	}
}

func TestClientIPIgnoresXFFFromNonLoopbackPeerByDefault(t *testing.T) {
	t.Setenv("TRUST_PROXY_HEADERS", "")
	t.Setenv("M365_TRUST_PROXY_HEADERS", "")
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "172.18.0.5:54321"
	r.Header.Set("X-Forwarded-For", "203.0.113.7")
	if got := clientIP(r); got != "172.18.0.5" {
		t.Fatalf("clientIP=%q, want the direct peer without the flag", got)
	}
}

func TestClientIPTrustsXFFFromNonLoopbackPeerWithFlag(t *testing.T) {
	t.Setenv("TRUST_PROXY_HEADERS", "true")
	t.Setenv("M365_TRUST_PROXY_HEADERS", "")
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "172.18.0.5:54321"
	r.Header.Set("X-Forwarded-For", "203.0.113.7, 198.51.100.3")
	if got := clientIP(r); got != "198.51.100.3" {
		t.Fatalf("clientIP=%q, want the right-most valid XFF entry", got)
	}
}

func TestClientIPAcceptsM365AliasAndTruthyValues(t *testing.T) {
	for _, v := range []string{"1", "TRUE", "yes", "on"} {
		t.Setenv("TRUST_PROXY_HEADERS", "")
		t.Setenv("M365_TRUST_PROXY_HEADERS", v)
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = "172.18.0.5:54321"
		r.Header.Set("X-Forwarded-For", "203.0.113.9")
		if got := clientIP(r); got != "203.0.113.9" {
			t.Fatalf("value %q: clientIP=%q", v, got)
		}
	}
}

func TestClientIPWithFlagSkipsMalformedXFF(t *testing.T) {
	t.Setenv("TRUST_PROXY_HEADERS", "true")
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "172.18.0.5:54321"
	r.Header.Set("X-Forwarded-For", "not-an-ip, 198.51.100.3, garbage")
	if got := clientIP(r); got != "198.51.100.3" {
		t.Fatalf("clientIP=%q, want the right-most parseable XFF entry", got)
	}
}

func TestClientIPWithoutXFFFallsBackToDirectPeer(t *testing.T) {
	t.Setenv("TRUST_PROXY_HEADERS", "true")
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "172.18.0.5:54321"
	if got := clientIP(r); got != "172.18.0.5" {
		t.Fatalf("clientIP=%q, want RemoteAddr host when no XFF is present", got)
	}
}

func TestSecureAdminCookieTrustsForwardedProtoWithFlag(t *testing.T) {
	t.Setenv("TRUST_PROXY_HEADERS", "true")
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "172.18.0.5:54321"
	r.Header.Set("X-Forwarded-Proto", "https")
	if !secureAdminCookie(r) {
		t.Fatal("secureAdminCookie=false, want Secure cookies behind a trusted proxy")
	}
	t.Setenv("TRUST_PROXY_HEADERS", "")
	r2 := httptest.NewRequest(http.MethodGet, "/", nil)
	r2.RemoteAddr = "172.18.0.5:54321"
	r2.Header.Set("X-Forwarded-Proto", "https")
	if secureAdminCookie(r2) {
		t.Fatal("secureAdminCookie=true from a non-loopback peer without the flag")
	}
}

func TestClientIPFingerprintUsesRealClientIPBehindProxy(t *testing.T) {
	t.Setenv("TRUST_PROXY_HEADERS", "true")
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "172.18.0.5:54321"
	r.Header.Set("User-Agent", "ua/1")
	r.Header.Set("X-Forwarded-For", "203.0.113.7")
	f1 := clientIPFingerprint(r)
	r.Header.Set("X-Forwarded-For", "203.0.113.8")
	f2 := clientIPFingerprint(r)
	if f1 == f2 {
		t.Fatal("fingerprint did not change when the real client IP changed behind the proxy")
	}
	// Same client IP from the same proxy must keep a stable fingerprint.
	f3 := clientIPFingerprint(r)
	if f2 != f3 {
		t.Fatal("fingerprint changed for an unchanged client IP")
	}
}
