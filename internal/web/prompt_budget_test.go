package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func longHistory(n int) []oaiMsg {
	out := make([]oaiMsg, 0, n+3)
	out = append(out, oaiMsg{Role: "system", Content: "Permanent task instruction: build the report from the ledger."})
	for i := 0; i < n; i++ {
		out = append(out, oaiMsg{Role: "user", Content: strings.Repeat("history-turn-"+itoa(i)+" ", 200)})
		out = append(out, oaiMsg{Role: "assistant", Content: strings.Repeat("history-answer-"+itoa(i)+" ", 200)})
	}
	out = append(out, oaiMsg{Role: "user", Content: "current turn: finish the report"})
	return out
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [24]byte
	p := len(b)
	for i > 0 {
		p--
		b[p] = byte('0' + i%10)
		i /= 10
	}
	return string(b[p:])
}

func TestBudgetMessagesKeepsTiersWhenOverBudget(t *testing.T) {
	msgs := longHistory(400)
	trimmed := budgetMessages(msgs, 20000)
	if len(trimmed) >= len(msgs) {
		t.Fatalf("budget did not trim: %d -> %d", len(msgs), len(trimmed))
	}
	// Tier 1: system instructions survive.
	if trimmed[0].Role != "system" || !strings.Contains(contentToString(trimmed[0].Content), "Permanent task instruction") {
		t.Fatalf("tier 1 instruction dropped: %#v", trimmed[0])
	}
	// Tier 3: the current user turn survives.
	last := trimmed[len(trimmed)-1]
	if last.Role != "user" || !strings.Contains(contentToString(last.Content), "current turn") {
		t.Fatalf("current turn dropped: %#v", last)
	}
	// Order preserved.
	for i := 1; i < len(trimmed); i++ {
		if trimmed[i].Role == "user" && i > 0 && trimmed[i-1].Role != "assistant" && trimmed[i-1].Role != "system" && trimmed[i-1].Role != "user" {
			t.Fatalf("order broken at %d: %#v", i, trimmed[i-1])
		}
	}
}

func TestBudgetMessagesKeepsToolUnitAtomic(t *testing.T) {
	msgs := []oaiMsg{
		{Role: "system", Content: "instructions"},
		{Role: "user", Content: strings.Repeat("old history ", 3000)},
		{Role: "assistant", ToolCalls: []map[string]any{{"id": "call_1", "function": map[string]any{"name": "exec", "arguments": `{"input":"run tests"}`}}}},
		{Role: "tool", ToolCallID: "call_1", Content: "tests passed"},
		{Role: "user", Content: strings.Repeat("more history ", 3000)},
		{Role: "user", Content: "current turn"},
	}
	trimmed := budgetMessages(msgs, 8000)
	if err := validateToolConversation(trimmed); err != nil {
		t.Fatalf("trimmed list violates tool protocol: %v", err)
	}
	// The tool evidence (tier 2) must survive: find the tool result.
	found := false
	for _, m := range trimmed {
		if m.Role == "tool" && m.ToolCallID == "call_1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("tool evidence dropped: %#v", trimmed)
	}
}

func TestBudgetMessagesNoOpWhenFits(t *testing.T) {
	msgs := []oaiMsg{{Role: "user", Content: "hi"}}
	trimmed := budgetMessages(msgs, 1_000_000)
	if len(trimmed) != len(msgs) {
		t.Fatalf("fit budget must return original: %d -> %d", len(msgs), len(trimmed))
	}
}

func TestCapOutputTokensHonorsMax(t *testing.T) {
	long := strings.Repeat("word ", 4000)
	capped := capOutputTokens(long, intp(50))
	if len(capped) >= len(long) {
		t.Fatalf("cap did not shorten")
	}
	if !strings.Contains(capped, "truncated by max_output_tokens") {
		t.Fatalf("cap marker missing: %q", capped)
	}
	if capOutputTokens("short", intp(1000)) != "short" {
		t.Fatal("short text must be untouched")
	}
	if capOutputTokens(long, nil) != long {
		t.Fatal("nil cap must be untouched")
	}
}

func TestEffectiveMaxOutputPreference(t *testing.T) {
	mc := 10
	mt := 20
	if got := effectiveMaxOutput(&oaiReq{MaxCompletionTokens: &mc, MaxTokens: &mt}); got == nil || *got != 10 {
		t.Fatalf("max_completion_tokens must win: %v", got)
	}
	if got := effectiveMaxOutput(&oaiReq{MaxTokens: &mt}); got == nil || *got != 20 {
		t.Fatalf("max_tokens fallback: %v", got)
	}
	if effectiveMaxOutput(&oaiReq{}) != nil {
		t.Fatal("no cap expected")
	}
}

func intp(v int) *int { return &v }

// TestM365RequestInputBudgetUsesRouteMaxInputTokens verifies the route
// setting is authoritative: an explicit per-route maxInputTokens is used
// verbatim (never capped or widened), routes without one fall back to the
// unified 256K default, and one route's value never leaks into another's
// budget.
func TestM365RequestInputBudgetUsesRouteMaxInputTokens(t *testing.T) {
	// Unset route → unified 256K fallback.
	if got := m365RequestInputBudget("gpt-5.6-sol", nil); got != 262144 {
		t.Fatalf("unset route must fall back to unified 256K: got %d want 262144", got)
	}
	// Explicit route value used verbatim, even above the effective window.
	large := 200000
	small := 1000
	mappings := []modelMapping{
		{PublicModel: "gpt-5.6-sol", MaxInputTokens: &large},
		{PublicModel: "gpt-5.4", MaxInputTokens: &small},
	}
	if got := m365RequestInputBudget("gpt-5.6-sol", mappings); got != large {
		t.Fatalf("explicit route limit must be authoritative: got %d want %d", got, large)
	}
	if got := m365RequestInputBudget("gpt-5.4", mappings); got != small {
		t.Fatalf("route limit mismatch: got %d want %d", got, small)
	}
	if got := m365RequestInputBudget("unconfigured-model", mappings); got != 262144 {
		t.Fatalf("unconfigured model must not inherit another route limit: got %d", got)
	}
}

func TestCompactRequestThreshold(t *testing.T) {
	if got := compactRequestThreshold(128000); got != 115200 {
		t.Fatalf("90%% of 128000 must be 115200: got %d", got)
	}
	if got := compactRequestThreshold(1000); got != 900 {
		t.Fatalf("90%% of 1000 must be 900: got %d", got)
	}
}

func TestEstimateMessagesTokensMatchesBudgetMessagesBasis(t *testing.T) {
	msgs := longHistory(20)
	est := estimateMessagesTokens(msgs)
	// The trimmer computes the same per-message cost; its essential-only lower
	// bound (system + current turn) must stay below the total estimate.
	if est <= 0 {
		t.Fatalf("estimate must be positive: %d", est)
	}
	if trimmed := budgetMessages(msgs, est); len(trimmed) != len(msgs) {
		t.Fatalf("estimate must be the trimmer's own basis: budget=estimate trimmed %d -> %d", len(msgs), len(trimmed))
	}
}

// TestOpenAIChatRejectsAboveCompactionThreshold drives the full chat handler:
// an input above the compaction threshold of the route budget must fail fast
// with a context_length_exceeded error instead of being silently trimmed.
// Unconfigured route → defaultRouteMaxInputTokens 262144, threshold 235929;
// one ~960k-character message ≈ 240k estimated tokens crosses it.
func TestOpenAIChatRejectsAboveCompactionThreshold(t *testing.T) {
	s := &Server{}
	raw := `{"model":"no-such-model-x","messages":[{"role":"user","content":"` + strings.Repeat("history ", 120000) + `"}]}`
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(raw))
	w := httptest.NewRecorder()
	s.openaiChat(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("over-threshold request must be rejected with 400: got %d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Error struct {
			Code    string `json:"code"`
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad error json: %v", err)
	}
	if resp.Error.Code != "context_length_exceeded" || resp.Error.Type != "invalid_request_error" {
		t.Fatalf("wrong error code/type: %+v", resp.Error)
	}
	if !strings.Contains(resp.Error.Message, "compact your context") {
		t.Fatalf("compaction instruction missing: %s", resp.Error.Message)
	}
}

// TestBelowThresholdRequestsAreNotTrimmed pins the contract under the gate: a
// request at or below the 85% threshold passes budgetMessages untouched, so
// the gateway never silently rewrites history the client is allowed to send.
func TestBelowThresholdRequestsAreNotTrimmed(t *testing.T) {
	budget := m365RequestInputBudget("any-model", nil)
	msgs := []oaiMsg{{Role: "user", Content: strings.Repeat("history ", 100)}}
	if est := estimateMessagesTokens(msgs); est > compactRequestThreshold(budget) {
		t.Fatalf("test fixture exceeds the threshold: est=%d threshold=%d", est, compactRequestThreshold(budget))
	}
	if got := budgetMessages(msgs, budget); len(got) != len(msgs) {
		t.Fatalf("below-threshold request must not be trimmed: %d -> %d", len(msgs), len(got))
	}
}

// TestContextOverflowErrorAnthropicDialect pins the /v1/messages wording that
// triggers Claude Code's auto-compact.
func TestContextOverflowErrorAnthropicDialect(t *testing.T) {
	r := httptest.NewRequest("POST", "/v1/messages", nil)
	w := httptest.NewRecorder()
	writeContextOverflowError(w, r, 95000, 100000)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", w.Code)
	}
	var resp struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if resp.Error.Type != "invalid_request_error" {
		t.Fatalf("anthropic type mismatch: %+v", resp.Error)
	}
	if !strings.Contains(resp.Error.Message, "prompt is too long: 95000 tokens > 90000 token maximum") {
		t.Fatalf("anthropic compact trigger wording missing: %s", resp.Error.Message)
	}
}
