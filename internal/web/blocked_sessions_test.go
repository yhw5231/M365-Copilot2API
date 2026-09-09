package web

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"m365-copilot2api/internal/auth"
)

func newBlockedSessionsTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("M365_BLOCKED_SESSIONS", filepath.Join(dir, "blockSessions.json"))
	t.Setenv("M365_SESSION_CACHE", filepath.Join(dir, "sessions.json"))
	store, err := auth.OpenStore(filepath.Join(dir, "tokens"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	s := &Server{
		tokens:              store,
		accountPool:         newAccountHealth(),
		accountConcurrency:  newAccountConcurrency(),
		gatewayConcurrency:  newGatewayConcurrency(),
		sessionConcurrency:  newSessionConcurrency(),
		sessions:            openSessionStore(),
		sessionResolver:     openSessionResolver(),
		conversationManager: openConversationManager(),
		settings:            openSettingsStore(),
		blockedSessions:     openBlockedSessions(),
		responseMessages:    map[string]map[string]respHistory{},
		usage:               &usageLog{persist: &persistStore{flush: func() error { return nil }}},
	}
	return s, dir
}

// TestBlockedSessionsPersistAndExpire covers the block lifecycle: a blocked id
// rejects, survives a reload (restart bypass must not work), and expires after
// the TTL.
func TestBlockedSessionsPersistAndExpire(t *testing.T) {
	s, dir := newBlockedSessionsTestServer(t)
	path := filepath.Join(dir, "blockSessions.json")

	s.blockedSessions.Block("sess-1", "content filter hit")
	if !s.blockedSessions.IsBlocked("sess-1") {
		t.Fatal("session must be blocked right after Block")
	}
	if s.blockedSessions.IsBlocked("sess-2") {
		t.Fatal("unrelated session must not be blocked")
	}
	// Flush the async persist queue, then verify the store file (a restart
	// bypass must not work).
	if err := s.blockedSessions.persist.flushNowBlocking(); err != nil {
		t.Fatalf("flush blocked sessions: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("blocked-sessions store file missing after flush: %v", err)
	}
	// Persistence: a fresh store over the same file (simulated restart) still
	// reports the block.
	reloaded := &blockedSessions{path: path, ttl: 24 * time.Hour, items: map[string]blockedSessionRecord{}}
	reloaded.load()
	if !reloaded.IsBlocked("sess-1") {
		t.Fatal("block must survive a restart (persisted store)")
	}
	// Expiry: a store with a 1ns TTL drops the entry on check.
	expired := &blockedSessions{path: path, ttl: time.Nanosecond, items: map[string]blockedSessionRecord{}}
	expired.load()
	if expired.IsBlocked("sess-1") {
		t.Fatal("block must expire after the TTL")
	}
}

// TestOpenaiChatRejectsBlockedSession drives the real openaiChat entry: a
// request whose explicit session id is blocked must fail with 403 BEFORE any
// upstream work, and must stay blocked on the next request.
func TestOpenaiChatRejectsBlockedSession(t *testing.T) {
	s, _ := newBlockedSessionsTestServer(t)
	s.blockedSessions.Block("blocked-sess-xyz", "content filter hit")

	makeReq := func() *http.Request {
		body := `{"model":"gpt-5.6-luna","messages":[{"role":"user","content":"hi"}],"session_key":"blocked-sess-xyz"}`
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer sk-test")
		return req
	}
	w := httptest.NewRecorder()
	s.openaiChat(w, makeReq())
	if w.Code != http.StatusForbidden {
		t.Fatalf("blocked session must be rejected with 403, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "content_policy_blocked") {
		t.Fatalf("rejection must name the content_policy_blocked code: %s", w.Body.String())
	}
	// The next request on the same session is rejected identically — the block
	// is not a one-shot drop.
	w2 := httptest.NewRecorder()
	s.openaiChat(w2, makeReq())
	if w2.Code != http.StatusForbidden {
		t.Fatalf("second request on a blocked session must also be 403, got %d", w2.Code)
	}
}

// TestOpenaiChatAllowsUnblockedSession: an explicit session that is NOT
// blocked must proceed past the gate (it will fail later on the missing
// account/model, but not with the content-policy 403).
func TestOpenaiChatAllowsUnblockedSession(t *testing.T) {
	s, _ := newBlockedSessionsTestServer(t)
	body := `{"model":"gpt-5.6-luna","messages":[{"role":"user","content":"hi"}],"session_key":"clean-sess-abc"}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-test")
	w := httptest.NewRecorder()
	s.openaiChat(w, req)
	if w.Code == http.StatusForbidden && strings.Contains(w.Body.String(), "content_policy_blocked") {
		t.Fatalf("unblocked session must not hit the content-policy gate: %d %s", w.Code, w.Body.String())
	}
}

// TestIdentityConcealmentInjection verifies the system policy is appended as a
// service-injected message (never part of session history) and honors the
// env kill switch.
func TestIdentityConcealmentInjection(t *testing.T) {
	msgs := []oaiMsg{{Role: "user", Content: "hi"}}
	out := injectIdentityConcealment(msgs)
	if len(out) != 2 {
		t.Fatalf("expected the policy message appended, got %d messages", len(out))
	}
	if out[1].Role != "system" || !out[1].ServiceInjected {
		t.Fatalf("policy must be a service-injected system message: %+v", out[1])
	}
	if content, _ := out[1].Content.(string); !strings.Contains(content, identityRevealToken()) {
		t.Fatalf("policy text missing the identity reveal token: %q", content)
	}
	t.Setenv("M365_IDENTITY_CONCEALMENT", "0")
	if out2 := injectIdentityConcealment(msgs); len(out2) != 1 {
		t.Fatalf("kill switch must disable the injection, got %d messages", len(out2))
	}
}

// TestSessionBlockTTLFromEnv pins the TTL override parsing.
func TestSessionBlockTTLFromEnv(t *testing.T) {
	t.Setenv("M365_SESSION_BLOCK_TTL_MINUTES", "30")
	if got := sessionBlockTTL(); got != 30*time.Minute {
		t.Fatalf("ttl = %v, want 30m", got)
	}
	t.Setenv("M365_SESSION_BLOCK_TTL_MINUTES", "bogus")
	if got := sessionBlockTTL(); got != defaultSessionBlockTTL {
		t.Fatalf("invalid ttl must fall back to default, got %v", got)
	}
	os.Unsetenv("M365_SESSION_BLOCK_TTL_MINUTES")
}
