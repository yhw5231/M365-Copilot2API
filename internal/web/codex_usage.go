package web

import (
	"encoding/json"
	"fmt"
	"unicode"

	"m365-copilot2api/internal/chathub"
)

const (
	// usageSourceTiktoken is retained for reporting compatibility only; the
	// BPE tokenizer behind it was skipped (see tokenEstimator).
	usageSourceTiktoken  = "tiktoken_o200k_base_estimate"
	usageSourceHeuristic = "heuristic_character_estimate"

	// These cover visible request framing not represented by literal message text.
	// They are conservative estimates, not ChatHub billing-token claims.
	requestProtocolTokens    = 4
	messageProtocolTokens    = 4
	toolProtocolTokens       = 6
	toolChoiceProtocolTokens = 2
	replyPrimingTokens       = 3
	outputProtocolTokens     = 3
)

func heuristicTokenCount(text string) int {
	ascii, other := 0, 0
	for _, r := range text {
		if unicode.IsSpace(r) {
			continue
		}
		if r <= 0x7f {
			ascii++
		} else {
			other++
		}
	}
	if ascii == 0 && other == 0 {
		return 0
	}
	return (ascii+3)/4 + other
}

type responsesUsageEstimate struct {
	Values map[string]any
	Source string
}

func tokenEstimator(_ string) (func(string) int, string) {
	// BPE tokenization (o200k_base) was deliberately skipped: per-message
	// encoding on large contexts cost seconds for marginal estimate accuracy.
	return heuristicTokenCount, usageSourceHeuristic
}

func serializedTokenCount(v any, count func(string) int) int {
	if s, ok := v.(string); ok {
		return count(s)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return count(fmt.Sprint(v))
	}
	return count(string(b))
}

// estimateResponsesUsage is a local Codex context estimate, never billing data.
func estimateResponsesUsage(model string, input []oaiMsg, tools []chathub.Tool, toolChoice any, output string) responsesUsageEstimate {
	count, source := tokenEstimator(model)
	in := requestProtocolTokens + replyPrimingTokens
	for _, message := range input {
		in += messageProtocolTokens
		in += count(message.Role)
		in += serializedTokenCount(message.Content, count)
		in += count(message.Name)
		in += count(message.ToolCallID)
		for _, call := range message.ToolCalls {
			in += serializedTokenCount(call, count)
		}
	}
	for _, tool := range tools {
		in += toolProtocolTokens + serializedTokenCount(tool, count)
	}
	if toolChoice != nil {
		in += toolChoiceProtocolTokens + serializedTokenCount(toolChoice, count)
	}
	out := count(output)
	if output != "" {
		out += outputProtocolTokens
	}
	return responsesUsageEstimate{Values: map[string]any{"input_tokens": in, "output_tokens": out, "total_tokens": in + out}, Source: source}
}

func localUsageMetadata(source string) map[string]any {
	return map[string]any{
		"usage_source":               source,
		"usage_values_are_estimates": true,
		"usage_estimate_scope":       "visible_request_and_completion",
		"usage_includes":             []string{"message_content", "message_framing", "tool_schemas", "tool_choice", "tool_calls", "completion_framing"},
	}
}

// splitResponsesInputTokens separates the estimated total input into newly
// submitted input and cached history restored through previous_response_id.
// The cached count is clamped so malformed estimates can never produce a
// negative new-input count.
func splitResponsesInputTokens(totalInputTokens, cachedInputTokens int) (newInputTokens, cacheTokens int) {
	if totalInputTokens < 0 {
		totalInputTokens = 0
	}
	if cachedInputTokens < 0 {
		cachedInputTokens = 0
	}
	if cachedInputTokens > totalInputTokens {
		cachedInputTokens = totalInputTokens
	}
	return totalInputTokens - cachedInputTokens, cachedInputTokens
}
