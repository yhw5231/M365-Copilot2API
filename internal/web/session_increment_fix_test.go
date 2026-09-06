package web

import (
	"strings"
	"testing"
)

// TestSessionIncrementMessages covers the increment slicing fixes: the leading
// assistant echo must be dropped (the upstream conversation already holds that
// turn in its own encoding), tool results and the new user turn must be kept,
// and an already-consumed request (historyLen == full list, the client-retry
// case) must degrade to the final message instead of a full-transcript resend.
func TestSessionIncrementMessages(t *testing.T) {
	asstEcho := oaiMsg{Role: "assistant", Content: "previous answer"}
	asstCalls := oaiMsg{Role: "assistant", ToolCalls: []map[string]any{{"id": "call_1", "type": "function", "function": map[string]any{"name": "exec"}}}}
	toolOut := oaiMsg{Role: "tool", ToolCallID: "call_1", Content: "result"}
	userNew := oaiMsg{Role: "user", Content: "next question"}

	cases := []struct {
		name   string
		msgs   []oaiMsg
		hist   int
		want   []oaiMsg
	}{
		{"new user turn after answer echo", []oaiMsg{asstEcho, userNew}, 0, []oaiMsg{userNew}},
		{"tool round keeps only the result", []oaiMsg{asstCalls, toolOut}, 0, []oaiMsg{toolOut}},
		{"client retry degrades to final message", []oaiMsg{asstEcho, userNew}, 2, []oaiMsg{userNew}},
		{"mid-list assistant echo is kept", []oaiMsg{toolOut, asstEcho, userNew}, 0, []oaiMsg{toolOut, asstEcho, userNew}},
		{"all-assistant increment keeps last", []oaiMsg{asstEcho, asstEcho}, 0, []oaiMsg{asstEcho}},
		{"historyLen beyond list clamps", []oaiMsg{asstEcho, userNew}, 99, []oaiMsg{userNew}},
	}
	for _, tc := range cases {
		got := sessionIncrementMessages(tc.msgs, tc.hist)
		if len(got) != len(tc.want) {
			t.Fatalf("%s: got %d messages, want %d", tc.name, len(got), len(tc.want))
		}
		for i := range got {
			if got[i].Role != tc.want[i].Role || contentToString(got[i].Content) != contentToString(tc.want[i].Content) {
				t.Fatalf("%s: message %d = role %s, want role %s", tc.name, i, got[i].Role, tc.want[i].Role)
			}
		}
	}
}

// TestContentToStringNilMessage asserts that assistant tool-call messages
// (nil Content) no longer render a literal "<nil>" into flattened prompts.
func TestContentToStringNilMessage(t *testing.T) {
	if got := contentToString(nil); got != "" {
		t.Fatalf("contentToString(nil) = %q, want empty", got)
	}
	m := oaiMsg{Role: "assistant", ToolCalls: []map[string]any{{"id": "c1", "type": "function", "function": map[string]any{"name": "exec", "arguments": "{}"}}}}
	prompt, _ := flattenPromptMessages([]oaiMsg{m}, nil)
	if strings.Contains(prompt, "<nil>") {
		t.Fatalf("flattened prompt contains <nil>: %q", prompt)
	}
	if !strings.Contains(prompt, "[assistant tool_calls]") {
		t.Fatalf("flattened prompt lost the tool_calls block: %q", prompt)
	}
}

// TestResponsesInputUnknownTypeSkipped asserts that typed Responses input items
// the gateway does not model are skipped instead of being converted into user
// messages carrying raw JSON.
func TestResponsesInputUnknownTypeSkipped(t *testing.T) {
	req := responsesRequest{Input: []any{
		map[string]any{"type": "item_reference", "id": "msg_abc"},
		map[string]any{"type": "web_search_call", "status": "completed"},
		map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "hi"}}},
		map[string]any{"role": "user", "content": "plain"},
	}}
	o, err := req.openAI()
	if err != nil {
		t.Fatalf("openAI: %v", err)
	}
	var texts []string
	for _, m := range o.Messages {
		if m.Role == "user" {
			texts = append(texts, contentToString(m.Content))
		}
	}
	if len(texts) != 2 || strings.TrimSpace(texts[0]) != "hi" || strings.TrimSpace(texts[1]) != "plain" {
		t.Fatalf("unexpected user messages: %q", texts)
	}
}

// TestTaskLedgerSkipsSyntheticContext asserts the task ledger's ORIGINAL_GOAL
// is the user's real request, not the Codex/DSH environment-context block.
func TestTaskLedgerSkipsSyntheticContext(t *testing.T) {
	body := &oaiReq{Messages: []oaiMsg{
		{Role: "system", Content: "base instructions"},
		{Role: "user", Content: "<environment_context>\n  <cwd>D:\\x</cwd>\n  <sandbox_policy>workspace-write</sandbox_policy>\n</environment_context>"},
		{Role: "user", Content: "把脚本改成单文件运行"},
	}}
	task := buildTaskLedger(body)
	if task.OriginalGoal == "" || strings.Contains(task.OriginalGoal, "environment_context") {
		t.Fatalf("ORIGINAL_GOAL = %q, want the real user request", task.OriginalGoal)
	}
	if !strings.Contains(task.OriginalGoal, "把脚本改成单文件运行") {
		t.Fatalf("ORIGINAL_GOAL = %q, want the real user request", task.OriginalGoal)
	}
}

// TestStreamStoresCustomToolCallShape asserts the storage shape the streaming
// adapter now emits for custom tool calls round-trips through the input
// conversion: a replayed custom_tool_call item must anchor-compare identical to
// the stored assistant message (same type, same {"input": ...} arguments).
func TestStreamStoresCustomToolCallShape(t *testing.T) {
	rawInput := "echo hi"
	// The streaming adapter stores: {id, type: custom, function:{name, arguments: {"input": rawInput}}}.
	stored := map[string]any{"id": "ctc_1", "type": "custom", "function": map[string]any{"name": "exec", "arguments": mustJSON(map[string]any{"input": rawInput})}}
	if stored["type"] != "custom" {
		t.Fatalf("stored type = %v, want custom", stored["type"])
	}
	// The replay conversion of a custom_tool_call item must produce the same shape.
	req := responsesRequest{Input: []any{
		map[string]any{"type": "custom_tool_call", "call_id": "ctc_1", "name": "exec", "input": rawInput},
	}}
	o, err := req.openAI()
	if err != nil {
		t.Fatalf("openAI: %v", err)
	}
	if len(o.Messages) != 1 || len(o.Messages[0].ToolCalls) != 1 {
		t.Fatalf("replay produced %d messages", len(o.Messages))
	}
	replayed := o.Messages[0].ToolCalls[0]
	if replayed["type"] != stored["type"] {
		t.Fatalf("replay type = %v, stored type = %v", replayed["type"], stored["type"])
	}
	rf := replayed["function"].(map[string]any)
	sf := stored["function"].(map[string]any)
	if rf["arguments"] != sf["arguments"] {
		t.Fatalf("arguments diverge: replay=%v stored=%v", rf["arguments"], sf["arguments"])
	}
}
