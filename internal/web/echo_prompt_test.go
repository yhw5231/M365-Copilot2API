package web

import (
	"strings"
	"testing"
)

// TestUserEchoCheckPromptExcludesInjectedBlocks guards the gate-3 echo-suppression
// root cause: the flattened full prompt contains a harness-injected runtime-context
// snapshot whose "sandbox" vocabulary would permanently disable the misjudgment
// gate. userEchoCheckPrompt must return only the caller's genuine question.
func TestUserEchoCheckPromptExcludesInjectedBlocks(t *testing.T) {
	messages := []oaiMsg{
		{Role: "system", Content: "You are an AI agent powered by DeepSeek Harness.\nThe DeepSeek Harness implementation checkout is at C:\\..."},
		{Role: "user", Content: "继续解决以下问题：\n\njob_dedup.py 中 semantic_adapter 的类型收窄修复"},
		{Role: "user", Content: "Current runtime context. This snapshot supersedes earlier runtime-context snapshots.\n\nCurrent DSH file policy: danger-full-access. The DSH file sandbox does not restrict file modifications by available operations."},
		{Role: "assistant", Content: "**Selecting next tool**\nChoosing to use the todo_write tool..."},
		{Role: "tool", Content: "Updated todo list", ToolCallID: "fc_1"},
		{Role: "user", Content: "<goal_round>\nObjective: \"继续完成代码库修复\"\nRound: 1/256\n\nContinue working toward the objective in this same session."},
	}
	got := userEchoCheckPrompt(messages)
	if !strings.Contains(got, "job_dedup.py") {
		t.Fatalf("genuine user question lost; got: %q", got)
	}
	if strings.Contains(got, "sandbox") {
		t.Fatalf("runtime-context snapshot leaked into echo prompt (contains sandbox): %q", got)
	}
	if strings.Contains(got, "Current runtime context") {
		t.Fatalf("runtime-context snapshot leaked into echo prompt: %q", got)
	}
	if strings.Contains(got, "<goal_round>") {
		t.Fatalf("goal-round template leaked into echo prompt: %q", got)
	}
	if strings.Contains(got, "Selecting next tool") {
		t.Fatalf("assistant narration leaked into echo prompt: %q", got)
	}
}

// TestUserPromptMentionsWorkspaceNotPoisonedByRuntimeContext proves the gate-3
// root cause end-to-end: workspaceToolMisjudgmentPossible must stay ARMED when
// the user's genuine question is clean, even though the flattened prompt (and
// thus the runtime-context snapshot with "sandbox") is present. Before the fix
// this returned false (gate 3 echo suppression) and the text gate was disabled.
func TestUserPromptMentionsWorkspaceNotPoisonedByRuntimeContext(t *testing.T) {
	messages := []oaiMsg{
		{Role: "system", Content: "You are an AI agent powered by DeepSeek Harness."},
		{Role: "user", Content: "修复 job_dedup.py 的类型收窄问题"},
		{Role: "user", Content: "Current runtime context. This snapshot supersedes earlier runtime-context snapshots.\n\nCurrent DSH file policy: danger-full-access. The DSH file sandbox does not restrict file modifications."},
	}
	full, _ := flattenPromptMessages(messages, nil)
	if !userPromptMentionsWorkspace(full) {
		t.Fatalf("precondition failed: full flattened prompt must contain sandbox echo term")
	}
	echo := userEchoCheckPrompt(messages)
	if userPromptMentionsWorkspace(echo) {
		t.Fatalf("gate 3 echo suppression poisoned by runtime context: clean user question still matches workspace vocabulary: %q", echo)
	}
	tools := []map[string]any{
		{"type": "function", "function": map[string]any{"name": "pwsh", "description": "PowerShell Core"}},
	}
	ledger := agentLedger{Completed: []toolEvidence{
		{ID: "bad", Name: "edit", Arguments: `{"file_path":"x"}`, Result: "Error: old_string was not found", Failed: true},
	}}
	if !workspaceToolMisjudgmentPossible(tools, echo, ledger) {
		t.Fatalf("workspaceToolMisjudgmentPossible must stay armed with clean user question + failed call")
	}
	if workspaceToolMisjudgmentPossible(tools, full, ledger) {
		t.Fatalf("regression: full flattened prompt must disable the gate (sandbox echo term), proving the test exercises the original root cause")
	}
}
