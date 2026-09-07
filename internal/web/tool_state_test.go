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

	repairDuplicateToolCallIDs(msgs)
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
	repairDuplicateToolCallIDs(a)
	repairDuplicateToolCallIDs(b)
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
	repairDuplicateToolCallIDs(msgs)
	if after := fmt.Sprintf("%#v", msgs); after != before {
		t.Fatalf("unique history was modified:\nbefore: %s\nafter:  %s", before, after)
	}
	if err := validateToolConversation(msgs); err != nil {
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
