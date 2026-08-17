package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"m365-copilot2api/internal/chathub"
)

// newTestSessionResolver returns an isolated resolver backed by a temp file so
// tests never touch the repository's default data directory.
func newTestSessionResolver(t *testing.T) *sessionResolver {
	t.Helper()
	t.Setenv("M365_SESSION_CACHE", filepath.Join(t.TempDir(), "sessions.json"))
	sr := openSessionResolver()
	t.Cleanup(func() { _ = sr.persist.flushNowBlocking() })
	return sr
}

// TestSessionsListOmitsContextHistory is the regression test for the session
// leak: GET /v1/sessions must return a sanitized summary and never serialize
// contextHistory (the full conversation payload).
func TestSessionsListOmitsContextHistory(t *testing.T) {
	sr := newTestSessionResolver(t)
	now := time.Now()
	sr.mu.Lock()
	sr.sessions["sess-1"] = sessionBinding{
		SessionID:  "sess-1",
		ConversationID: "conv-1",
		AccountID:      "acc-1",
		CreatedAt:      now,
		LastUsedAt:     now,
		ContextHistory: []oaiMsg{{Role: "user", Content: "TOP-SECRET-CONTENT"}},
	}
	sr.mu.Unlock()

	srv := &Server{sessionResolver: sr}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/sessions", nil)
	srv.handleSessions(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, "contextHistory") || strings.Contains(body, "TOP-SECRET-CONTENT") {
		t.Fatalf("/v1/sessions leaked conversation contents: %s", body)
	}
	var out struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Data) != 1 {
		t.Fatalf("data=%d entries, want 1", len(out.Data))
	}
	if out.Data[0]["id"] != "sess-1" {
		t.Fatalf("summary id missing: %#v", out.Data[0])
	}
	if out.Data[0]["message_count"] != float64(1) {
		t.Fatalf("message_count=%v, want 1", out.Data[0]["message_count"])
	}
	if out.Data[0]["conversation_id"] != "conv-1" {
		t.Fatalf("conversation_id=%v", out.Data[0]["conversation_id"])
	}
}

// TestConversationTitleStripsControlChars guards the back-end half of the
// stored-XSS fix: titles derived from user content must not contain control
// characters (which break DOM text nodes / log lines) and must stay
// length-bounded.
func TestConversationTitleStripsControlChars(t *testing.T) {
	title := conversationTitle([]oaiMsg{{Role: "user", Content: "a\u0000<script>b</script>\nline2"}})
	if strings.ContainsAny(title, "\x00\x01\x02\x03\x04\x05\x06\x07\x08\x0b\x0c\x0e\x0f") {
		t.Fatalf("control characters survived sanitization: %q", title)
	}
	if strings.Contains(title, "\n") {
		t.Fatalf("newline survived sanitization: %q", title)
	}
	long := conversationTitle([]oaiMsg{{Role: "user", Content: strings.Repeat("漢", 100)}})
	if len([]rune(long)) > 41 {
		t.Fatalf("title not length-bounded: %q", long)
	}
}

// TestSanitizeChatNameLimitsRemoteNames checks cloud-supplied chat names get
// the same control-character and length treatment at the API boundary.
func TestSanitizeChatNameLimitsRemoteNames(t *testing.T) {
	got := sanitizeChatName("evil\u0001name<script>")
	if strings.ContainsAny(got, "\x01") {
		t.Fatalf("control char survived: %q", got)
	}
	got = sanitizeChatName(strings.Repeat("x", 200))
	if len([]rune(got)) > 81 {
		t.Fatalf("remote name not capped: %q", got)
	}
}

// TestSSRFValidationRejectsLocalTargets asserts the shared URL gate refuses
// loopback, cloud-metadata, private, link-local and CGNAT destinations and
// non-https schemes, while accepting a public literal IP.
func TestSSRFValidationRejectsLocalTargets(t *testing.T) {
	rejected := []string{
		"https://127.0.0.1/x",
		"https://169.254.169.254/latest/meta-data",
		"https://10.1.2.3/x",
		"https://192.168.1.1/x",
		"https://100.64.0.1/x", // CGNAT
		"https://[::1]/x",
		"http://example.com/x", // no https
		"ftp://example.com/x",
		"https:///no-host",
	}
	for _, u := range rejected {
		if err := chathub.ValidateRemoteDownloadURL(u); err == nil {
			t.Errorf("ValidateRemoteDownloadURL(%q) accepted a forbidden target", u)
		}
	}
	if err := chathub.ValidateRemoteDownloadURL("https://8.8.8.8/x"); err != nil {
		t.Errorf("public literal IP rejected: %v", err)
	}
}

// TestImageDownloadRefusesLocalHost is the web-layer entry check: the fetch
// path must fail before any network I/O for unsafe destinations.
func TestImageDownloadRefusesLocalHost(t *testing.T) {
	if _, _, err := downloadImageAsBase64WithToken("https://127.0.0.1/x", ""); err == nil {
		t.Fatal("downloadImageAsBase64WithToken accepted 127.0.0.1")
	}
	if _, _, err := downloadImageAsBase64WithToken("https://169.254.169.254/latest/meta-data", "tok"); err == nil {
		t.Fatal("downloadImageAsBase64WithToken accepted metadata address")
	}
}

// TestPKCEStartRateLimit verifies /api/auth/start is limited per client and
// the window resets afterwards.
func TestPKCEStartRateLimit(t *testing.T) {
	s := &Server{pkce: map[string]pendingPKCE{}, pkceStarts: map[string][]time.Time{}}
	ip := "1.2.3.4"
	now := time.Now()
	for i := 0; i < pkceStartLimit; i++ {
		if !s.pkceStartAllowedLocked(ip, now) {
			t.Fatalf("attempt %d denied before limit", i+1)
		}
	}
	if s.pkceStartAllowedLocked(ip, now) {
		t.Fatal("rate limit not enforced")
	}
	if !s.pkceStartAllowedLocked(ip, now.Add(pkceStartWindow+time.Second)) {
		t.Fatal("rate limit window did not reset")
	}
}

// TestPKCEStatePoolEvictsOldest verifies the global state pool never exceeds
// maxPKCEStates: the oldest entry is evicted when the pool is full.
func TestPKCEStatePoolEvictsOldest(t *testing.T) {
	s := &Server{pkce: map[string]pendingPKCE{}, pkceStarts: map[string][]time.Time{}}
	now := time.Now()
	// All states stay inside the TTL (10 min) so prunePKCELocked does not
	// interfere; ages spread over ~8.5 minutes via half-second steps.
	for i := 0; i < maxPKCEStates; i++ {
		s.pkce[fmt.Sprintf("state-%04d", i)] = pendingPKCE{Created: now.Add(-time.Duration(i*500) * time.Millisecond)}
	}
	// 最老的状态是 Created 最早的那个：i 越大越老，state-(maxPKCEStates-1)
	// 是池中最老的条目，满容量时应被驱逐。
	oldest := fmt.Sprintf("state-%04d", maxPKCEStates-1)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/auth/start", nil)
	s.startPKCE(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("startPKCE status=%d body=%s", w.Code, w.Body.String())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pkce) != maxPKCEStates {
		t.Fatalf("pool size=%d, want %d", len(s.pkce), maxPKCEStates)
	}
	if _, ok := s.pkce[oldest]; ok {
		t.Fatal("oldest PKCE state was not evicted at capacity")
	}
}

// TestPersistListRegistersStoreOnce guards the persistList growth bug: a
// store marked dirty many times must be registered exactly once.
func TestPersistListRegistersStoreOnce(t *testing.T) {
	p := &persistStore{flush: func() error { return nil }}
	persistMu.Lock()
	before := len(persistList)
	persistMu.Unlock()
	for i := 0; i < 5; i++ {
		p.markDirty()
	}
	persistMu.Lock()
	after := len(persistList)
	persistMu.Unlock()
	if after != before+1 {
		t.Fatalf("persistList grew by %d entries, want exactly 1", after-before)
	}
}