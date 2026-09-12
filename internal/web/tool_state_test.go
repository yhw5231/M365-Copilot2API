package web

import (
	"fmt"
	"testing"
)

// assistantCallMsg builds an assistant message with one tool call.
func assistantCallMsg(id string) oaiMsg {
	return assistantParallelCallsMsg([]string{id})
}

// assistantParallelCallsMsg builds ONE assistant message carrying a group of
// parallel tool calls (the shape the Responses converter produces when a
// client replays consecutive function_call items).
func assistantParallelCallsMsg(ids []string) oaiMsg {
	calls := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		calls = append(calls, map[string]any{"id": id, "type": "function", "function": map[string]any{"name": "terminal", "arguments": "{\"command\":\"pwd\"}"}})
	}
	return oaiMsg{Role: "assistant", ToolCalls: calls}
}

// toolResultMsg builds a tool result message answering the given call id.
func toolResultMsg(id string) oaiMsg {
	return oaiMsg{Role: "tool", ToolCallID: id, Content: "ok"}
}

// TestRepairDuplicateToolCallIDsParallelGroupReplay reproduces the bricked
// session from the 2026-09-07 error records: an agent client replayed the same
// parallel tool-call round (identical chatcmpl-tool-* ids AND arguments) four
// times in one request after its own retry/compaction cycle. Every replay
// collided in validateToolConversation ("duplicate tool call id"), and each
// client retry sent the same history again — a hard 400 loop with no recovery.
// The repair must alias every later occurrence (calls AND their paired
// results) so the request validates.
func TestRepairDuplicateToolCallIDsParallelGroupReplay(t *testing.T) {
	// One user turn followed by the same 3-call parallel group replayed 4x.
	msgs := []oaiMsg{{Role: "user", Content: "先待办吧"}}
	ids := []string{"chatcmpl-tool-a4ba8863bb373fe1", "chatcmpl-tool-90d615d7e44495b3", "chatcmpl-tool-9c0165a22a12ff85"}
	for round := 0; round < 4; round++ {
		msgs = append(msgs, assistantParallelCallsMsg(ids))
		for _, id := range ids {
			msgs = append(msgs, toolResultMsg(id))
		}
	}
	msgs = append(msgs, oaiMsg{Role: "user", Content: "go"})

	msgs = repairDuplicateToolCallIDs(msgs)
	if err := validateToolConversation(msgs); err != nil {
		t.Fatalf("replayed duplicate ids still rejected after repair: %v", err)
	}
	// First occurrence keeps the original id; later occurrences get indexed
	// aliases (both on the call and on its paired result).
	for i, id := range ids {
		if got := msgs[1].ToolCalls[i]["id"]; got != id {
			t.Fatalf("first occurrence call %d id = %v, want %q", i, got, id)
		}
		if got := msgs[2+i].ToolCallID; got != id {
			t.Fatalf("first occurrence result id = %q, want %q", got, id)
		}
		want := fmt.Sprintf("%s#dup4", id)
		if got := msgs[13].ToolCalls[i]["id"]; got != want {
			t.Fatalf("4th occurrence call %d id = %v, want %q", i, got, want)
		}
		if got := msgs[14+i].ToolCallID; got != want {
			t.Fatalf("4th occurrence result id = %q, want %q", got, want)
		}
	}
}

// TestRepairDuplicateToolCallIDsDeterministic guards the requirement that the
// same replayed history always produces the same aliases: anchors exclude
// tool-call ids but the flattened prompt includes them, and two turns carrying
// the same history must flatten identically.
func TestRepairDuplicateToolCallIDsDeterministic(t *testing.T) {
	build := func() []oaiMsg {
		msgs := []oaiMsg{assistantCallMsg("call_x"), toolResultMsg("call_x"), assistantCallMsg("call_x"), toolResultMsg("call_x")}
		return msgs
	}
	a, b := build(), build()
	a = repairDuplicateToolCallIDs(a)
	b = repairDuplicateToolCallIDs(b)
	for i := range a {
		if a[i].ToolCallID != b[i].ToolCallID {
			t.Fatalf("message %d tool_call_id differs between runs: %q vs %q", i, a[i].ToolCallID, b[i].ToolCallID)
		}
		if len(a[i].ToolCalls) != len(b[i].ToolCalls) {
			t.Fatalf("message %d tool call count differs", i)
		}
		for j := range a[i].ToolCalls {
			if a[i].ToolCalls[j]["id"] != b[i].ToolCalls[j]["id"] {
				t.Fatalf("message %d call %d id differs: %v vs %v", i, j, a[i].ToolCalls[j]["id"], b[i].ToolCalls[j]["id"])
			}
		}
	}
}

// TestRepairDuplicateToolCallIDsUniqueHistoryIsUntouched: a protocol-clean
// conversation must come out byte-identical — the repair only fires on actual
// duplicates so session prompts never shift for healthy clients.
func TestRepairDuplicateToolCallIDsUniqueHistoryIsUntouched(t *testing.T) {
	msgs := []oaiMsg{
		{Role: "user", Content: "hi"},
		assistantParallelCallsMsg([]string{"call_1", "call_2"}),
		toolResultMsg("call_1"),
		toolResultMsg("call_2"),
		{Role: "user", Content: "next"},
		assistantCallMsg("call_3"),
		toolResultMsg("call_3"),
	}
	before := fmt.Sprintf("%#v", msgs)
	after := repairDuplicateToolCallIDs(msgs)
	if afterStr := fmt.Sprintf("%#v", after); afterStr != before {
		t.Fatalf("unique history was modified:\nbefore: %s\nafter:  %s", before, afterStr)
	}
	if err := validateToolConversation(after); err != nil {
		t.Fatalf("clean history rejected: %v", err)
	}
}

// TestValidateToolConversationRejectsDuplicateIDs: without the repair the
// validator must still reject a repeated id (the repair is applied at the
// request boundary, the validator's contract stays strict).
func TestValidateToolConversationRejectsDuplicateIDs(t *testing.T) {
	msgs := []oaiMsg{assistantCallMsg("call_1"), toolResultMsg("call_1"), assistantCallMsg("call_1"), toolResultMsg("call_1")}
	if err := validateToolConversation(msgs); err == nil {
		t.Fatal("duplicate id must still be rejected when the repair has not run")
	}
}

// TestRepairInterleavedAssistantText reproduces the 2026-09-08 error record:
// Codex Desktop replays an assistant turn that narrated while calling a tool
// as separate Responses items — function_call, assistant message,
// function_call_output. The literal conversion puts a plain assistant message
// between the call and its result and validateToolConversation rejects the
// whole turn ("tool results missing before assistant message"), bricking the
// session because every retry replays the same history. The narration must be
// merged into the tool-call assistant message.
func TestRepairInterleavedAssistantText(t *testing.T) {
	msgs := []oaiMsg{
		{Role: "user", Content: "check the file"},
		assistantCallMsg("call-1"),
		{Role: "assistant", Content: "我先检查文件内容。"},
		toolResultMsg("call-1"),
		{Role: "user", Content: "next"},
	}
	body := repairInterleavedAssistantText(msgs)
	if len(body) != 4 {
		t.Fatalf("expected 4 messages after merge, got %d", len(body))
	}
	if err := validateToolConversation(body); err != nil {
		t.Fatalf("interleaved history still rejected after repair: %v", err)
	}
	// The narration is folded into the tool-call assistant message; the
	// standalone assistant message is gone.
	merged := body[1]
	if len(merged.ToolCalls) != 1 {
		t.Fatalf("merged assistant message lost its tool calls: %d", len(merged.ToolCalls))
	}
	if contentToString(merged.Content) != "我先检查文件内容。" {
		t.Fatalf("merged content = %q, want narration text", contentToString(merged.Content))
	}
}

// TestRepairInterleavedAssistantTextPreservesCleanHistory: a conversation
// where assistant text messages follow completed tool rounds must pass
// through unchanged — the repair only fires while results are still pending.
func TestRepairInterleavedAssistantTextPreservesCleanHistory(t *testing.T) {
	msgs := []oaiMsg{
		{Role: "user", Content: "hi"},
		assistantCallMsg("call_1"),
		toolResultMsg("call_1"),
		{Role: "assistant", Content: "done"},
		{Role: "user", Content: "next"},
	}
	before := fmt.Sprintf("%#v", msgs)
	after := repairInterleavedAssistantText(msgs)
	if fmt.Sprintf("%#v", after) != before {
		t.Fatalf("clean history was modified:\nbefore: %s\nafter:  %s", before, fmt.Sprintf("%#v", after))
	}
}

// TestRepairDuplicateToolCallIDsSameGroupTwice: the trace also shows a client
// re-emitting the SAME call id twice inside one replayed group (two
// function_call items, two outputs). Both results must be aliased by
// consumption order — result #1 → original id, result #2 → #dup2 — instead of
// both being re-pointed at #dup2 by the declared count.
func TestRepairDuplicateToolCallIDsSameGroupTwice(t *testing.T) {
	msgs := []oaiMsg{
		{Role: "user", Content: "go"},
		assistantCallMsg("call-dup"),
		assistantCallMsg("call-dup"),
		toolResultMsg("call-dup"),
		toolResultMsg("call-dup"),
		{Role: "user", Content: "next"},
	}
	msgs = repairInterleavedAssistantText(msgs)
	// The two call turns collapse into one parallel round.
	if len(msgs) != 5 {
		t.Fatalf("expected 5 messages after group merge, got %d", len(msgs))
	}
	msgs = repairDuplicateToolCallIDs(msgs)
	if err := validateToolConversation(msgs); err != nil {
		t.Fatalf("same-group duplicate ids still rejected after repair: %v", err)
	}
	if got := msgs[3].ToolCallID; got != "call-dup#dup2" {
		t.Fatalf("second result id = %q, want call-dup#dup2", got)
	}
}

// TestRepairDuplicateToolCallIDsExtraResults reproduces the 2026-09-12 error
// record: OpenClaw's cron replay declared one exec function_call, answered it
// with an empty placeholder function_call_output, and later appended the REAL
// output under the SAME call id — one declared call, two results. Aliasing the
// second result by consumption count produced <id>#dup2 with no matching call
// and validateToolConversation hard-rejected the turn ("unexpected tool
// result: …#dup2"), bricking every retry of the same history behind a stream
// that had already emitted response.created. The extra result must fold into
// the earlier one: the real output replaces the empty placeholder in its
// original position and the duplicate is dropped.
func TestRepairDuplicateToolCallIDsExtraResults(t *testing.T) {
	const realOutput = "❌ Elysiver 签到失败\nUnauthorized, invalid access token"
	msgs := []oaiMsg{
		{Role: "user", Content: "run checkin"},
		assistantCallMsg("fc0817c107ab5f45a08ee1e426dc400452"),
		{Role: "tool", ToolCallID: "fc0817c107ab5f45a08ee1e426dc400452", Content: ""},
		assistantCallMsg("callb9bb7b1c9d5c470b987584e6c0c06f28"),
		{Role: "tool", ToolCallID: "callb9bb7b1c9d5c470b987584e6c0c06f28", Content: ""},
		{Role: "assistant", Content: ""},
		{Role: "tool", ToolCallID: "fc0817c107ab5f45a08ee1e426dc400452", Content: realOutput},
		{Role: "tool", ToolCallID: "callb9bb7b1c9d5c470b987584e6c0c06f28", Content: realOutput},
	}
	msgs = repairDuplicateToolCallIDs(msgs)
	if err := validateToolConversation(msgs); err != nil {
		t.Fatalf("extra results still rejected after repair: %v", err)
	}
	if len(msgs) != 6 {
		t.Fatalf("expected 6 messages after folding extra results, got %d", len(msgs))
	}
	if got := msgs[2].ToolCallID; got != "fc0817c107ab5f45a08ee1e426dc400452" {
		t.Fatalf("first result id = %q, want the unaliased original id", got)
	}
	if got := contentToString(msgs[2].Content); got != realOutput {
		t.Fatalf("folded result content = %q, want the real output", got)
	}
	if got := contentToString(msgs[4].Content); got != realOutput {
		t.Fatalf("second folded result content = %q, want the real output", got)
	}
}

// TestRepairDuplicateToolCallIDsEmptyExtraResultKeepsRealOutput: the fold must
// not let a later EMPTY replay clobber an earlier real output — the latest
// non-empty content wins.
func TestRepairDuplicateToolCallIDsEmptyExtraResultKeepsRealOutput(t *testing.T) {
	msgs := []oaiMsg{
		{Role: "user", Content: "go"},
		assistantCallMsg("call_x"),
		{Role: "tool", ToolCallID: "call_x", Content: "real output"},
		{Role: "tool", ToolCallID: "call_x", Content: ""},
	}
	msgs = repairDuplicateToolCallIDs(msgs)
	if err := validateToolConversation(msgs); err != nil {
		t.Fatalf("extra empty result still rejected after repair: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages after dropping the empty duplicate, got %d", len(msgs))
	}
	if got := contentToString(msgs[2].Content); got != "real output" {
		t.Fatalf("result content = %q, want the earlier real output", got)
	}
}
