package web

import "testing"

// TestNeedsWorkspaceToolMisjudgmentCorrection verifies the protocol-first gate:
// corrections only fire when the caller declared a tool, no tool call completed
// this turn, and the user's own prompt did not already introduce the workspace
// vocabulary.
func TestNeedsWorkspaceToolMisjudgmentCorrection(t *testing.T) {
	execTools := []map[string]any{
		{"type": "function", "function": map[string]any{"name": "pwsh", "description": "shell"}},
		{"type": "function", "function": map[string]any{"name": "read", "description": "read"}},
	}
	// User asked about the very vocabulary the detector keys on; the model's
	// explanation is a legitimate answer, not a misjudgment.
	userPrompt := "为什么我的 Linux 容器 /mnt/data 无法访问工作区？"
	text := "当前会话实际没有可调用的 pwsh、read、write、edit、glob 或 grep 文件操作接口"
	if needsWorkspaceToolMisjudgmentCorrection(text, execTools, userPrompt, agentLedger{}) {
		t.Fatal("user-echoed workspace vocabulary must not trigger correction")
	}

	// Same text with a neutral user prompt must still be caught.
	if !needsWorkspaceToolMisjudgmentCorrection(text, execTools, "帮我修复这个项目", agentLedger{}) {
		t.Fatal("genuine misjudgment with neutral prompt not detected")
	}

	// Protocol gate 1: caller declared no tool at all -> no correction.
	if needsWorkspaceToolMisjudgmentCorrection("当前会话没有任何可用工具", nil, "修复项目", agentLedger{}) {
		t.Fatal("no declared tools must suppress correction")
	}

	// Protocol gate 2: a completed tool call proves tools exist -> no correction.
	ledger := agentLedger{Completed: []toolEvidence{{ID: "c1", Name: "pwsh", Arguments: `{"cmd":"dir"}`}}}
	if needsWorkspaceToolMisjudgmentCorrection("当前会话没有任何可用工具", execTools, "修复项目", ledger) {
		t.Fatal("completed tool call must suppress correction")
	}

	// Gate 2 exception: a FAILED completed tool call is the strongest trigger for
	// sandbox hallucination (the model retroactively denies even successful calls
	// after an edit mismatch). Detection must stay armed.
	failedLedger := agentLedger{Completed: []toolEvidence{
		{ID: "ok", Name: "pwsh", Arguments: `{"cmd":"dir"}`},
		{ID: "bad", Name: "edit", Arguments: `{"file_path":"x.py"}`, Result: "Error: old_string was not found", Failed: true},
	}}
	if !needsWorkspaceToolMisjudgmentCorrection("当前会话没有任何可用工具", execTools, "修复项目", failedLedger) {
		t.Fatal("failed completed tool call must keep correction armed")
	}
	if !needsWorkspaceToolMisjudgmentCorrection("当前工作目录为 /mnt/data，其中没有项目文件", execTools, "修复项目", failedLedger) {
		t.Fatal("mnt/data sole-environment claim must be detected after a failed call")
	}
	if !needsWorkspaceToolMisjudgmentCorrection("/mnt/d/NET/ai/ythh-1 不存在，本轮无法继续", execTools, "修复项目", failedLedger) {
		t.Fatal("missing /mnt/d/ caller path claim must be detected after a failed call")
	}

	// Echo suppression must still win even with a failed call in the ledger:
	// the user asked about /mnt/data, so the model explaining it is not a
	// misjudgment.
	if needsWorkspaceToolMisjudgmentCorrection("当前工作目录为 /mnt/data，其中没有项目文件", execTools, "为什么我的工作目录是 /mnt/data？", failedLedger) {
		t.Fatal("user-echoed workspace vocabulary must suppress correction even after a failed call")
	}

	// Empty text never fires.
	if needsWorkspaceToolMisjudgmentCorrection("   ", execTools, "修复项目", agentLedger{}) {
		t.Fatal("empty text must not fire")
	}
}

// TestUserPromptMentionsWorkspace verifies echo-term detection covers the exact
// vocabulary users commonly ask about.
func TestUserPromptMentionsWorkspace(t *testing.T) {
	cases := []struct {
		prompt string
		want   bool
	}{
		{"为什么我的 Linux 容器 /mnt/data 无法访问工作区？", true},
		{"请修复这个项目的构建错误", false},
		{"当前会话只提供 linux 容器，无法运行命令", true},
		{"部署到云端 sandbox 环境", true},
		{"帮我检查 Windows 工作区里文件的权限", false}, // 仅提"工作区"不算回声
	}
	for _, c := range cases {
		if got := userPromptMentionsWorkspace(c.prompt); got != c.want {
			t.Errorf("userPromptMentionsWorkspace(%q) = %v, want %v", c.prompt, got, c.want)
		}
	}
}
