package web

import (
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

func TestM365EffectiveContextWindowDefault(t *testing.T) {
	t.Setenv("M365_EFFECTIVE_CONTEXT_WINDOW", "")
	if got := m365EffectiveContextWindow(); got <= 0 {
		t.Fatalf("default effective window must be positive: %d", got)
	}
	t.Setenv("M365_EFFECTIVE_CONTEXT_WINDOW", "120000")
	if got := m365EffectiveContextWindow(); got != 120000 {
		t.Fatalf("env override not honored: %d", got)
	}
}

func TestModelRouteMaxInputTokensDefaultsTo256K(t *testing.T) {
	if got := modelRouteMaxInputTokens("gpt-5.6-sol", nil); got != 262144 {
		t.Fatalf("missing route value must use unified 256K default: got %d", got)
	}
	mappings := []modelMapping{{PublicModel: "gpt-5.6-sol"}}
	if got := modelRouteMaxInputTokens("gpt-5.6-sol", mappings); got != 262144 {
		t.Fatalf("nil route value must use unified 256K default: got %d", got)
	}
}

func TestModelRouteMaxInputTokensAreRouteLocal(t *testing.T) {
	limit256 := 262144
	limit1000 := 1000
	mappings := []modelMapping{
		{PublicModel: "gpt-5.6-sol", MaxInputTokens: &limit256},
		{PublicModel: "gpt-5.4", MaxInputTokens: &limit1000},
	}
	if got := modelRouteMaxInputTokens("gpt-5.6-sol", mappings); got != limit256 {
		t.Fatalf("gpt-5.6-sol route limit mismatch: got %d want %d", got, limit256)
	}
	if got := modelRouteMaxInputTokens("gpt-5.4", mappings); got != limit1000 {
		t.Fatalf("gpt-5.4 route limit mismatch: got %d want %d", got, limit1000)
	}
	if got := modelRouteMaxInputTokens("unconfigured-model", mappings); got != 262144 {
		t.Fatalf("unconfigured model must not inherit another route limit: got %d", got)
	}
}
