package web

import "encoding/json"

// usageWithCache builds an OpenAI-compatible chat completion usage object that
// records how much of the input was served from the reused upstream M365
// conversation. The prompt_tokens_details.cached_tokens / text_tokens
// breakdown follows the standard usage contract, so downstream relays (e.g.
// sub2api / one-api style gateways) and client apps that read
// usage.prompt_tokens_details.cached_tokens can display cache hits and
// savings.
//
// The cached_tokens value is clamped to the prompt size so the breakdown can
// never produce a negative text_tokens count.
func usageWithCache(pt, ct, cached int64) map[string]any {
	if cached < 0 {
		cached = 0
	}
	if cached > pt {
		cached = pt
	}
	return map[string]any{
		"prompt_tokens":     pt,
		"completion_tokens": ct,
		"total_tokens":      pt + ct,
		"cached_tokens":     cached,
		"prompt_tokens_details": map[string]any{
			"cached_tokens": cached,
			"text_tokens":   pt - cached,
		},
		// Responses-format aliases so relays that consume the chat-completion
		// usage as a generic usage map see the cache details regardless of
		// which spelling they parse.
		"input_tokens":     pt,
		"output_tokens":    ct,
		"input_tokens_details": map[string]any{
			"cached_tokens": cached,
			"text_tokens":   pt - cached,
		},
	}
}

// cachedInputTokens estimates how much of the full logical prompt was already
// held by the reused upstream conversation and therefore was NOT re-sent: the
// difference between the full flattened prompt and the incremental prompt that
// was actually submitted to ChatHub. Returns 0 when the whole prompt was sent
// (fresh conversation, or the request pinned to an existing conversation but
// with full history re-submitted).
func cachedInputTokens(fullPrompt, sentPrompt string) int64 {
	full := EstimateTokens(fullPrompt)
	sent := EstimateTokens(sentPrompt)
	if sent <= 0 || sent >= full {
		return 0
	}
	return full - sent
}

// cachedTokensFromUsage extracts the cached input token count from an
// OpenAI-style usage object, accepting every spelling used across protocols:
// prompt_tokens_details.cached_tokens (Chat Completions),
// input_tokens_details.cached_tokens (Responses / Anthropic), or a top-level
// cached_tokens (Kimi / xAI). Returns 0 when nothing is present.
func cachedTokensFromUsage(usage any) int64 {
	u, ok := usage.(map[string]any)
	if !ok {
		return 0
	}
	for _, detailsKey := range []string{"input_tokens_details", "prompt_tokens_details"} {
		if details, ok := u[detailsKey].(map[string]any); ok {
			if v, ok := details["cached_tokens"]; ok {
				if n := numberToInt64(v); n > 0 {
					return n
				}
			}
		}
	}
	if v, ok := u["cached_tokens"]; ok {
		if n := numberToInt64(v); n > 0 {
			return n
		}
	}
	return 0
}

// numberToInt64 converts float64/int64/int/JSON-number values to int64.
func numberToInt64(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case float64:
		return int64(n)
	case int:
		return int64(n)
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return i
		}
	}
	return 0
}

// withInputCacheDetails stamps the OpenAI Responses / Anthropic style
// input_tokens_details.cached_tokens breakdown onto a usage map. cached is
// clamped to the input size so text_tokens can never go negative.
//
// It also stamps the Chat Completions aliases (prompt_tokens / completion_tokens
// / prompt_tokens_details.cached_tokens) with the same values: downstream
// relays such as sub2api / one-api / new-api and billing panels read the chat
// format even on a Responses endpoint and would otherwise fall back to their own
// token count (showing a different total and no cache) when only input_tokens
// is present.
func withInputCacheDetails(usage map[string]any, cached int64) map[string]any {
	input := int64(0)
	if v, ok := usage["input_tokens"]; ok {
		if n := numberToInt64(v); n > 0 {
			input = n
		}
	}
	output := int64(0)
	if v, ok := usage["output_tokens"]; ok {
		if n := numberToInt64(v); n > 0 {
			output = n
		}
	}
	if cached < 0 {
		cached = 0
	}
	if cached > input {
		cached = input
	}
	details := map[string]any{
		"cached_tokens": cached,
		"text_tokens":   input - cached,
	}
	usage["input_tokens_details"] = details
	// Chat Completions aliases (see doc comment above).
	usage["prompt_tokens"] = input
	usage["completion_tokens"] = output
	usage["total_tokens"] = input + output
	usage["prompt_tokens_details"] = map[string]any{
		"cached_tokens": cached,
		"text_tokens":   input - cached,
	}
	// Top-level cached_tokens: some relays (one-api / Kimi / xAI style) read
	// usage.cached_tokens directly rather than the nested details format.
	usage["cached_tokens"] = cached
	return usage
}
