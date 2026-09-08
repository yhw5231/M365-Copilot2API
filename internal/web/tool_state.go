package web

import (
	"fmt"
	"strings"
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
// deterministically as <id>#dup<N> by occurrence index. The tool result that
// answers an occurrence is re-pointed to the same alias: results follow their
// calls in order, so the j-th result for an id answers the j-th occurrence —
// results are aliased by CONSUMPTION count, not by the total declared count.
// (Two identical results can follow two identical calls replayed inside one
// group; aliasing by the declared count would point both results at #dup2 and
// reject the second as unexpected.) A result can only reference a fully
// paired replay: validateToolConversation rejects an assistant turn while
// results are still pending, so an id that repeats is always one whose
// earlier occurrence is already completed.
//
// The rewrite mutates the request's maps in place. That is deliberate:
// cloneMessages shares these maps with body.ClientMessages, and anchors
// (msgAnchor) exclude tool-call ids, so session matching is unaffected while
// the full prompt and the session increment stay consistent with each other.
// The alias is derived from the original id and its index — never random — so
// identical replays produce identical prompts across turns.
func repairDuplicateToolCallIDs(messages []oaiMsg) []oaiMsg {
	declared := map[string]int{} // original id -> occurrences declared so far
	consumed := map[string]int{} // original id -> results consumed so far
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
			consumed[m.ToolCallID]++
			if n := consumed[m.ToolCallID]; n > 1 {
				m.ToolCallID = fmt.Sprintf("%s#dup%d", m.ToolCallID, n)
			}
		}
	}
	return messages
}

// repairInterleavedAssistantText re-attaches assistant narration that a
// replayed history places between a tool call and its tool result. Codex
// Desktop (observed 2026-09-08 error records) replays an assistant turn that
// narrated while calling a tool as separate Responses items in the order
// function_call → assistant message → function_call_output. Converted
// literally, the plain assistant message lands between the call and its
// result, and validateToolConversation hard-rejects the whole turn with "tool
// results missing before assistant message" — bricking the session, because
// every client retry replays the same history. The narration belongs to the
// assistant turn that made the call, so it is merged into that message's
// content (an assistant message may carry content and tool_calls together)
// and the standalone message is dropped. A second call group that arrives
// while results are still pending is folded in the same way (consecutive
// assistant call turns are equally protocol-invalid).
//
// Like repairDuplicateToolCallIDs this mutates the request's maps in place and
// returns a filtered slice the caller must reassign; the rewrite is
// deterministic, so identical replays produce identical prompts and session
// anchors are unaffected. Clean histories (no pending calls around an
// assistant text message) pass through unchanged.
func repairInterleavedAssistantText(messages []oaiMsg) []oaiMsg {
	kept := make([]oaiMsg, 0, len(messages))
	pending := 0    // tool calls awaiting a result
	lastCalls := -1 // kept-index of the assistant message holding the pending calls
	for _, m := range messages {
		switch m.Role {
		case "assistant":
			if len(m.ToolCalls) > 0 {
				if pending > 0 && lastCalls >= 0 {
					// Another call group while results are still pending:
					// also protocol-invalid (consecutive assistant turns with
					// no results between). Fold its calls into the pending
					// group — one parallel round.
					kept[lastCalls].ToolCalls = append(kept[lastCalls].ToolCalls, m.ToolCalls...)
					kept[lastCalls].Content = mergeMessageContent(kept[lastCalls].Content, m.Content)
					continue
				}
				pending += len(m.ToolCalls)
				lastCalls = len(kept)
				kept = append(kept, m)
				continue
			}
			if pending > 0 && lastCalls >= 0 {
				kept[lastCalls].Content = mergeMessageContent(kept[lastCalls].Content, m.Content)
				continue
			}
			kept = append(kept, m)
		case "tool":
			if pending > 0 {
				pending--
			}
			kept = append(kept, m)
		default:
			kept = append(kept, m)
		}
	}
	return kept
}

// mergeMessageContent folds interleaved assistant text into the tool-call
// assistant message. Either side may be nil, a string, or content blocks; the
// first non-empty side is preserved verbatim (so image/file blocks survive),
// and when both carry text the strings are joined with a blank line.
func mergeMessageContent(dst, src any) any {
	if strings.TrimSpace(contentToString(src)) == "" {
		return dst
	}
	if strings.TrimSpace(contentToString(dst)) == "" {
		return src
	}
	return contentToString(dst) + "\n\n" + contentToString(src)
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
