package web

import (
	"fmt"
	"strings"
	"testing"
)

func TestCompactToolResultKeepsHeadTailAndError(t *testing.T) {
	s := "start\n" + strings.Repeat("progress line\n", 1000) + "ERROR: build failed\nexit code 1"
	got := compactToolResult(s, 800)
	if len(got) > 900 || !strings.Contains(got, "start") || !strings.Contains(got, "ERROR: build failed") || !strings.Contains(got, "exit code 1") || !strings.Contains(got, "truncated") {
		t.Fatalf("bad compact result: %d %q", len(got), got)
	}
}

func TestAgentLedgerRepeatedFailureBlocksContinuation(t *testing.T) {
	msgs := []oaiMsg{
		{Role: "assistant", ToolCalls: []map[string]any{{"id": "c1", "type": "function", "function": map[string]any{"name": "run", "arguments": "{\"cmd\":\"build\"}"}}}},
		{Role: "tool", ToolCallID: "c1", Content: "exit code 1: failed"},
		{Role: "assistant", ToolCalls: []map[string]any{{"id": "c2", "type": "function", "function": map[string]any{"name": "run", "arguments": "{\"cmd\":\"build\"}"}}}},
		{Role: "tool", ToolCallID: "c2", Content: "exit code 1: failed"},
	}
	l := buildAgentLedger(msgs)
	if !l.RepeatedFailure {
		t.Fatalf("expected repeated failure: %+v", l)
	}
	if err := l.CanContinue(32); err == nil || !strings.Contains(err.Error(), "repeated tool failure") {
		t.Fatalf("expected repeated failure error, got: %v", err)
	}
}

// TestAgentLedgerSkipsEmptyNameCall ensures a tool call with an empty name
// (transport-level corruption, not a model strategy loop) does not contribute
// to repeated-failure or stuck-loop detection, and does not leave a pending
// entry that blocks continuation.
func TestAgentLedgerSkipsEmptyNameCall(t *testing.T) {
	msgs := []oaiMsg{
		{Role: "assistant", ToolCalls: []map[string]any{{"id": "c1", "type": "function", "function": map[string]any{"name": "", "arguments": "{\"command\":\"x\"}"}}}},
		{Role: "tool", ToolCallID: "c1", Content: "error: unknown tool"},
		{Role: "assistant", ToolCalls: []map[string]any{{"id": "c2", "type": "function", "function": map[string]any{"name": "", "arguments": "{\"command\":\"x\"}"}}}},
		{Role: "tool", ToolCallID: "c2", Content: "error: unknown tool"},
	}
	l := buildAgentLedger(msgs)
	if l.RepeatedFailure {
		t.Fatalf("empty-name call must not trigger repeated failure: %+v", l)
	}
	if l.RepeatedCall {
		t.Fatalf("empty-name call must not trigger repeated call: %+v", l)
	}
	if len(l.Pending) > 0 {
		t.Fatalf("empty-name call must not leave pending: %+v", l)
	}
	if err := l.CanContinue(32); err != nil {
		t.Fatalf("empty-name call must not block continuation: %v", err)
	}
}

func TestAgentLedgerEvidenceAndUniqueCallIDs(t *testing.T) {
	a := scopedCallID("run", "{}", 0, "turn-a")
	b := scopedCallID("run", "{}", 0, "turn-b")
	if a == b {
		t.Fatal("call IDs collide across turns")
	}
	l := buildAgentLedger([]oaiMsg{{Role: "assistant", ToolCalls: []map[string]any{{"id": "c1", "type": "function", "function": map[string]any{"name": "create", "arguments": "{}"}}}}, {Role: "tool", ToolCallID: "c1", Content: "created"}})
	if len(l.Completed) != 1 || !strings.Contains(l.RouterContext(), "c1") {
		t.Fatalf("missing evidence: %+v", l)
	}
}

func TestAgentLedgerDetectsRepeatedCallAndRoundLimit(t *testing.T) {
	var msgs []oaiMsg
	for i := 0; i < 4; i++ {
		id := fmt.Sprintf("c%d", i)
		msgs = append(msgs, oaiMsg{Role: "assistant", ToolCalls: []map[string]any{{"id": id, "type": "function", "function": map[string]any{"name": "poll", "arguments": "{\"id\":1}"}}}}, oaiMsg{Role: "tool", ToolCallID: id, Content: "still pending"})
	}
	l := buildAgentLedger(msgs)
	if !l.RepeatedCall || l.ToolRounds != 4 {
		t.Fatalf("loop not detected: %+v", l)
	}
	if err := l.CanContinue(3); err == nil {
		t.Fatal("expected round limit")
	}
}

func TestActiveMessagesIgnoresOlderToolHistory(t *testing.T) {
	var msgs []oaiMsg
	for i := 0; i < 20; i++ {
		id := fmt.Sprintf("old%d", i)
		msgs = append(msgs,
			oaiMsg{Role: "assistant", ToolCalls: []map[string]any{{"id": id, "type": "function", "function": map[string]any{"name": "old", "arguments": "{}"}}}},
			oaiMsg{Role: "tool", ToolCallID: id, Content: "done"},
		)
	}
	msgs = append(msgs, oaiMsg{Role: "user", Content: "continue with a new model"})
	full := buildAgentLedger(msgs)
	active := buildAgentLedger(activeMessages(msgs))
	if full.ToolRounds < 20 {
		t.Fatalf("expected full history tools, got %d", full.ToolRounds)
	}
	if active.ToolRounds != 0 {
		t.Fatalf("new user turn should reset round limit scope, got %d", active.ToolRounds)
	}
	if err := active.CanContinue(16); err != nil {
		t.Fatalf("new user turn blocked by old history: %v", err)
	}
}

func TestCompletionGuardRejectsPendingAndUnsupportedSuccess(t *testing.T) {
	l := buildAgentLedger([]oaiMsg{
		{Role: "assistant", ToolCalls: []map[string]any{
			{"id": "p1", "type": "function", "function": map[string]any{"name": "deploy", "arguments": "{}"}},
		}},
	})
	if completionEvidenceAllows("Deployment completed successfully", l) {
		t.Fatal("pending action allowed as complete")
	}
}

func TestCompletionGuardRejectsUnsupportedSuccess(t *testing.T) {
	if completionEvidenceAllows("Installed, started, and verified successfully", buildAgentLedger(nil)) {
		t.Fatal("unsupported success allowed")
	}
	if !completionEvidenceAllows("I cannot confirm completion because no tool results were returned.", buildAgentLedger(nil)) {
		t.Fatal("honest incomplete response rejected")
	}
	// A tool-less conversational answer that makes no completion claim must
	// still pass: the guard only rejects self-congratulatory success with no
	// supporting tool evidence.
	if !completionEvidenceAllows("Here is the code you asked for.", buildAgentLedger(nil)) {
		t.Fatal("plain conversational answer rejected")
	}
}
