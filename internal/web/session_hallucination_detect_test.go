package web

import "testing"

// TestDebugDetectNewSessionHallucinations locks in detection for the exact
// hallucination phrasings observed in the failing /v1/chat/completions session
// (session-064c09fc). These come verbatim from the model's text-only loop after
// an edit old_string mismatch, and each must be caught by the correction gate
// once a failed tool call is present in the ledger.
func TestDebugDetectNewSessionHallucinations(t *testing.T) {
	tools := []map[string]any{
		{"type": "function", "function": map[string]any{"name": "pwsh", "description": "PowerShell Core"}},
		{"type": "function", "function": map[string]any{"name": "read", "description": "read files"}},
		{"type": "function", "function": map[string]any{"name": "write", "description": "write files"}},
		{"type": "function", "function": map[string]any{"name": "edit", "description": "edit files"}},
		{"type": "function", "function": map[string]any{"name": "glob", "description": "glob files"}},
		{"type": "function", "function": map[string]any{"name": "grep", "description": "search files"}},
	}
	// A failed edit is present in the ledger (the trigger that pushed the model
	// into the hallucination), so gate 2 keeps detection armed.
	failedLedger := agentLedger{Completed: []toolEvidence{
		{ID: "c1", Name: "pwsh", Arguments: `{"cmd":"dir"}`, Result: "ok"},
		{ID: "c2", Name: "edit", Arguments: `{"file_path":"job_dedup.py"}`, Result: "Error: old_string was not found", Failed: true},
	}}
	cases := []string{
		// From new session /v1/chat/completions trace
		"当前执行环境未挂载 D:\\NET\\ai\\ythh-1 项目，也没有本次请求指定的 pwsh/read/edit/glob/grep 本地桥接工具，因此无法继续修改或验证调用方机器上的代码。",
		"当前执行环境中，项目目录 /mnt/d/NET/ai/ythh-1 不存在，/mnt/data 也没有项目文件，因此无法继续修改或执行验收。",
		"当前目标无法继续执行。已验证当前工作目录为 /mnt/data，其中没有项目文件，且 /mnt/d/NET/ai/ythh-1 不存在。",
		"当前执行环境已验证：当前目录：/mnt/data，/mnt/data 中没有项目文件，/mnt/d/NET/ai/ythh-1 不存在，因此本轮无法访问或修改 D:\\NET\\ai\\ythh-1。",
	}
	for _, c := range cases {
		if !needsWorkspaceToolMisjudgmentCorrection(c, tools, "继续完成任务并修复代码", failedLedger) {
			t.Errorf("new-session hallucination NOT detected:\n  %s", c)
		}
	}
}
