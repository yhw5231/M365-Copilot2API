package web

import (
	"encoding/json"
	"testing"
)

// goalRoundSample mirrors the exact CALL_TOOL output that killed a goal-loop
// continuation round: the model re-issues update_goal wrapped in the exec
// tool (Codex code mode), which the completed-call dedup pruned, and the held
// tool-shaped response then released zero content (empty_upstream_response).
const goalRoundSample = `CALL_TOOL: exec({"input":"const result = await tools.update_goal({ status: \"complete\" }); text(JSON.stringify(result));"})`

// goalRoundTools mirrors the tool maps the /v1/responses conversion builds for
// the Codex exec custom tool (single "input" string property bridge).
var goalRoundTools = []map[string]any{
	{"type": "custom", "function": map[string]any{
		"name":        "exec",
		"description": "Run JavaScript code to orchestrate/compose tool calls",
		"parameters": map[string]any{
			"type":                 "object",
			"properties":           map[string]any{"input": map[string]any{"type": "string"}},
			"required":             []any{"input"},
			"additionalProperties": false,
		},
	}},
}

func goalRoundCall(t *testing.T) detectedToolCall {
	t.Helper()
	calls, parsed := parseModelToolDecision(goalRoundSample, goalRoundTools, "auto")
	if !parsed || len(calls) != 1 {
		t.Fatalf("valid goal-round CALL_TOOL must parse: parsed=%v calls=%d", parsed, len(calls))
	}
	return calls[0]
}

func TestGoalProtocolExecDedupExemption(t *testing.T) {
	call := goalRoundCall(t)
	l := agentLedger{Completed: []toolEvidence{{
		ID: "call_prior", Name: "exec", Arguments: string(call.Arguments),
		Result: `{"goal":{"status":"complete"}}`,
	}}}
	got := filterCompletedCalls([]detectedToolCall{call}, l)
	if len(got) != 1 {
		t.Fatal("exec-wrapped update_goal must survive the completed-call dedup: the client-side revision guard makes re-invocation safe, and pruning it left the round with zero content")
	}
}

func TestFilterCompletedCallsStillPrunesPlainExec(t *testing.T) {
	args, _ := json.Marshal(map[string]any{"input": "Write-Output 'done'"})
	l := agentLedger{Completed: []toolEvidence{{
		ID: "call_prior", Name: "exec", Arguments: string(args),
		Result: "done",
	}}}
	calls := []detectedToolCall{{Name: "exec", Arguments: args}}
	if got := filterCompletedCalls(calls, l); len(got) != 0 {
		t.Fatalf("non-goal exec repeat must still be pruned, got %d call(s)", len(got))
	}
}

func TestFilterCompletedCallsKeepsDirectGoalTools(t *testing.T) {
	args, _ := json.Marshal(map[string]any{"status": "complete"})
	l := agentLedger{Completed: []toolEvidence{{
		ID: "call_prior", Name: "update_goal", Arguments: string(args), Result: "{}",
	}}}
	calls := []detectedToolCall{{Name: "update_goal", Arguments: args}}
	if got := filterCompletedCalls(calls, l); len(got) != 1 {
		t.Fatal("direct update_goal repeat must stay exempt from the dedup (existing behavior)")
	}
}

func TestToolShapeReleaseTextNeverEmpty(t *testing.T) {
	// A whole-response protocol line strips to nothing: the release must fall
	// back to the original text instead of emitting an empty stream.
	if got := toolShapeReleaseText(goalRoundSample); got != goalRoundSample {
		t.Fatalf("single-line protocol release = %q, want the original text", got)
	}
	// Multi-line responses release only the prose.
	multi := "CALL_TOOL: exec({\"input\":\n\nchecking the files first."
	want := "checking the files first."
	if got := toolShapeReleaseText(multi); got != want {
		t.Fatalf("multi-line release = %q, want %q", got, want)
	}
	// Plain prose is untouched.
	if got := toolShapeReleaseText("just an answer"); got != "just an answer" {
		t.Fatalf("prose modified: %q", got)
	}
}

// TestGoalRoundEndToEndNoEmptyRelease walks the exact failing chain: parse →
// ledger prune → release fallback, asserting a tool call now survives and the
// release path can never produce empty content.
func TestGoalRoundEndToEndNoEmptyRelease(t *testing.T) {
	call := goalRoundCall(t)
	l := agentLedger{Completed: []toolEvidence{{
		ID: "call_prior", Name: "exec", Arguments: string(call.Arguments),
		Result: `{"goal":{"status":"complete"}}`,
	}}}
	calls := filterCompletedCalls([]detectedToolCall{call}, l)
	if len(calls) > 0 {
		return // the call survives; the release path is never reached
	}
	// Regression guard for any future prune rule: the released text must not
	// be empty, otherwise /v1/responses fails with empty_upstream_response.
	if toolShapeReleaseText(goalRoundSample) == "" {
		t.Fatal("release produced empty content — the round would die with empty_upstream_response")
	}
}
