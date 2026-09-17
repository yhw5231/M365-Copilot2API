package web

import (
	"testing"
)

// TestResponsesAdapterPreservesGoalLoopSignals verifies that the /v1/responses
// adapter conversion (responsesRequest.openAI) keeps every signal the goal-loop
// breaker (forceGoalRoundToolChoice -> tool_choice=required) depends on:
// the <goal_round> user message, the Round: N/M counter, and the declared tools.
// Without them the fix silently cannot engage on the Responses protocol.
func TestResponsesAdapterPreservesGoalLoopSignals(t *testing.T) {
	req := responsesRequest{
		Model: "gpt-5.6-luna",
		Input: []any{
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": "修复 mypy"}}},
			map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "目标尚未完成，现有证据确认..."}}},
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": "<goal_round>\nObjective: \"修复 mypy\"\nRound: 2/256\nContinue..."}}},
		},
		Tools: []map[string]any{
			{"type": "function", "name": "pwsh", "description": "shell", "parameters": map[string]any{"type": "object", "properties": map[string]any{}}},
			{"type": "function", "name": "create_goal", "description": "goal", "parameters": map[string]any{"type": "object", "properties": map[string]any{}}},
		},
	}
	o, err := req.openAI()
	if err != nil {
		t.Fatalf("openAI conversion failed: %v", err)
	}
	if len(o.Tools) == 0 {
		t.Fatal("Responses adapter dropped the tool declarations — forceGoalRoundToolChoice cannot engage")
	}
	if !goalRoundRequest(o.Messages, &taskLedger{GoalID: "g1"}, o.Tools) {
		t.Fatal("Responses adapter lost the <goal_round>/Round signal — goal-loop breaker cannot detect the round")
	}
	if !forceGoalRoundToolChoice(o.Messages, &taskLedger{GoalID: "g1"}, o.Tools) {
		t.Fatal("Responses adapter conversion defeats forceGoalRoundToolChoice (text-only previous reply must force required)")
	}
}

// TestResponsesAdapterReasoningFragmentIsNotAStatusReport pins the DSH replay
// shape that used to misread the goal-loop breaker: the harness replays a tool
// round as a text-only output_text reasoning fragment immediately followed by
// the round's function_call item. After conversion, the fragment is an
// assistant message WITHOUT ToolCalls directly preceding an assistant message
// WITH ToolCalls — forceGoalRoundToolChoice must treat the fragment as the
// reasoning OF the call, not as a text-only status report.
func TestResponsesAdapterReasoningFragmentIsNotAStatusReport(t *testing.T) {
	req := responsesRequest{
		Model: "gpt-5.6-luna",
		Input: []any{
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": "<goal_round>\nObjective: \"修复 mypy\"\nRound: 2/256\nContinue..."}}},
			map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "<thinking>need to run the full regression</thinking>"}}},
			map[string]any{"type": "function_call", "call_id": "fc_1", "name": "pwsh", "arguments": `{"command":"python -m pytest -q"}`},
			map[string]any{"type": "function_call_output", "call_id": "fc_1", "output": "370 passed"},
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": "Background subagent 3aa7bc98 finished and will do no further work unless you send it more.Its closing message:# Web 管理控制台用户体验审查结论"}}},
		},
		Tools: []map[string]any{
			{"type": "function", "name": "pwsh", "description": "shell", "parameters": map[string]any{"type": "object", "properties": map[string]any{}}},
			{"type": "function", "name": "create_goal", "description": "goal", "parameters": map[string]any{"type": "object", "properties": map[string]any{}}},
		},
	}
	o, err := req.openAI()
	if err != nil {
		t.Fatalf("openAI conversion failed: %v", err)
	}
	if len(o.Messages) < 4 {
		t.Fatalf("conversion dropped messages: %d", len(o.Messages))
	}
	if forceGoalRoundToolChoice(o.Messages, &taskLedger{GoalID: "g1"}, o.Tools) {
		t.Fatal("reasoning fragment of a tool round was misread as a text-only status report — must not force required")
	}
}

// TestAnthropicAdapterPreservesGoalLoopSignals is the same guarantee for the
// /v1/messages (Anthropic) protocol path.
func TestAnthropicAdapterPreservesGoalLoopSignals(t *testing.T) {
	req := anthropicRequest{
		Model: "gpt-5.6-luna",
		Messages: []anthropicMessage{
			{Role: "user", Content: "修复 mypy"},
			{Role: "assistant", Content: "目标尚未完成，现有证据确认..."},
			{Role: "user", Content: "<goal_round>\nObjective: \"修复 mypy\"\nRound: 2/256\nContinue..."},
		},
		Tools: []anthropicTool{
			{Name: "pwsh", Description: "shell", InputSchema: map[string]any{"type": "object", "properties": map[string]any{}}},
			{Name: "create_goal", Description: "goal", InputSchema: map[string]any{"type": "object", "properties": map[string]any{}}},
		},
	}
	o, err := req.openAI()
	if err != nil {
		t.Fatalf("openAI conversion failed: %v", err)
	}
	if len(o.Tools) == 0 {
		t.Fatal("Anthropic adapter dropped the tool declarations — forceGoalRoundToolChoice cannot engage")
	}
	if !goalRoundRequest(o.Messages, &taskLedger{GoalID: "g1"}, o.Tools) {
		t.Fatal("Anthropic adapter lost the <goal_round>/Round signal — goal-loop breaker cannot detect the round")
	}
	if !forceGoalRoundToolChoice(o.Messages, &taskLedger{GoalID: "g1"}, o.Tools) {
		t.Fatal("Anthropic adapter conversion defeats forceGoalRoundToolChoice (text-only previous reply must force required)")
	}
}
