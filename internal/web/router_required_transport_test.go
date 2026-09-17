package web

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"m365-copilot2api/internal/auth"
	"m365-copilot2api/internal/chathub"

	"github.com/gorilla/websocket"
)

// TestToolRouterRequiredTransportFailureFallsThrough reproduces the cloud
// failure report: /v1/responses with tool_choice=required on a NEW session
// where the router probe's WebSocket dial/read dies on the wire
// ("tool router: ws dial: context deadline exceeded"). The transport-class
// failure means the model never produced any decision — so the required round
// must NOT hard-fail with 502 ("tool router: ..."). It degrades into the
// answer stream (which carries the same tool definitions) and delivers the
// tool call the client asked for.
func TestToolRouterRequiredTransportFailureFallsThrough(t *testing.T) {
	pinModelTone(t, "gpt-5.6-luna", "") // unmapped → magic tone, non-reasoning answer path
	// Connections 1-2: the router probe + its central same-account transport
	// retry both fail on the wire (frames list closed abruptly before any
	// completion frame → "ws read before completion", a transport failure).
	// Connection 3: the answer stream succeeds with the tool decision.
	up, conns := fakeCountedChatHubUpstream(t, func(connIdx int) []string {
		if connIdx <= 2 {
			return nil // no frames, then the server closes → transport error
		}
		return toolDecisionFrames()
	})
	s := newRequiredTransportTestServer(t, up.URL)

	body := `{"model":"gpt-5.6-luna","stream":true,"messages":[{"role":"user","content":"classify"}],` + routerTestTools + `,"tool_choice":"required"}`
	w := streamChatRequest(t, s, body)
	raw := w.Body.String()
	if w.Code != 200 {
		t.Fatalf("status=%d, want 200 (required + transport router failure must fall through to the answer stream); body=%s", w.Code, raw[:min(len(raw), 500)])
	}
	if strings.Contains(raw, "tool router") || strings.Contains(raw, "context deadline exceeded") {
		t.Fatalf("required round hard-failed on a transport failure, body=%s", raw[:min(len(raw), 500)])
	}
	if !strings.Contains(raw, "terminal") {
		t.Fatalf("expected the tool call from the answer stream, body=%s", raw[:min(len(raw), 500)])
	}
	// Probe (1) + probe transport retry (2) + answer stream (3); a 4th
	// connection would mean the required chain wrongly re-probed.
	if got := conns.Load(); got != 3 {
		t.Fatalf("upstream connections=%d, want 3", got)
	}
}

func newRequiredTransportTestServer(t *testing.T, wsBase string) *Server {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("M365_SESSION_CACHE", filepath.Join(dir, "sessions.json"))
	t.Setenv("M365_CONVERSATION_SESSION_CACHE", filepath.Join(dir, "conversation-sessions.json"))
	t.Setenv("M365_CONVERSATION_CACHE", filepath.Join(dir, "conversations.json"))
	t.Setenv("M365_USER_SESSION_CACHE", filepath.Join(dir, "users.json"))
	t.Setenv("M365_SETTINGS_FILE", filepath.Join(dir, "settings.json"))
	t.Setenv("M365_ACCOUNT_SETTINGS_FILE", filepath.Join(dir, "account-settings.json"))
	ws := strings.Replace(wsBase, "http://", "ws://", 1) + "/m365Copilot/Chathub"
	t.Setenv("M365_CHATHUB_WS_BASE", ws)

	store, err := auth.OpenStore(dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, err := store.Upsert(auth.TokenSet{
		HomeOID:     "acc-router-transport-1",
		TenantID:    "tid-1",
		Email:       "test@example.com",
		AccessToken: "tok",
		ExpiresAt:   time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("upsert account: %v", err)
	}
	c := &chathub.Client{
		HTTPHeader: make(http.Header),
		HTTPClient: &http.Client{Timeout: 60 * time.Second},
		Dialer:     &websocket.Dialer{HandshakeTimeout: 15 * time.Second},
		Pool:       chathub.NewConnPool(&websocket.Dialer{HandshakeTimeout: 15 * time.Second}, make(http.Header)),
	}
	c.HTTPHeader.Set("Origin", "https://m365.cloud.microsoft")
	c.HTTPHeader.Set("User-Agent", "required-transport-test")
	return &Server{
		tokens:              store,
		accountPool:         newAccountHealth(),
		accountConcurrency:  newAccountConcurrency(),
		gatewayConcurrency:  newGatewayConcurrency(),
		sessionConcurrency:  newSessionConcurrency(),
		chat:                c,
		sessions:            openSessionStore(),
		userSessions:        openUserSessionStore(30 * time.Minute),
		sessionResolver:     openSessionResolver(),
		conversationManager: openConversationManager(),
		settings:            openSettingsStore(),
		responseMessages:    map[string]map[string]respHistory{},
		usage:               &usageLog{persist: &persistStore{flush: func() error { return nil }}},
	}
}
