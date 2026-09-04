package web

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

// bindLikeServer mimics server.go bindConversation: it appends the assistant
// reply into historyBody.Messages (assistantText param stays empty), carries
// the pristine client messages in ClientMessages exactly like the production
// path, and calls BindWithTask — so the anchor chain covers the client's
// messages only.
func bindLikeServer(t *testing.T, sr *sessionResolver, sid, convID, accID string, reqMessages []oaiMsg, assistantText string) {
	t.Helper()
	historyBody := oaiReq{Messages: append(cloneMessages(reqMessages), oaiMsg{
		Role:    "assistant",
		Content: assistantText,
	})}
	historyBody.ClientMessages = cloneMessages(reqMessages)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set(sessionHeaderName, sid)
	sr.BindWithTask("upstream-session", convID, accID, &historyBody, "", req, nil)
}

// TestDSHPureTextContinuation reproduces the exact production bind shape for a
// pure-text conversation: first turn binds [system, user1] as the client
// history plus the synthesized assistant reply; second turn replays
// [system, user1, assistant(output_text item), user2]. This MUST match
// (chain tail = user1) so the resolver reuses the upstream conversation and
// reports cached tokens; the echoed answer belongs to the increment.
func TestDSHPureTextContinuation(t *testing.T) {
	t.Setenv("M365_SESSION_CACHE", filepath.Join(t.TempDir(), "sessions.json"))
	t.Setenv("M365_CONVERSATION_CACHE", filepath.Join(t.TempDir(), "conversations.json"))
	t.Setenv("M365_USER_SESSION_CACHE", filepath.Join(t.TempDir(), "users.json"))
	sr := openSessionResolver()

	systemMsg := oaiMsg{Role: "system", Content: "You are a helpful assistant."}
	userMsg1 := oaiMsg{Role: "user", Content: "hello"}
	assistantText := "Hi there! How can I help you today?"

	bindLikeServer(t, sr, "dsh-session", "conv-1", "account-a",
		[]oaiMsg{systemMsg, userMsg1}, assistantText)

	// Second turn: DSH replays assistant as output_text item (parts-array)
	assistantItem := oaiMsg{
		Role:    "assistant",
		Content: []any{map[string]any{"type": "output_text", "text": assistantText, "annotations": []any{}}},
	}
	userMsg2 := oaiMsg{Role: "user", Content: "tell me more"}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set(sessionHeaderName, "dsh-session")
	secondTurn := &oaiReq{Messages: []oaiMsg{systemMsg, userMsg1, assistantItem, userMsg2}}
	resolved := sr.Resolve(req, secondTurn)

	if resolved.IsNew {
		t.Fatal("DSH with session_id should NOT be IsNew")
	}
	if resolved.ResetUpstream {
		t.Fatalf("pure-text DSH continuation should reuse upstream; got ResetUpstream=%t matched=%q HistoryLen=%d", resolved.ResetUpstream, resolved.MatchedBy, resolved.HistoryLen)
	}
	if resolved.HistoryLen != 2 {
		t.Fatalf("expected HistoryLen=2 (chain=system+user1; echoed answer is increment), got %d", resolved.HistoryLen)
	}
	t.Logf("OK: HistoryLen=%d matched=%s", resolved.HistoryLen, resolved.MatchedBy)
}

// TestDSHToolCallContinuation simulates a tool-calling turn. bindConversation
// stores ONLY the assistant final text (res.Text) as ONE message. DSH's replay
// splits the assistant turn into MULTIPLE items: output_text message + one
// function_call item + tool result. If the stored 1 assistant message cannot
// line up with the replayed N items, the strict prefix match fails and the
// session resets upstream every turn → cached_tokens always 0.
func TestDSHToolCallContinuation(t *testing.T) {
	t.Setenv("M365_SESSION_CACHE", filepath.Join(t.TempDir(), "sessions.json"))
	t.Setenv("M365_CONVERSATION_CACHE", filepath.Join(t.TempDir(), "conversations.json"))
	t.Setenv("M365_USER_SESSION_CACHE", filepath.Join(t.TempDir(), "users.json"))
	sr := openSessionResolver()

	systemMsg := oaiMsg{Role: "system", Content: "You are a helpful assistant."}
	userMsg1 := oaiMsg{Role: "user", Content: "search for something"}
	assistantText := "I'll search for that."

	// bindConversation stores [system, user1, assistant(final-text)] — tool calls
	// are NOT stored because chathub.Result only carries res.Text.
	bindLikeServer(t, sr, "dsh-tool-session", "conv-2", "account-a",
		[]oaiMsg{systemMsg, userMsg1}, assistantText)

	// Second turn replay: DSH splits the assistant turn into separate items
	assistantTextItem := oaiMsg{
		Role:    "assistant",
		Content: []any{map[string]any{"type": "output_text", "text": assistantText, "annotations": []any{}}},
	}
	assistantToolCallItem := oaiMsg{
		Role: "assistant",
		ToolCalls: []map[string]any{
			{"id": "call_123", "type": "function", "function": map[string]any{"name": "web_search", "arguments": `{"query": "test"}`}},
		},
	}
	toolResultItem := oaiMsg{
		Role:       "tool",
		ToolCallID: "call_123",
		Content:    "search results",
	}
	userMsg2 := oaiMsg{Role: "user", Content: "what did you find?"}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set(sessionHeaderName, "dsh-tool-session")
	secondTurn := &oaiReq{Messages: []oaiMsg{systemMsg, userMsg1, assistantTextItem, assistantToolCallItem, toolResultItem, userMsg2}}
	resolved := sr.Resolve(req, secondTurn)

	if resolved.IsNew {
		t.Fatal("DSH with session_id should NOT be IsNew")
	}
	t.Logf("result: matched=%q HistoryLen=%d ResetUpstream=%t", resolved.MatchedBy, resolved.HistoryLen, resolved.ResetUpstream)
	if resolved.ResetUpstream {
		t.Logf("ROOT CAUSE CONFIRMED: tool-calling turns reset the upstream conversation every time")
		t.Logf("stored 1 assistant msg (text only) vs replayed %d assistant items (text + tool_call)", 2)
		t.Logf("→ contextPrefixLen=0 → explicit_context_reset → new upstream conversation → cached_tokens=0")
	} else {
		t.Logf("tool-call prefix matched: HistoryLen=%d (assistant items aligned)", resolved.HistoryLen)
	}
}
