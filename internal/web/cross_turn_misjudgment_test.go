package web

import "testing"

// TestActiveMessagesSlicesAtLastUserMessage verifies that activeMessages cuts at
// the last user message, so tool evidence from earlier turns is excluded from
// the active-turn ledger used by the workspace/tool misjudgment gate.
func TestActiveMessagesSlicesAtLastUserMessage(t *testing.T) {
	msgs := []oaiMsg{
		{Role: "system", Content: "You are an AI agent powered by DeepSeek Harness."},
		{Role: "user", Content: "第一轮：修复这个项目"},
		{Role: "assistant", ToolCalls: []map[string]any{
			{"id": "c1", "type": "function", "function": map[string]any{"name": "pwsh", "arguments": `{"cmd":"dir"}`}},
		}},
		{Role: "tool", ToolCallID: "c1", Content: "ok"},
		{Role: "user", Content: "第二轮：重新验证以下项目"},
	}
	active := activeMessages(msgs)
	if len(active) != 1 {
		t.Fatalf("activeMessages = %d messages, want 1 (only the new user turn); got %#v", len(active), active)
	}
	fullLedger := buildAgentLedger(msgs)
	activeLedger := buildAgentLedger(active)
	if len(fullLedger.Completed) == 0 {
		t.Fatal("precondition failed: full-history ledger must contain the turn-1 completed call")
	}
	if len(activeLedger.Completed) != 0 {
		t.Fatalf("active ledger must be empty for a fresh turn, got %d completed calls", len(activeLedger.Completed))
	}
}

// TestCrossTurnToolAvailabilityDenialStillCorrected reproduces the exact failure
// from the 图片重排序 session: turn 1 calls tools successfully, then turn 2 (a
// new user message) the model denies tool availability. With the active-turn
// ledger the misjudgment gate must stay armed and the correction must fire;
// with the full-history ledger the gate was wrongly closed (the pre-fix bug).
func TestCrossTurnToolAvailabilityDenialStillCorrected(t *testing.T) {
	msgs := []oaiMsg{
		{Role: "system", Content: "You are an AI agent powered by DeepSeek Harness."},
		{Role: "user", Content: "写一个批处理，将图片重新排序，支持网络路径，并注册为文件夹右键菜单"},
		{Role: "assistant", ToolCalls: []map[string]any{
			{"id": "c1", "type": "function", "function": map[string]any{"name": "pwsh", "arguments": `{"command":"Get-Location"}`}},
			{"id": "c2", "type": "function", "function": map[string]any{"name": "write", "arguments": `{"file_path":"D:\\NET\\ai\\图片重排序\\图片一键重排序.bat","content":"@echo off"}`}},
		}},
		{Role: "tool", ToolCallID: "c1", Content: "D:\\NET\\ai\\图片重排序"},
		{Role: "tool", ToolCallID: "c2", Content: "Created file"},
		{Role: "assistant", Content: "文件已创建，但批处理解析仍失败，暂不导入注册表。"},
		{Role: "user", Content: "重新验证以下项目：批处理编码和命令解析问题、JPG/JPEG 两阶段重命名、中文空格及 UNC 网络路径兼容性"},
	}
	tools := []map[string]any{
		{"type": "function", "function": map[string]any{"name": "pwsh", "description": "PowerShell Core shell"}},
		{"type": "function", "function": map[string]any{"name": "read", "description": "read files"}},
		{"type": "function", "function": map[string]any{"name": "write", "description": "write files"}},
		{"type": "function", "function": map[string]any{"name": "edit", "description": "edit files"}},
	}
	denial := "无法继续完成本轮验证，因为当前会话实际未提供 `pwsh`、`read`、`write` 或 `edit` 工具，无法访问 `D:\\NET\\ai\\图片重排序`。"
	userPrompt := "重新验证以下项目：批处理编码和命令解析问题、JPG/JPEG 两阶段重命名、中文空格及 UNC 网络路径兼容性"

	fullLedger := buildAgentLedger(msgs)
	activeLedger := buildAgentLedger(activeMessages(msgs))
	if len(fullLedger.Completed) == 0 {
		t.Fatal("precondition failed: turn-1 tool calls must appear in the full-history ledger")
	}
	if len(activeLedger.Completed) != 0 {
		t.Fatalf("precondition failed: fresh turn must have an empty active ledger, got %d completed", len(activeLedger.Completed))
	}

	// Pre-fix behavior: the full-history ledger closes the gate (the bug that
	// let the turn-2 denial through un-corrected).
	if workspaceToolMisjudgmentPossible(tools, userPrompt, fullLedger) {
		t.Fatal("full-history ledger closes the misjudgment gate (documents the pre-fix bug); only the active ledger must be consulted")
	}

	// Post-fix behavior: the active-turn ledger keeps the gate armed, so the
	// cross-turn denial is detected and corrected.
	if !workspaceToolMisjudgmentPossible(tools, userPrompt, activeLedger) {
		t.Fatal("active-turn ledger must keep the misjudgment gate armed for a fresh turn despite successful calls in earlier turns")
	}
	if !needsWorkspaceToolMisjudgmentCorrection(denial, tools, userPrompt, activeLedger) {
		t.Fatal("cross-turn tool-availability denial must be corrected when evaluated against the active-turn ledger")
	}
}

// TestSameTurnSuccessStillClosesGate ensures the same-turn protection is
// intact: once the model has successfully run a tool in THIS turn, a later
// availability denial is not corrected (the tools are proven to exist).
func TestSameTurnSuccessStillClosesGate(t *testing.T) {
	tools := []map[string]any{
		{"type": "function", "function": map[string]any{"name": "pwsh", "description": "PowerShell Core shell"}},
	}
	ledger := agentLedger{Completed: []toolEvidence{
		{ID: "c1", Name: "pwsh", Arguments: `{"cmd":"dir"}`},
	}}
	if workspaceToolMisjudgmentPossible(tools, "修复项目", ledger) {
		t.Fatal("same-turn completed call must close the gate (tools proven this turn)")
	}
	if needsWorkspaceToolMisjudgmentCorrection("当前会话没有任何可用工具", tools, "修复项目", ledger) {
		t.Fatal("same-turn completed call must suppress correction")
	}
}
