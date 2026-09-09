package web

import (
	"strings"
	"testing"
)

func TestCanonicalToolArgumentsDeduplicateEquivalentJSON(t *testing.T) {
	ledger := agentLedger{Completed: []toolEvidence{{
		Name:      "workspace_write_file",
		Arguments: `{"path":"main.go","content":"x"}`,
	}}}
	if !ledger.hasCompleted("workspace_write_file", ` { "content":"x", "path":"main.go" } `) {
		t.Fatal("equivalent JSON arguments were not deduplicated")
	}
}

func TestFilterCompletedCallsKeepsNewArguments(t *testing.T) {
	ledger := agentLedger{Completed: []toolEvidence{{
		Name:      "workspace_write_file",
		Arguments: `{"path":"main.go","content":"old"}`,
	}}}
	calls := []detectedToolCall{
		{Name: "workspace_write_file", Arguments: []byte(`{"path":"main.go","content":"old"}`)},
		{Name: "workspace_write_file", Arguments: []byte(`{"path":"main.go","content":"new"}`)},
	}
	got := filterCompletedCalls(calls, ledger)
	if len(got) != 1 || string(got[0].Arguments) != `{"path":"main.go","content":"new"}` {
		t.Fatalf("unexpected filtered calls: %#v", got)
	}
}

func TestRouterContextStaysCompact(t *testing.T) {
	ledger := agentLedger{Completed: []toolEvidence{{
		ID: "call_1", Name: "workspace_write_file", Arguments: `{"path":"main.go"}`, Result: "written successfully",
	}}}
	ctx := ledger.RouterContext()
	if len(ctx) > 2000 {
		t.Fatalf("router context unexpectedly large: %d bytes", len(ctx))
	}
	if len(ctx) == 0 {
		t.Fatal("router context is empty")
	}
}

func TestRouterContextTruncatesOversizedArguments(t *testing.T) {
	huge := `{"input":"` + strings.Repeat("x", 200_000) + `"}`
	ledger := agentLedger{Completed: []toolEvidence{{
		ID: "call_1", Name: "exec", Arguments: huge, Result: "Script completed",
	}}}
	ctx := ledger.RouterContext()
	if len(ctx) > 10_000 {
		t.Fatalf("router context not bounded by argument truncation: %d bytes", len(ctx))
	}
	// The full arguments must still exist in the replayed history the answer
	// path sees; the ledger only needs an identifying slice.
	if !strings.Contains(ctx, "[truncated") {
		t.Fatalf("expected truncation marker in router context: %.200s", ctx)
	}
}
