package web

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
		"prompt_tokens_details": map[string]any{
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
