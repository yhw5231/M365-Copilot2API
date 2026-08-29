package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestGoalStateEndpointRoundTrip exercises the /v1/goal read/update entry:
// GET returns the ledger, POST complete closes it, subsequent GET reflects it.
func TestGoalStateEndpointRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("M365_SESSION_CACHE", filepath.Join(dir, "sessions.json"))
	t.Setenv("M365_CONVERSATION_CACHE", filepath.Join(dir, "conversations.json"))
	t.Setenv("M365_USER_SESSION_CACHE", filepath.Join(dir, "users.json"))

	store := newAPIKeyStore(filepath.Join(dir, "api-keys.json"))
	validRaw := "sk-goal-test-key"
	store.Keys = []apiKeyRecord{{
		ID:        "key-goal",
		Name:      "goal test",
		Prefix:    "sk-goal",
		Hash:      keyHash(validRaw),
		CreatedAt: time.Now(),
	}}
	s := &Server{
		sessionResolver:     openSessionResolver(),
		apiKeys:             store,
		sessions:            openSessionStore(),
		userSessions:        openUserSessionStore(30 * time.Minute),
		conversationManager: openConversationManager(),
	}
	routes := s.Routes()

	// Bind a session with a task ledger.
	sr := s.sessionResolver
	task := buildTaskLedger(&oaiReq{Messages: []oaiMsg{{Role: "user", Content: "Refactor auth module"}}})
	task.GoalID = "goal-abc123"
	body := &oaiReq{Messages: []oaiMsg{{Role: "user", Content: "Refactor auth module"}}}
	sr.BindWithTask("sess-goal-1", "conv-1", "acc-1", body, "", httptest.NewRequest("POST", "/v1/chat/completions", nil), task)

	do := func(method, action, goalID string) *httptest.ResponseRecorder {
		var payload string
		if method == http.MethodPost {
			var m = map[string]any{"action": action}
			if goalID != "" {
				m["goal_id"] = goalID
			}
			b, _ := json.Marshal(m)
			payload = string(b)
		}
		req := httptest.NewRequest(method, "/v1/goal", strings.NewReader(payload))
		req.Header.Set("Authorization", "Bearer "+validRaw)
		req.Header.Set(sessionHeaderName, "sess-goal-1")
		rr := httptest.NewRecorder()
		routes.ServeHTTP(rr, req)
		return rr
	}

	rr := do(http.MethodGet, "", "")
	if rr.Code != 200 {
		t.Fatalf("GET /v1/goal status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if got["status"] != "active" || got["goal_id"] != "goal-abc123" {
		t.Fatalf("GET returned unexpected state: %#v", got)
	}

	rr = do(http.MethodPost, "complete", "goal-abc123")
	if rr.Code != 200 {
		t.Fatalf("POST /v1/goal status=%d body=%s", rr.Code, rr.Body.String())
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("bad json after complete: %v", err)
	}
	if got["status"] != "complete" {
		t.Fatalf("POST complete did not close: %#v", got)
	}

	// The persisted binding must show complete now.
	b, ok := sr.GetSession("sess-goal-1")
	if !ok || b.Task == nil || !b.Task.IsComplete() {
		t.Fatalf("binding task not complete: %#v ok=%t", b.Task, ok)
	}

	// GET reflects the completion.
	rr = do(http.MethodGet, "", "")
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"status":"complete"`) {
		t.Fatalf("GET after complete: status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestGoalStateEndpointRejectsMismatch(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("M365_SESSION_CACHE", filepath.Join(dir, "sessions.json"))
	t.Setenv("M365_CONVERSATION_CACHE", filepath.Join(dir, "conversations.json"))
	t.Setenv("M365_USER_SESSION_CACHE", filepath.Join(dir, "users.json"))

	store := newAPIKeyStore(filepath.Join(dir, "api-keys.json"))
	validRaw := "sk-goal-test-key-2"
	store.Keys = []apiKeyRecord{{
		ID:        "key-goal-2",
		Name:      "goal test 2",
		Prefix:    "sk-goal",
		Hash:      keyHash(validRaw),
		CreatedAt: time.Now(),
	}}
	s := &Server{
		sessionResolver:     openSessionResolver(),
		apiKeys:             store,
		sessions:            openSessionStore(),
		userSessions:        openUserSessionStore(30 * time.Minute),
		conversationManager: openConversationManager(),
	}
	routes := s.Routes()

	task := buildTaskLedger(&oaiReq{Messages: []oaiMsg{{Role: "user", Content: "goal"}}})
	task.GoalID = "goal-real"
	body := &oaiReq{Messages: []oaiMsg{{Role: "user", Content: "goal"}}}
	s.sessionResolver.BindWithTask("sess-goal-2", "conv-1", "acc-1", body, "", httptest.NewRequest("POST", "/v1/chat/completions", nil), task)

	req := httptest.NewRequest(http.MethodPost, "/v1/goal", strings.NewReader(`{"action":"complete","goal_id":"goal-wrong"}`))
	req.Header.Set("Authorization", "Bearer "+validRaw)
	req.Header.Set(sessionHeaderName, "sess-goal-2")
	rr := httptest.NewRecorder()
	routes.ServeHTTP(rr, req)
	if rr.Code != http.StatusConflict {
		t.Fatalf("goal_id mismatch must 409, got %d body=%s", rr.Code, rr.Body.String())
	}
	b, _ := s.sessionResolver.GetSession("sess-goal-2")
	if b.Task.IsComplete() {
		t.Fatal("mismatched update must not close the goal")
	}
}
