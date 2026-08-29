package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newAdminSessionTestServer(t *testing.T, dataDir string) *Server {
	t.Helper()
	t.Setenv("M365_DATA_DIR", dataDir)
	t.Setenv("M365_ADMIN_PASSWORD_FILE", filepath.Join(dataDir, "admin-password"))
	t.Setenv("M365_ADMIN_SESSIONS_FILE", filepath.Join(dataDir, adminSessionFileName))
	t.Setenv("M365_ADMIN_PASSWORD", "persistent-admin-password")
	t.Setenv("M365_ADMIN_PASSWORD_BOOTSTRAP_FILE", "")
	s, err := New()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func loginAdminForSessionTest(t *testing.T, s *Server) *http.Cookie {
	t.Helper()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/admin/login", strings.NewReader(`{"password":"persistent-admin-password"}`))
	request.Header.Set("Content-Type", "application/json")
	s.adminLogin(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("admin login status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	for _, cookie := range recorder.Result().Cookies() {
		if cookie.Name == "m365_admin_session" {
			return cookie
		}
	}
	t.Fatal("administrator session cookie was not returned")
	return nil
}

func requestWithAdminCookie(method, target string, cookie *http.Cookie) *http.Request {
	request := httptest.NewRequest(method, target, nil)
	request.AddCookie(cookie)
	return request
}

func TestAdminSessionSurvivesServerRestartWithoutPersistingPlaintextToken(t *testing.T) {
	dataDir := t.TempDir()
	first := newAdminSessionTestServer(t, dataDir)
	cookie := loginAdminForSessionTest(t, first)

	persisted, err := os.ReadFile(adminSessionPath())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(persisted), cookie.Value) {
		t.Fatal("administrator session token was persisted in plaintext")
	}
	if !strings.Contains(string(persisted), adminSessionDigest(cookie.Value)) {
		t.Fatal("persisted session file does not contain the SHA-256 token digest")
	}

	second := newAdminSessionTestServer(t, dataDir)
	if !second.validAdminSession(requestWithAdminCookie(http.MethodGet, "/api/admin/session", cookie)) {
		t.Fatal("unexpired administrator session was not restored after server restart")
	}
}

func TestAdminLogoutRevokesPersistedSession(t *testing.T) {
	dataDir := t.TempDir()
	s := newAdminSessionTestServer(t, dataDir)
	cookie := loginAdminForSessionTest(t, s)

	recorder := httptest.NewRecorder()
	s.adminLogout(recorder, requestWithAdminCookie(http.MethodPost, "/api/admin/logout", cookie))
	if recorder.Code != http.StatusOK {
		t.Fatalf("logout status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if s.validAdminSession(requestWithAdminCookie(http.MethodGet, "/api/admin/session", cookie)) {
		t.Fatal("logged-out session remained valid in memory")
	}

	restarted := newAdminSessionTestServer(t, dataDir)
	if restarted.validAdminSession(requestWithAdminCookie(http.MethodGet, "/api/admin/session", cookie)) {
		t.Fatal("logged-out session was restored from persistent storage")
	}
}

func TestExpiredAdminSessionsAreCleanedFromPersistentStorage(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("M365_DATA_DIR", dataDir)
	t.Setenv("M365_ADMIN_SESSIONS_FILE", filepath.Join(dataDir, adminSessionFileName))
	expiredToken := "expired-administrator-token"
	activeToken := "active-administrator-token"
	stored := persistedAdminSessions{
		Version: 1,
		Sessions: map[string]time.Time{
			adminSessionDigest(expiredToken): time.Now().Add(-time.Hour),
			adminSessionDigest(activeToken):  time.Now().Add(time.Hour),
		},
	}
	data, err := json.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(adminSessionPath(), data, 0o600); err != nil {
		t.Fatal(err)
	}

	sessions, err := loadAdminSessions(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := sessions[adminSessionDigest(expiredToken)]; ok {
		t.Fatal("expired administrator session was loaded")
	}
	if _, ok := sessions[adminSessionDigest(activeToken)]; !ok {
		t.Fatal("active administrator session was removed")
	}

	server := &Server{adminSessions: map[string]time.Time{adminSessionDigest(expiredToken): time.Now().Add(-time.Minute)}}
	if server.validAdminSession(requestWithAdminCookie(http.MethodGet, "/api/admin/session", &http.Cookie{Name: "m365_admin_session", Value: expiredToken})) {
		t.Fatal("expired administrator session validated successfully")
	}
}

func TestAdminPasswordChangeRevokesAllPersistedSessions(t *testing.T) {
	dataDir := t.TempDir()
	s := newAdminSessionTestServer(t, dataDir)
	firstCookie := loginAdminForSessionTest(t, s)
	secondCookie := loginAdminForSessionTest(t, s)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/admin/password", strings.NewReader(`{"current_password":"persistent-admin-password","new_password":"replacement-admin-password"}`))
	request.Header.Set("Content-Type", "application/json")
	request.AddCookie(firstCookie)
	s.adminChangePassword(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("password change status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if s.validAdminSession(requestWithAdminCookie(http.MethodGet, "/api/admin/session", firstCookie)) {
		t.Fatal("password-changing session remained valid")
	}
	if s.validAdminSession(requestWithAdminCookie(http.MethodGet, "/api/admin/session", secondCookie)) {
		t.Fatal("another existing session remained valid after password change")
	}

	restarted := newAdminSessionTestServer(t, dataDir)
	if restarted.validAdminSession(requestWithAdminCookie(http.MethodGet, "/api/admin/session", secondCookie)) {
		t.Fatal("revoked session returned after restart")
	}
}

func TestAdminLoginCookieMatchesConfiguredSessionLifetime(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("M365_ADMIN_SESSION_TTL", "72h")
	s := newAdminSessionTestServer(t, dataDir)
	before := time.Now()
	cookie := loginAdminForSessionTest(t, s)
	after := time.Now()

	const expectedSeconds = 72 * 60 * 60
	if cookie.MaxAge < expectedSeconds-2 || cookie.MaxAge > expectedSeconds {
		t.Fatalf("cookie MaxAge=%d, want approximately %d", cookie.MaxAge, expectedSeconds)
	}
	minimumExpiry := before.Add(72*time.Hour - 2*time.Second)
	maximumExpiry := after.Add(72*time.Hour + 2*time.Second)
	if cookie.Expires.Before(minimumExpiry) || cookie.Expires.After(maximumExpiry) {
		t.Fatalf("cookie Expires=%s, want between %s and %s", cookie.Expires, minimumExpiry, maximumExpiry)
	}

	expires, ok := s.adminSessions[adminSessionDigest(cookie.Value)]
	if !ok {
		t.Fatal("server-side administrator session was not stored")
	}
	if expires.Sub(cookie.Expires) < -time.Second || expires.Sub(cookie.Expires) > time.Second {
		t.Fatalf("server expiry %s does not match cookie expiry %s", expires, cookie.Expires)
	}
}

func TestDefaultAndConfiguredAdminSessionTTL(t *testing.T) {
	t.Setenv("M365_ADMIN_SESSION_TTL", "")
	if got := adminSessionTTL(); got != defaultAdminSessionTTL {
		t.Fatalf("default TTL=%s, want %s", got, defaultAdminSessionTTL)
	}
	t.Setenv("M365_ADMIN_SESSION_TTL", "45d")
	if got := adminSessionTTL(); got != 45*24*time.Hour {
		t.Fatalf("configured TTL=%s, want %s", got, 45*24*time.Hour)
	}
}

// TestAdminLoginSucceedsWhenSessionPersistenceFails guards the fix for
// "administrator session could not be saved; check the persistent data
// directory permissions" bouncing a correct password back as a 500 login
// failure (seen on read-only / freshly bind-mounted container data dirs).
// Session validation reads only the in-memory map, so persistence failure
// must degrade to "logged out after restart" rather than locking the
// administrator out entirely.
func TestAdminLoginSucceedsWhenSessionPersistenceFails(t *testing.T) {
	dataDir := t.TempDir()
	// Point the session store at a path whose parent collides with an existing
	// regular file, so os.MkdirAll(parent) fails and every saveAdminSessions
	// write errors. New() still loads fine (os.ReadFile on the missing path is
	// just "not found"), mirroring a temporary read-only /data in a container.
	blocker := filepath.Join(dataDir, "blocker-file")
	if err := os.WriteFile(blocker, []byte("in the way"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("M365_DATA_DIR", filepath.Join(dataDir, "data-unused"))
	t.Setenv("M365_ADMIN_PASSWORD_FILE", filepath.Join(dataDir, "admin-password"))
	t.Setenv("M365_ADMIN_SESSIONS_FILE", filepath.Join(blocker, "admin-sessions.json"))
	t.Setenv("M365_ADMIN_PASSWORD", "persistent-admin-password")
	t.Setenv("M365_ADMIN_PASSWORD_BOOTSTRAP_FILE", "")
	s, err := New()
	if err != nil {
		t.Fatal(err)
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/admin/login", strings.NewReader(`{"password":"persistent-admin-password"}`))
	request.Header.Set("Content-Type", "application/json")
	s.adminLogin(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("admin login with failing persistence: status=%d body=%s, want 200 (login must not be locked out by a write error)", recorder.Code, recorder.Body.String())
	}

	var cookie *http.Cookie
	for _, c := range recorder.Result().Cookies() {
		if c.Name == "m365_admin_session" {
			cookie = c
			break
		}
	}
	if cookie == nil {
		t.Fatal("administrator session cookie was not set despite ok login")
	}
	if !s.validAdminSession(requestWithAdminCookie(http.MethodGet, "/api/admin/session", cookie)) {
		t.Fatal("in-memory administrator session should be valid even though persistence failed")
	}
}
