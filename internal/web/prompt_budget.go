package web

import (
	"log"
	"strings"
)

// Context is managed in three tiers so trimming a long conversation can never
// destroy the model's understanding of what the task is:
//
//	tier 1 — permanent task instructions and user constraints (system/developer)
//	tier 2 — the current task ledger and tool evidence (tool results + calls)
//	tier 3 — ordinary user/assistant history (trimmed first)
//
// The gateway manages requests under a route-configured input budget instead
// of letting them grow unbounded, and advertises that same budget honestly in
// /v1/models. A model route's configured maxInputTokens is the enforced budget
// for that route; routes without one share the unified 256K default. (The
// upstream itself tolerates far more — the context-length probe measured
// ~838K real tokens accepted — so the budget is an operational choice, not an
// upstream constraint.) Requests above the compaction threshold are rejected
// with a compaction request; budgetMessages below it is a defensive no-op.

const (
	// defaultRouteMaxInputTokens is the input budget for model routes without
	// their own maxInputTokens setting. 256K tokens matches what the upstream
	// can actually consume (probe: ~838K real tokens ≈ 1M estimated).
	defaultRouteMaxInputTokens = 262144
	// compactRequestThresholdPercent is the share of the input budget at which
	// the gateway stops accepting a request and instead answers with a
	// context-overflow error that asks the client to compact. Agent clients
	// (Claude Code, Codex CLI, ...) run their own context compaction on that
	// error and resend a smaller request, which preserves their summaries
	// instead of the gateway silently dropping oldest history.
	compactRequestThresholdPercent = 90
)

// m365RequestInputBudget returns the input-token budget the gateway actually
// enforces for one request. The model route's configured MaxInputTokens is
// authoritative when set — the gateway never caps or widens an explicit
// per-route value. Routes without their own limit fall back to the unified
// 256K default. This same value is what /v1/models advertises for the route
// and what budgetMessages trims against: enforcement, advertisement, and
// defensive trimming share one source of truth.
func m365RequestInputBudget(model string, mappings []modelMapping) int {
	if mapping, ok := configuredModelMapping(model, mappings); ok && mapping.MaxInputTokens != nil && *mapping.MaxInputTokens > 0 {
		return *mapping.MaxInputTokens
	}
	return defaultRouteMaxInputTokens
}

// compactRequestThreshold is the largest accepted input-token estimate for one
// request: above it openaiChat rejects with a compaction-required error.
func compactRequestThreshold(budget int) int {
	return budget * compactRequestThresholdPercent / 100
}

// estimateMessagesTokens mirrors the per-message accounting budgetMessages
// uses (content text + 4 protocol tokens per message), so the compaction gate
// and the defensive trimmer measure a request on exactly the same basis.
// Multimodal image parts collapse to short placeholders, matching the trimmer.
func estimateMessagesTokens(msgs []oaiMsg) int {
	tokens := make([]int, len(msgs))
	total := 0
	for i, m := range msgs {
		tokens[i] = int(EstimateTokens(contentToString(m.Content))) + 4
		total += tokens[i]
	}
	return total
}

// budgetMessages trims a message list to fit maxTokens without violating the
// three-tier rule or the OpenAI tool protocol:
//   - tier 1 (system/developer) is always kept;
//   - tier 2 (tool results and the assistant calls they answer) is always kept
//     as an atomic unit, so trim can never leave a dangling tool result;
//   - tier 3 ordinary history is dropped oldest-first, newest kept first, and
//     the current user turn (tail from the last user message) is never dropped.
//
// When the list already fits, the original slice is returned untouched so
// session prefix matching is unaffected.
func budgetMessages(msgs []oaiMsg, maxTokens int) []oaiMsg {
	if maxTokens <= 0 || len(msgs) == 0 {
		return msgs
	}
	tokens := make([]int, len(msgs))
	total := int64(0)
	essential := make([]bool, len(msgs))
	for i, m := range msgs {
		tokens[i] = int(EstimateTokens(contentToString(m.Content))) + 4
		total += int64(tokens[i])
		role := strings.ToLower(strings.TrimSpace(m.Role))
		switch role {
		case "system", "developer":
			essential[i] = true // tier 1: permanent instructions
		case "tool":
			essential[i] = true // tier 2: tool evidence
		}
	}
	if total <= int64(maxTokens) {
		return msgs
	}
	// Assistant tool-call messages are tier 2 evidence too: their call ids must
	// survive so the paired tool results stay protocol-valid. Mark the last
	// assistant-with-toolcalls unit as essential regardless of position.
	for i := len(msgs) - 1; i >= 0; i-- {
		if strings.ToLower(strings.TrimSpace(msgs[i].Role)) == "assistant" && len(msgs[i].ToolCalls) > 0 {
			essential[i] = true
			// Everything from this assistant message onward belongs to the same
			// open tool round; keep the whole unit.
			for j := i + 1; j < len(msgs); j++ {
				if strings.ToLower(strings.TrimSpace(msgs[j].Role)) == "tool" {
					essential[j] = true
				} else {
					break
				}
			}
			break
		}
	}
	// Goal tool evidence (create_goal/get_goal/update_goal) is critical task
	// state: it carries the goal id and completion status the server-side ledger
	// mirrors. Keep these calls even in old history so a trimmed request can
	// still recover the goal identity when the ledger is rebuilt (their paired
	// tool results are already tier-2 essential above).
	for i, m := range msgs {
		if strings.ToLower(strings.TrimSpace(m.Role)) != "assistant" || len(m.ToolCalls) == 0 {
			continue
		}
		for _, raw := range m.ToolCalls {
			fn, _ := raw["function"].(map[string]any)
			name, _ := fn["name"].(string)
			switch strings.ToLower(name) {
			case "create_goal", "get_goal", "update_goal":
				essential[i] = true
			}
		}
	}
	// The current turn tail (from the last user message) is never trimmed.
	lastUser := -1
	for i := len(msgs) - 1; i >= 0; i-- {
		if strings.ToLower(strings.TrimSpace(msgs[i].Role)) == "user" {
			lastUser = i
			break
		}
	}
	for i := lastUser; i < len(msgs); i++ {
		if i >= 0 {
			essential[i] = true
		}
	}
	kept := make([]bool, len(msgs))
	essentialTotal := int64(0)
	for i := range msgs {
		if essential[i] {
			kept[i] = true
			essentialTotal += int64(tokens[i])
		}
	}
	if essentialTotal > int64(maxTokens) {
		// Even instructions and evidence exceed the budget. Never drop tier 1/2:
		// surface the overflow and ship the core sections as-is.
		log.Printf("[prompt-budget] essential instructions/evidence exceed budget %d (essential=%d); preserving all core sections", maxTokens, essentialTotal)
		return compactEssentialMessages(msgs, kept)
	}
	remain := int64(maxTokens) - essentialTotal
	// Newest tier-3 history first while budget lasts.
	for i := len(msgs) - 1; i >= 0; i-- {
		if kept[i] || essential[i] {
			continue
		}
		if int64(tokens[i]) <= remain {
			kept[i] = true
			remain -= int64(tokens[i])
		}
	}
	return compactEssentialMessages(msgs, kept)
}

// compactEssentialMessages rebuilds the trimmed list in the original order.
func compactEssentialMessages(msgs []oaiMsg, kept []bool) []oaiMsg {
	out := make([]oaiMsg, 0, len(msgs))
	for i, m := range msgs {
		if kept[i] {
			out = append(out, m)
		}
	}
	// A trimmed list must still start with an allowed role (never a bare tool
	// result): the assistant tool-call unit is kept atomically, but guard any
	// edge case where the head ends up being a tool message, so the first
	// non-system message is always a user or assistant turn.
	for len(out) > 0 {
		r := strings.ToLower(strings.TrimSpace(out[0].Role))
		if r == "tool" {
			out = out[1:]
			continue
		}
		break
	}
	return out
}

// effectiveMaxOutput returns the request's output cap, preferring
// max_completion_tokens (OpenAI) and falling back to legacy max_tokens.
func effectiveMaxOutput(body *oaiReq) *int {
	if body == nil {
		return nil
	}
	if body.MaxCompletionTokens != nil {
		return body.MaxCompletionTokens
	}
	return body.MaxTokens
}

// capOutputTokens honors max_output_tokens / max_completion_tokens / max_tokens
// by truncating the final assistant text to the requested budget. It is applied
// only to the final answer (never to a tool-call response), so a client that
// sets an explicit output cap actually gets one instead of a silently ignored
// parameter. Rough 4-char-per-token mapping matches the gateway's local token
// estimator (EstimateTokens).
func capOutputTokens(text string, maxTokens *int) string {
	if maxTokens == nil || *maxTokens <= 0 || text == "" {
		return text
	}
	maxChars := *maxTokens * 4
	if len(text) <= maxChars {
		return text
	}
	cut := maxChars
	head := text[:cut]
	if i := strings.LastIndex(head, "\n\n"); i > cut/2 {
		cut = i
	} else if i := strings.LastIndex(head, ". "); i > cut/2 {
		cut = i + 1
	}
	return strings.TrimSpace(text[:cut]) + " …[output truncated by max_output_tokens]"
}
