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
