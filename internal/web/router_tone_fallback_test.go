package web

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"m365-copilot2api/internal/auth"
	"m365-copilot2api/internal/chathub"

	"github.com/gorilla/websocket"
)

// The router tone-fallback: when the mapped tone returns a persistent empty
// completion on the tool-router turn, the gateway must retry the same
// routePrompt with tone=magic instead of failing the whole agent turn with 502
// ("tool router: upstream returned empty completion").

func toolDecisionFrames() []string {
	return []string{
		updateFrame("CALL_TOOL: terminal({\"command\":[\"echo\",\"hi\"]})"),
		completionFrame,
	}
}

// fakeCountedChatHubUpstream scripts frames PER CONNECTION: connection n gets
// framesFor(n). The shared fakeChatHubUpstream replays the same list on every
// connection, which cannot express "empty on the first turns, tool decision
// after the fallback" — the client ends its stream at the first type-3 frame.
// Returns the live connection counter for attempt-count assertions.
func fakeCountedChatHubUpstream(t *testing.T, framesFor func(connIdx int) []string) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	var connCount atomic.Int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(connCount.Add(1))
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if _, _, err := conn.ReadMessage(); err != nil { // SignalR handshake
			return
		}
		_ = conn.WriteMessage(websocket.TextMessage, []byte("{}"+signalRRecord))
		if _, _, err := conn.ReadMessage(); err != nil { // chat invocation
			return
		}
		for _, f := range framesFor(n) {
			if err := conn.WriteMessage(websocket.TextMessage, []byte(f+signalRRecord)); err != nil {
				return
			}
		}
		_ = conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
	}))
	t.Cleanup(ts.Close)
	return ts, &connCount
}

// pinModelTone pins the resolved upstream tone for one model through the
// settings store, overriding whatever the operator's settings.json happens to
// hold. Pass tone=="" to leave the model unmapped (modelTone default → magic).
func pinModelTone(t *testing.T, model, tone string) {
	t.Helper()
	priorMappings := openSettingsStore().v.ModelMappings
	priorUpstream := openSettingsStore().v.UpstreamMappings
	openSettingsStore().mu.Lock()
	if tone == "" {
		openSettingsStore().v.ModelMappings = nil
	} else {
		openSettingsStore().v.ModelMappings = []modelMapping{
			{PublicModel: model, UpstreamMapping: "RouterTestMapping", DisplayName: model, DefaultReasoningLevel: "medium"},
		}
		openSettingsStore().v.UpstreamMappings = []upstreamMapping{
			{Name: "RouterTestMapping", Tone: tone},
		}
	}
	openSettingsStore().mu.Unlock()
	t.Cleanup(func() {
		openSettingsStore().mu.Lock()
		openSettingsStore().v.ModelMappings = priorMappings
		openSettingsStore().v.UpstreamMappings = priorUpstream
		openSettingsStore().mu.Unlock()
	})
}

func newRouterFallbackTestServer(t *testing.T, emptyTurns int) (*Server, *atomic.Int64) {
	t.Helper()
	// Script per connection: the first emptyTurns chat invocations complete
	// with no text (router turn + its central same-tone retry), then the next
	// connection — the magic-tone fallback — answers with a tool decision.
	up, conns := fakeCountedChatHubUpstream(t, func(connIdx int) []string {
		if connIdx <= emptyTurns {
			return []string{completionFrame}
		}
		return toolDecisionFrames()
	})
	dir := t.TempDir()
	t.Setenv("M365_SESSION_CACHE", filepath.Join(dir, "sessions.json"))
	t.Setenv("M365_CONVERSATION_SESSION_CACHE", filepath.Join(dir, "conversation-sessions.json"))
	t.Setenv("M365_CONVERSATION_CACHE", filepath.Join(dir, "conversations.json"))
	t.Setenv("M365_USER_SESSION_CACHE", filepath.Join(dir, "users.json"))
	t.Setenv("M365_SETTINGS_FILE", filepath.Join(dir, "settings.json"))
	t.Setenv("M365_ACCOUNT_SETTINGS_FILE", filepath.Join(dir, "account-settings.json"))
	ws := strings.Replace(up.URL, "http://", "ws://", 1) + "/m365Copilot/Chathub"
	t.Setenv("M365_CHATHUB_WS_BASE", ws)

	store, err := auth.OpenStore(dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, err := store.Upsert(auth.TokenSet{
		HomeOID:     "acc-router-fb-1",
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
	c.HTTPHeader.Set("User-Agent", "router-fallback-test")
	s := &Server{
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
	return s, conns
}

const routerTestTools = `"tools":[{"type":"function","function":{"name":"terminal","description":"run a command","parameters":{"type":"object","properties":{"command":{"type":"array","items":{"type":"string"}}},"required":["command"]}}}]`

// TestToolRouterEmptyCompletionFallsBackToMagic reproduces the production
// failure: the mapped tone (Gpt_5_6_Reasoning here, as on the luna route)
// returns empty completions on the router turn. chatWithAccount retries the
// same tone once (empty connection 2), so the fallback sees connection 3 with
// tone=magic and must recover with the scripted tool decision instead of
// surfacing a 502.
func TestToolRouterEmptyCompletionFallsBackToMagic(t *testing.T) {
	pinModelTone(t, "gpt-5.6-luna", "Gpt_5_6_Reasoning")
	s, _ := newRouterFallbackTestServer(t, 2)
	body := `{"model":"gpt-5.6-luna","stream":true,"messages":[{"role":"user","content":"list files"}],` + routerTestTools + `,"tool_choice":"auto"}`
	w := streamChatRequest(t, s, body)
	raw := w.Body.String()
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, raw[:min(len(raw), 400)])
	}
	if strings.Contains(raw, "tool router") || strings.Contains(raw, "empty completion") {
		t.Fatalf("tone fallback did not recover, body=%s", raw[:min(len(raw), 500)])
	}
	if !strings.Contains(raw, "terminal") {
		t.Fatalf("expected tool call in response, body=%s", raw[:min(len(raw), 500)])
	}
}

// TestToolRouterMagicToneDoesNotRetryFallback verifies a request whose tone is
// already magic (no model mapping → modelTone default) does NOT trigger the
// fallback: the router turn gets the central same-request retry only (2 upstream
// connections) and then fails, instead of looping on the recovery tone itself.
func TestToolRouterMagicToneDoesNotRetryFallback(t *testing.T) {
	pinModelTone(t, "test-magic-model", "")
	s, conns := newRouterFallbackTestServer(t, 5)
	body := `{"model":"test-magic-model","stream":true,"messages":[{"role":"user","content":"list files"}],` + routerTestTools + `,"tool_choice":"auto"}`
	w := streamChatRequest(t, s, body)
	if w.Code != 502 {
		t.Fatalf("status=%d, want 502", w.Code)
	}
	// Exactly the router turn + chatWithAccount's central retry; a third
	// connection would mean the fallback fired on the magic tone itself.
	if got := conns.Load(); got != 2 {
		t.Fatalf("upstream connections=%d, want 2 (no magic-tone retry on magic)", got)
	}
}
