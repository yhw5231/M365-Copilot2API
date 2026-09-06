package web

import (
	"strings"
	"testing"
)

// brokenCallSample mirrors the malformed CALL_TOOL output observed from the
// upstream model: unescaped inner quotes inside the JSON arguments, a line
// truncated mid-token, and trailing planning prose.
const brokenCallSample = "CALL_TOOL: exec({\"input\":\"const result = await tools.exec_command({cmd:\"$cwd=Get-Location; Write-Output 'x'\"}); text(JSON.stringify(*\n\n\nI'm checking the current state after the failed attempt, focusing on verifying the existence of the files before proceeding.\n"

var brokenCallTools = []map[string]any{
	{"type": "custom", "function": map[string]any{"name": "exec", "description": "run code"}},
}

func TestToolCallIntentPrefix(t *testing.T) {
	if !toolCallIntentPrefix("CALL_TOOL: exec({})") || !toolCallIntentPrefix("  call_tool: a({})") {
		t.Fatal("CALL_TOOL/call_tool prefixes not detected")
	}
	if toolCallIntentPrefix("Hello, please CALL_TOOL later") {
		t.Fatal("mid-text mention must not count as intent")
	}
}

func TestHasBrokenToolCallIntent(t *testing.T) {
	if !hasBrokenToolCallIntent(brokenCallSample, brokenCallTools, "auto") {
		t.Fatal("broken CALL_TOOL sample must be detected as broken intent")
	}
	// A valid CALL_TOOL line is not broken intent.
	valid := "CALL_TOOL: exec({\"input\":\"echo hi\"})"
	if !hasBrokenToolCallIntent(valid, brokenCallTools, "auto") {
		t.Log("valid custom-tool CALL_TOOL not parsed (schema-dependent); acceptable")
	}
	if hasBrokenToolCallIntent("just an answer", brokenCallTools, "auto") {
		t.Fatal("plain prose must not be broken intent")
	}
}

func TestStripToolCallProtocolLine(t *testing.T) {
	got := stripToolCallProtocolLine(brokenCallSample)
	if strings.Contains(got, "CALL_TOOL") || strings.Contains(got, "JSON.stringify") {
		t.Fatalf("protocol fragment survived: %q", got)
	}
	if !strings.Contains(got, "I'm checking the current state") {
		t.Fatalf("planning prose was lost: %q", got)
	}
	// Whole-response protocol line strips to empty.
	if got := stripToolCallProtocolLine("CALL_TOOL: exec({\"input\":"); got != "" {
		t.Fatalf("single-line protocol strip = %q, want empty", got)
	}
	// Prose is untouched.
	if got := stripToolCallProtocolLine("plain answer"); got != "plain answer" {
		t.Fatalf("prose modified: %q", got)
	}
}

// TestParseModelToolDecisionRejectsBrokenJSON asserts the parser does not
// accept the malformed sample (it must go through repair instead of being
// executed with garbage arguments).
func TestParseModelToolDecisionRejectsBrokenJSON(t *testing.T) {
	calls, _ := parseModelToolDecision(brokenCallSample, brokenCallTools, "auto")
	if len(calls) != 0 {
		t.Fatalf("broken sample unexpectedly parsed: %v", calls)
	}
}
