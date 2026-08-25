package web

import "testing"

func TestUsageWithCacheBreakdown(t *testing.T) {
	u := usageWithCache(100, 20, 70)
	if u["prompt_tokens"].(int64) != 100 || u["completion_tokens"].(int64) != 20 || u["total_tokens"].(int64) != 120 {
		t.Fatalf("usage totals wrong: %#v", u)
	}
	det, ok := u["prompt_tokens_details"].(map[string]any)
	if !ok {
		t.Fatalf("missing prompt_tokens_details: %#v", u)
	}
	if det["cached_tokens"].(int64) != 70 || det["text_tokens"].(int64) != 30 {
		t.Fatalf("prompt_tokens_details wrong: %#v", det)
	}
}

func TestUsageWithCacheClampsCachedToPrompt(t *testing.T) {
	u := usageWithCache(50, 10, 999)
	det := u["prompt_tokens_details"].(map[string]any)
	if det["cached_tokens"].(int64) != 50 || det["text_tokens"].(int64) != 0 {
		t.Fatalf("clamp failed: %#v", det)
	}
	u = usageWithCache(50, 10, -5)
	det = u["prompt_tokens_details"].(map[string]any)
	if det["cached_tokens"].(int64) != 0 || det["text_tokens"].(int64) != 50 {
		t.Fatalf("negative clamp failed: %#v", det)
	}
}

func TestUsageWithCacheZeroCachedStillEmitsDetails(t *testing.T) {
	u := usageWithCache(40, 8, 0)
	det := u["prompt_tokens_details"].(map[string]any)
	if det["cached_tokens"].(int64) != 0 || det["text_tokens"].(int64) != 40 {
		t.Fatalf("zero-cache details wrong: %#v", det)
	}
}

func TestCachedInputTokensIncrementalSend(t *testing.T) {
	full := "system: rules\nuser: q1\nassistant: a1\nuser: q2"
	inc := "user: q2"
	cached := cachedInputTokens(full, inc)
	if cached <= 0 {
		t.Fatalf("incremental send must report positive cached tokens, got %d", cached)
	}
	fullTokens := EstimateTokens(full)
	incTokens := EstimateTokens(inc)
	if cached != fullTokens-incTokens {
		t.Fatalf("cached=%d want %d", cached, fullTokens-incTokens)
	}
}

func TestCachedInputTokensFullSendIsZero(t *testing.T) {
	full := "user: hello"
	if got := cachedInputTokens(full, full); got != 0 {
		t.Fatalf("full send must be 0 cached, got %d", got)
	}
	if got := cachedInputTokens("", ""); got != 0 {
		t.Fatalf("empty inputs must be 0 cached, got %d", got)
	}
	if got := cachedInputTokens(full, full+" extra"); got != 0 {
		t.Fatalf("sent >= full must be 0 cached, got %d", got)
	}
}

func TestCachedTokensFromUsageSpellings(t *testing.T) {
	cases := []struct {
		name  string
		usage any
		want  int64
	}{
		{"chat completions prompt_tokens_details", map[string]any{"prompt_tokens_details": map[string]any{"cached_tokens": float64(70)}}, 70},
		{"responses input_tokens_details", map[string]any{"input_tokens_details": map[string]any{"cached_tokens": float64(12)}}, 12},
		{"top-level cached_tokens", map[string]any{"cached_tokens": float64(5)}, 5},
		{"input_tokens_details wins", map[string]any{"input_tokens_details": map[string]any{"cached_tokens": float64(9)}, "prompt_tokens_details": map[string]any{"cached_tokens": float64(3)}}, 9},
		{"missing", map[string]any{"prompt_tokens": 10}, 0},
		{"zero", map[string]any{"prompt_tokens_details": map[string]any{"cached_tokens": float64(0)}}, 0},
		{"not a map", "usage", 0},
		{"nil", nil, 0},
	}
	for _, tc := range cases {
		if got := cachedTokensFromUsage(tc.usage); got != tc.want {
			t.Fatalf("%s: got %d want %d", tc.name, got, tc.want)
		}
	}
}

func TestWithInputCacheDetailsClampsToInput(t *testing.T) {
	usage := map[string]any{"input_tokens": float64(100), "output_tokens": float64(10)}
	withInputCacheDetails(usage, 30)
	det := usage["input_tokens_details"].(map[string]any)
	if det["cached_tokens"].(int64) != 30 || det["text_tokens"].(int64) != 70 {
		t.Fatalf("details wrong: %#v", det)
	}

	usage2 := map[string]any{"input_tokens": float64(50)}
	withInputCacheDetails(usage2, 999)
	det2 := usage2["input_tokens_details"].(map[string]any)
	if det2["cached_tokens"].(int64) != 50 || det2["text_tokens"].(int64) != 0 {
		t.Fatalf("clamp wrong: %#v", det2)
	}

	usage3 := map[string]any{"input_tokens": float64(50)}
	withInputCacheDetails(usage3, -3)
	det3 := usage3["input_tokens_details"].(map[string]any)
	if det3["cached_tokens"].(int64) != 0 || det3["text_tokens"].(int64) != 50 {
		t.Fatalf("negative wrong: %#v", det3)
	}
}
