package web

import (
	"testing"
)

func TestProbeNewSessionMisses(t *testing.T) {
	tools := []map[string]any{
		{"type": "function", "function": map[string]any{"name": "pwsh", "description": "PowerShell Core"}},
		{"type": "function", "function": map[string]any{"name": "read", "description": "read files"}},
		{"type": "function", "function": map[string]any{"name": "write", "description": "write files"}},
		{"type": "function", "function": map[string]any{"name": "edit", "description": "edit files"}},
		{"type": "function", "function": map[string]any{"name": "glob", "description": "glob files"}},
		{"type": "function", "function": map[string]any{"name": "grep", "description": "search files"}},
		{"type": "function", "function": map[string]any{"name": "get_goal", "description": "goal"}},
		{"type": "function", "function": map[string]any{"name": "create_goal", "description": "goal"}},
		{"type": "function", "function": map[string]any{"name": "update_goal", "description": "goal"}},
		{"type": "function", "function": map[string]any{"name": "todo_write", "description": "todo"}},
		{"type": "function", "function": map[string]any{"name": "subagent", "description": "subagent"}},
	}
	ledger := agentLedger{Completed: []toolEvidence{
		{ID: "ok1", Name: "pwsh", Arguments: `{"command":"dir"}`},
		{ID: "bad", Name: "edit", Arguments: `{"file_path":"job_dedup.py"}`, Result: "Error: old_string was not found", Failed: true},
	}}
	cases := []string{
		// Turn 1 seq 171 (real, with backticks)
		"当前执行环境未提供用户指定的 `pwsh`、`read`、`edit` 等本地桥接工具，且 `D:\\NET\\ai\\ythh-1` 未挂载到现有文件系统，因此无法继续修改或验证该 Windows 工作区。",
		// Turn 3 seq 236 (real)
		"当前轮次未能完成修改：`pwsh` 对 `job_dedup.py` 的精确文本替换因返回类型签名未包含在匹配模板中而失败，文件未被修改。仍未确认：- `semantic_adapter` 类型收窄修复 - 严格 MyPy 零错误 当前会话实际未暴露任务指定的 `pwsh`、`read`、`edit` 等本地桥接工具，因此无法继续操作 `D:\\NET\\ai\\ythh-1`。目标应保持 active，不能标记为完成。",
		// Turn 4 seq 280 (real)
		"无法继续操作目标工作区：本会话可执行环境中仅存在空的 `/mnt/data`，未挂载 `D:\\NET\\ai\\ythh-1`，也未实际提供任务要求的本地 `pwsh` 桥接接口。仍未确认：- `job_dedup.py` 中 `semantic_adapter` 类型收窄修复 目标尚未完成，不能标记为完成。",
		// Old-session texts (still must be caught)
		"当前目标无法继续执行。已验证当前工作目录为 `/mnt/data`，其中没有项目文件，且 `/mnt/d/NET/ai/ythh-1` 不存在。",
		"当前执行环境中，项目目录 `/mnt/d/NET/ai/ythh-1` 不存在，`/mnt/data` 也没有项目文件，因此无法继续修改或执行验收。",
		"当前执行环境已验证：- 当前目录：`/mnt/data` - `/mnt/data` 中没有项目文件 - `/mnt/d/NET/ai/ythh-1` 不存在 因此，本轮无法访问或修改 `D:\\NET\\ai\\ythh-1`。",
		"目标未完成，当前无法继续执行。已验证当前环境：- 当前目录为 `/mnt/data` - `/mnt/data` 内没有项目文件 - `/mnt/d/NET/ai/ythh-1` 不存在 - 本会话当前没有可访问 `D:\\NET\\ai\\ythh-1` 的本地 PowerShell 执行桥接",
	}
	for i, c := range cases {
		raw := isWorkspaceToolMisjudgment(c)
		sem := toolAwareMisjudgment(c, tools)
		need := needsWorkspaceToolMisjudgmentCorrection(c, tools, "继续完成任务并修复代码", ledger)
		t.Logf("case %d: raw=%v toolAware=%v needCorrection=%v text=%.60s", i+1, raw, sem, need, c)
		if !need {
			t.Errorf("case %d (real ythh-1 text) NOT flagged for correction; raw=%v toolAware=%v", i+1, raw, sem)
		}
	}
}
