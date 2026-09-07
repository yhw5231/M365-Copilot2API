package web

import (
	"fmt"
)

// repairDuplicateToolCallIDs rewrites assistant tool-call ids that a replayed
// history carries more than once. Some clients re-emit the SAME tool-call
// group — identical ids and identical arguments — several times per request
// (observed: an agent client replayed one three-call parallel round six times
// after its own retry/compaction cycle). The tool protocol treats a call id as
// unique per conversation, so validateToolConversation hard-rejects the whole
// turn with "duplicate tool call id", and every retry replays the same broken
// history: the session is bricked.
//
// The first occurrence keeps its id; every later occurrence is aliased
// deterministically as <id>#dup<N> by occurrence index, and the tool result
// that follows a replayed group is re-pointed to the same alias (results
// always follow their calls, so the current declared-occurrence count selects
// the matching alias). A result can only reference a fully paired replay:
// validateToolConversation rejects an assistant turn while results are still
// pending, so an id that repeats is always one whose earlier occurrence is
// already completed.
//
// The rewrite mutates the request's maps in place. That is deliberate:
// cloneMessages shares these maps with body.ClientMessages, and anchors
// (msgAnchor) exclude tool-call ids, so session matching is unaffected while
// the full prompt and the session increment stay consistent with each other.
// The alias is derived from the original id and its index — never random — so
// identical replays produce identical prompts across turns.
func repairDuplicateToolCallIDs(messages []oaiMsg) []oaiMsg {
	declared := map[string]int{} // original id -> occurrences declared so far
	for i := range messages {
		m := &messages[i]
		switch m.Role {
		case "assistant":
			for _, call := range m.ToolCalls {
				id, _ := call["id"].(string)
				if id == "" {
					continue
				}
				declared[id]++
				if n := declared[id]; n > 1 {
					call["id"] = fmt.Sprintf("%s#dup%d", id, n)
				}
			}
		case "tool":
			if m.ToolCallID == "" {
				continue
			}
			if n := declared[m.ToolCallID]; n > 1 {
				m.ToolCallID = fmt.Sprintf("%s#dup%d", m.ToolCallID, n)
			}
		}
	}
	return messages
}

// validateToolConversation enforces the OpenAI tool protocol without making
// assumptions about what a tool does. Every assistant call must be followed by
// exactly one matching tool result before another model turn is requested.
func validateToolConversation(messages []oaiMsg) error {
	if len(messages) > 0 {
		first := messages[0].Role
		if first != "system" && first != "developer" && first != "user" && first != "assistant" {
			return fmt.Errorf("first message must have role system, developer, user, or assistant, got %q", first)
		}
	}
	pending := map[string]bool{}
	completed := map[string]bool{}
	for i, m := range messages {
		switch m.Role {
		case "assistant":
			if len(pending) > 0 {
				return fmt.Errorf("tool results missing before assistant message at index %d", i)
			}
			for _, call := range m.ToolCalls {
				id, _ := call["id"].(string)
				if id == "" {
					return fmt.Errorf("assistant tool call missing id at index %d", i)
				}
				if pending[id] || completed[id] {
					return fmt.Errorf("duplicate tool call id: %s", id)
				}
				pending[id] = true
			}
		case "tool":
			if m.ToolCallID == "" {
				return fmt.Errorf("tool_call_id required at index %d", i)
			}
			if !pending[m.ToolCallID] {
				return fmt.Errorf("unexpected tool result: %s", m.ToolCallID)
			}
			delete(pending, m.ToolCallID)
			completed[m.ToolCallID] = true
		}
	}
	if len(pending) > 0 {
		for id := range pending {
			return fmt.Errorf("missing tool result for tool_call_id: %s", id)
		}
	}
	return nil
}
