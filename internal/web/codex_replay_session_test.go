package web

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestReplayCodexSessionIncrement replays a real Codex Desktop 0.153.1 session
// (rollout items, one snapshot per user turn) through the gateway's real
// conversion + session resolver + increment slicing to see exactly what the
// upstream conversation would receive each turn.
func TestReplayCodexSessionIncrement(t *testing.T) {
	fixPath := os.Getenv("CODEX_REPLAY_FIXTURE")
	if fixPath == "" {
		fixPath = filepath.Join("..", "..", "tmp", "codex_replay_fixture.json")
	}
	b, err := os.ReadFile(fixPath)
	if err != nil {
		t.Skipf("fixture not available: %v", err)
	}
	var fix struct {
		Instructions string            `json:"instructions"`
		Snapshots    [][]json.RawMessage `json:"snapshots"`
	}
	if err := json.Unmarshal(b, &fix); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	tmp := t.TempDir()
	t.Setenv("M365_SESSION_CACHE", filepath.Join(tmp, "sessions.json"))
	t.Setenv("M365_CONVERSATION_CACHE", filepath.Join(tmp, "conversations.json"))

	sr := openSessionResolver()
	sessionID := ""
	conversationID := "conv-replay"

	for turn, snap := range fix.Snapshots {
		if len(snap) == 0 {
			continue
		}
		var items []any
		for _, raw := range snap {
			var v any
			_ = json.Unmarshal(raw, &v)
			items = append(items, v)
		}
		req := responsesRequest{Model: "m365-copilot", Instructions: fix.Instructions, Input: items, Stream: true}
		o, err := req.openAI()
		if err != nil {
			t.Fatalf("turn %d: openAI: %v", turn+1, err)
		}
		o.ClientMessages = cloneMessages(o.Messages)

		r := httptest.NewRequest("POST", "/v1/responses", nil)
		r.Header.Set("session_id", "codex-session-01")
		res := sr.Resolve(r, &o)

		incDesc := "(fresh conversation -> FULL prompt)"
		if !res.IsNew && !res.ResetUpstream {
			if res.HistoryLen > 0 {
				incMsgs := sessionIncrementMessages(o.ClientMessages, res.HistoryLen)
				incPrompt, _ := flattenPromptMessages(incMsgs, nil)
				incPrompt = strings.TrimSpace(incPrompt)
				tail := incPrompt
				if len(tail) > 400 {
					tail = tail[len(tail)-400:]
				}
				incDesc = fmt.Sprintf("increment msgs=%d len=%d tail=%q", len(incMsgs), len(incPrompt), tail)
				if incPrompt == "" {
					incDesc = "INCREMENT EMPTY -> falls back to FULL prompt"
				}
			} else {
				incDesc = "explicit_incremental -> current turn only"
			}
		} else if res.ResetUpstream {
			incDesc = fmt.Sprintf("ResetUpstream (%s) -> fresh conversation, FULL prompt", res.MatchedBy)
		}
		t.Logf("turn %d: msgs=%d matched=%s historyLen=%d chain=%d %s",
			turn+1, len(o.Messages), res.MatchedBy, res.HistoryLen, len(srAnchor(sr)), incDesc)
		// Print the last few converted messages to see the increment composition.
		start := res.HistoryLen
		if !res.IsNew && !res.ResetUpstream && res.HistoryLen == 0 {
			start = len(o.ClientMessages) - 3
		}
		if start < 0 {
			start = 0
		}
		for idx := start; idx < len(o.ClientMessages); idx++ {
			m := o.ClientMessages[idx]
			txt := strings.ReplaceAll(contentToString(m.Content), "\n", " ")
			if len(txt) > 80 {
				txt = txt[:80]
			}
			tc := ""
			if len(m.ToolCalls) > 0 {
				tc = fmt.Sprintf(" toolcalls=%d", len(m.ToolCalls))
			}
			t.Logf("    msg[%d] role=%s%s text=%q", idx, m.Role, tc, txt)
		}

		// Bind like bindConversation does after a completed round.
		if res.IsNew {
			sessionID = uuid.NewString()
		} else {
			sessionID = res.SessionID
			if res.ConversationID != "" {
				conversationID = res.ConversationID
			}
		}
		if res.ResetUpstream || res.IsNew {
			conversationID = "conv-replay-" + uuid.NewString()[:8]
		}
		hb := o
		hb.ClientMessages = o.ClientMessages
		hb.Messages = append(cloneMessages(o.ClientMessages), oaiMsg{Role: "assistant", Content: "(assistant reply)"})
		sr.BindWithTask(sessionID, conversationID, "acc1", &hb, "", r, nil)
	}
}

func srAnchor(sr *sessionResolver) []string {
	sessions := sr.ListSessions()
	if len(sessions) == 0 {
		return nil
	}
	return sessions[0].AnchorChain
}
