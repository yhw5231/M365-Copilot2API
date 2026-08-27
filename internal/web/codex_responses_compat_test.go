package web

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"m365-copilot2api/internal/chathub"
)

func TestParseContentAcceptsResponsesTextBlocks(t *testing.T) {
	content := []any{
		map[string]any{"type": "input_text", "text": "input"},
		map[string]any{"type": "output_text", "text": " output"},
	}
	text, files := parseContent(content)
	if text != "input output" || len(files) != 0 {
		t.Fatalf("text=%q files=%#v", text, files)
	}
}

func TestResponsesUsageEstimateIsNonZeroForText(t *testing.T) {
	usage := estimateResponsesUsage("gpt-5.5", []oaiMsg{{Role: "user", Content: "hello"}}, nil, nil, "world").Values
	if usage["input_tokens"].(int) <= 0 || usage["output_tokens"].(int) <= 0 || usage["total_tokens"].(int) <= 0 {
		t.Fatalf("usage=%#v", usage)
	}
}

func TestResponsesGPTUsageSkipsBpeTokenizer(t *testing.T) {
	// BPE tokenization was deliberately skipped; gpt-* models use the same
	// heuristic estimator as every other model, reported as such.
	input := "杩欐槸鐢ㄤ簬楠岃瘉 GPT tokenizer 鐨勪腑鏂囧拰 code: func main() {}"
	estimate := estimateResponsesUsage("gpt-5.5", []oaiMsg{{Role: "user", Content: input}}, nil, nil, "")
	if estimate.Source != usageSourceHeuristic {
		t.Fatalf("source=%q, want heuristic (BPE tokenizer skipped)", estimate.Source)
	}
	if estimate.Values["input_tokens"].(int) <= 0 || estimate.Values["total_tokens"].(int) <= 0 {
		t.Fatalf("estimate=%#v", estimate)
	}
}

func TestResponsesUnknownModelUsesHeuristicFallback(t *testing.T) {
	estimate := estimateResponsesUsage("claude-sonnet", []oaiMsg{{Role: "user", Content: "hello"}}, nil, nil, "")
	if estimate.Source != usageSourceHeuristic {
		t.Fatalf("source=%q", estimate.Source)
	}
}

func TestResponsesUsageIncludesToolSchemaAndChoice(t *testing.T) {
	base := estimateResponsesUsage("gpt-5.5", []oaiMsg{{Role: "user", Content: "weather"}}, nil, nil, "")
	tools := []chathub.Tool{{Type: "function", Function: json.RawMessage(`{"name":"weather","description":"Get weather","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}`)}}
	withTools := estimateResponsesUsage("gpt-5.5", []oaiMsg{{Role: "user", Content: "weather"}}, tools, map[string]any{"type": "function", "name": "weather"}, "")
	if withTools.Values["input_tokens"].(int) <= base.Values["input_tokens"].(int) {
		t.Fatalf("tool schema and choice were not counted: base=%#v tools=%#v", base, withTools)
	}
}

func TestResponsesResultIncludesUsage(t *testing.T) {
	rr := httptest.NewRecorder()
	writeResponsesResult(rr, "gpt-5.5", false, map[string]any{
		"choices":           []any{map[string]any{"message": map[string]any{"content": "hello"}}},
		"usage":             estimateResponsesUsage("gpt-5.5", []oaiMsg{{Role: "user", Content: "prompt"}}, nil, nil, "hello").Values,
		"m365_usage_source": usageSourceTiktoken,
	})
	var response map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	usage, ok := response["usage"].(map[string]any)
	if !ok || usage["total_tokens"].(float64) <= 0 {
		t.Fatalf("missing usage: %#v", response)
	}
	m365, ok := response["m365"].(map[string]any)
	if !ok || m365["usage_source"] != usageSourceTiktoken || m365["usage_estimate_scope"] != "visible_request_and_completion" {
		t.Fatalf("missing usage source: %#v", response)
	}
}

func TestStreamingResponsesResultIncludesUsage(t *testing.T) {
	rr := httptest.NewRecorder()
	writeResponsesResult(rr, "gpt-5.5", true, map[string]any{
		"choices":           []any{map[string]any{"message": map[string]any{"content": "hello"}}},
		"usage":             estimateResponsesUsage("gpt-5.5", []oaiMsg{{Role: "user", Content: "prompt"}}, nil, nil, "hello").Values,
		"m365_usage_source": usageSourceTiktoken,
	})
	body := rr.Body.String()
	if !strings.Contains(body, "event: response.completed") || !strings.Contains(body, `"total_tokens":`) || !strings.Contains(body, usageSourceTiktoken) {
		t.Fatalf("stream completion missing usage: %s", body)
	}
}

func TestResponsesResultIncludesReasoningItem(t *testing.T) {
	rr := httptest.NewRecorder()
	writeResponsesResult(rr, "gpt-5.6-reasoning", false, map[string]any{
		"choices": []any{map[string]any{"message": map[string]any{"content": "answer", "reasoning_content": "think step by step"}}},
		"usage":   estimateResponsesUsage("gpt-5.6-reasoning", []oaiMsg{{Role: "user", Content: "prompt"}}, nil, nil, "answer").Values,
	})
	var response map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	output, _ := response["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("expected reasoning + message output, got %#v", output)
	}
	rs, _ := output[0].(map[string]any)
	if rs["type"] != "reasoning" {
		t.Fatalf("first output item must be reasoning, got %#v", rs)
	}
	summary, _ := rs["summary"].([]any)
	if len(summary) != 1 {
		t.Fatalf("reasoning item missing summary: %#v", rs)
	}
	s, _ := summary[0].(map[string]any)
	if s["text"] != "think step by step" {
		t.Fatalf("reasoning summary text=%v", s["text"])
	}
	msg, _ := output[1].(map[string]any)
	if msg["type"] != "message" {
		t.Fatalf("second output item must be message, got %#v", msg)
	}
}

func TestStreamingResponsesResultEmitsReasoningDelta(t *testing.T) {
	rr := httptest.NewRecorder()
	writeResponsesResult(rr, "gpt-5.6-reasoning", true, map[string]any{
		"choices": []any{map[string]any{"message": map[string]any{"content": "answer", "reasoning_content": "think step by step"}}},
		"usage":   estimateResponsesUsage("gpt-5.6-reasoning", []oaiMsg{{Role: "user", Content: "prompt"}}, nil, nil, "answer").Values,
	})
	body := rr.Body.String()
	if !strings.Contains(body, "event: response.reasoning_summary_text.delta") || !strings.Contains(body, `"delta":"think step by step"`) {
		t.Fatalf("stream missing reasoning delta: %s", body)
	}
	if !strings.Contains(body, `"type":"reasoning"`) {
		t.Fatalf("stream missing reasoning item: %s", body)
	}
}

func TestResponsesOutputHasContentCountsReasoning(t *testing.T) {
	if !responsesOutputHasContent(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": "", "reasoning_content": "   think  "}}}}) {
		t.Fatal("expected reasoning-only to count as content")
	}
	if !responsesOutputHasContent(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": "hi"}}}}) {
		t.Fatal("expected text content to count")
	}
	if responsesOutputHasContent(map[string]any{"choices": []any{map[string]any{"message": map[string]any{}}}}) {
		t.Fatal("expected empty message not to count")
	}
}

func TestSplitResponsesInputTokensForPreviousResponse(t *testing.T) {
	newInput, cachedInput := splitResponsesInputTokens(1250, 1000)
	if newInput != 250 || cachedInput != 1000 {
		t.Fatalf("splitResponsesInputTokens() = new %d, cached %d; want new 250, cached 1000", newInput, cachedInput)
	}

	cost := estimateUsageCost("gpt-5.2", int64(newInput), 50, int64(cachedInput))
	want := float64(250)/1_000_000*1.75 +
		float64(1000)/1_000_000*0.175 +
		float64(50)/1_000_000*14.00
	if cost != want {
		t.Fatalf("incremental Responses cost = %.12f, want %.12f", cost, want)
	}
}

func TestSplitResponsesInputTokensClampsInvalidCachedEstimate(t *testing.T) {
	tests := []struct {
		name                string
		total, cached       int
		wantNew, wantCached int
	}{
		{name: "cached exceeds total", total: 100, cached: 200, wantNew: 0, wantCached: 100},
		{name: "negative cached", total: 100, cached: -10, wantNew: 100, wantCached: 0},
		{name: "negative total", total: -10, cached: 5, wantNew: 0, wantCached: 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotNew, gotCached := splitResponsesInputTokens(tc.total, tc.cached)
			if gotNew != tc.wantNew || gotCached != tc.wantCached {
				t.Fatalf("splitResponsesInputTokens(%d, %d) = (%d, %d), want (%d, %d)", tc.total, tc.cached, gotNew, gotCached, tc.wantNew, tc.wantCached)
			}
		})
	}
}
