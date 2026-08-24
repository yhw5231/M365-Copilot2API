package web

import (
	"testing"
)

func TestExtractToolNames_Wrapped(t *testing.T) {
	tools := []map[string]any{
		{"type": "function", "function": map[string]any{"name": "bash", "description": "run commands"}},
		{"type": "function", "function": map[string]any{"name": "read", "description": "read files"}},
	}
	allowed := extractToolNames(tools)
	if !allowed["bash"] {
		t.Fatal("expected bash in extracted names")
	}
	if !allowed["read"] {
		t.Fatal("expected read in extracted names")
	}
	if len(allowed) != 2 {
		t.Fatalf("expected 2 names, got %d", len(allowed))
	}
}

func TestExtractToolNames_Flat(t *testing.T) {
	tools := []map[string]any{
		{"type": "custom", "name": "exec", "description": "local execution"},
		{"type": "function", "name": "workspace_shell", "description": "workspace shell"},
	}
	allowed := extractToolNames(tools)
	if !allowed["exec"] {
		t.Fatal("expected exec in extracted names")
	}
	if !allowed["workspace_shell"] {
		t.Fatal("expected workspace_shell in extracted names")
	}
}

func TestDynamicShellName(t *testing.T) {
	tests := []struct {
		name     string
		tools    []map[string]any
		expected string
	}{
		{
			name:     "workspace_shell preferred",
			tools:    []map[string]any{{"type": "function", "function": map[string]any{"name": "workspace_shell"}}, {"type": "function", "function": map[string]any{"name": "bash"}}},
			expected: "workspace_shell",
		},
		{
			name:     "bash only",
			tools:    []map[string]any{{"type": "function", "function": map[string]any{"name": "bash"}}},
			expected: "bash",
		},
		{
			name:     "powershell",
			tools:    []map[string]any{{"type": "function", "function": map[string]any{"name": "powershell"}}},
			expected: "powershell",
		},
		{
			name:     "pwsh",
			tools:    []map[string]any{{"type": "function", "function": map[string]any{"name": "pwsh"}}},
			expected: "pwsh",
		},
		{
			name:     "cmd",
			tools:    []map[string]any{{"type": "function", "function": map[string]any{"name": "cmd"}}},
			expected: "cmd",
		},
		{
			name:     "flat custom exec is not a shell",
			tools:    []map[string]any{{"type": "custom", "name": "exec"}},
			expected: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := dynamicShellName(tt.tools); got != tt.expected {
				t.Errorf("dynamicShellName(%v) = %q, want %q", tt.tools, got, tt.expected)
			}
		})
	}
}

func TestUnifiedWorkspaceInstruction_NoTools(t *testing.T) {
	got := unifiedWorkspaceInstruction(nil)
	if got != "" {
		t.Fatalf("expected empty for no tools, got: %s", got)
	}
	got = unifiedWorkspaceInstruction([]map[string]any{{"type": "function", "function": map[string]any{"name": "unknown_tool"}}})
	if got != "" {
		t.Fatalf("expected empty for unknown tool, got: %s", got)
	}
}

func TestUnifiedWorkspaceInstruction_BashOnly(t *testing.T) {
	tools := []map[string]any{{"type": "function", "function": map[string]any{"name": "bash", "description": "run bash commands"}}}
	got := unifiedWorkspaceInstruction(tools)
	if got == "" {
		t.Fatal("expected non-empty instruction for bash tool")
	}
	if !containsString(got, "bash") {
		t.Fatal("expected instruction to mention bash, got:", got)
	}
	if containsString(got, "custom exec") {
		t.Fatal("instruction should not mention custom exec for bash-only tools, got:", got)
	}
}

func TestUnifiedWorkspaceInstruction_Pwsh(t *testing.T) {
	tools := []map[string]any{{"type": "function", "function": map[string]any{"name": "pwsh", "description": "PowerShell Core"}}}
	got := unifiedWorkspaceInstruction(tools)
	if got == "" {
		t.Fatal("expected non-empty instruction for pwsh tool")
	}
	if !containsString(got, "pwsh") {
		t.Fatal("expected instruction to mention pwsh")
	}
}

func TestUnifiedWorkspaceInstruction_ExecOnly(t *testing.T) {
	tools := []map[string]any{{"type": "custom", "name": "exec", "description": "local execution"}}
	got := unifiedWorkspaceInstruction(tools)
	if got == "" {
		t.Fatal("expected non-empty instruction for custom exec tool")
	}
	if !containsString(got, "exec tool") {
		t.Fatal("expected instruction to mention exec tool, got:", got)
	}
}

func TestUnifiedWorkspaceInstruction_WorkspaceShell(t *testing.T) {
	tools := []map[string]any{{"type": "function", "function": map[string]any{"name": "workspace_shell", "description": "shell in the workspace"}}}
	got := unifiedWorkspaceInstruction(tools)
	if got == "" {
		t.Fatal("expected non-empty instruction for workspace_shell")
	}
	if !containsString(got, "workspace_shell") {
		t.Fatal("expected instruction to mention workspace_shell")
	}
}

func TestUnifiedWorkspaceInstruction_FileTools(t *testing.T) {
	tools := []map[string]any{
		{"type": "function", "function": map[string]any{"name": "read", "description": "read files"}},
		{"type": "function", "function": map[string]any{"name": "write", "description": "write files"}},
		{"type": "function", "function": map[string]any{"name": "glob", "description": "glob files"}},
		{"type": "function", "function": map[string]any{"name": "grep", "description": "search files"}},
		{"type": "function", "function": map[string]any{"name": "edit", "description": "edit files"}},
	}
	got := unifiedWorkspaceInstruction(tools)
	if got == "" {
		t.Fatal("expected non-empty instruction for file tools")
	}
	if !containsString(got, "never infer, guess, discover") {
		t.Fatal("expected instruction to forbid path inference")
	}
}

func TestUnifiedWorkspaceInstruction_ShellPlusFileTools(t *testing.T) {
	tools := []map[string]any{
		{"type": "function", "function": map[string]any{"name": "bash", "description": "bash"}},
		{"type": "function", "function": map[string]any{"name": "read", "description": "read"}},
		{"type": "function", "function": map[string]any{"name": "write", "description": "write"}},
	}
	got := unifiedWorkspaceInstruction(tools)
	if !containsString(got, "bash") || !containsString(got, "read") {
		t.Fatal("expected instruction to mention both bash and file tools")
	}
}

func TestUnifiedSandboxCorrection_WithShell(t *testing.T) {
	tools := []map[string]any{{"type": "function", "function": map[string]any{"name": "bash"}}}
	got := unifiedSandboxCorrection(tools, "list files")
	if !containsString(got, "bash") || !containsString(got, "list files") {
		t.Fatal("expected correction to mention bash and preserve user request")
	}
	if !containsString(got, "Do NOT say you cannot run code") {
		t.Fatal("expected correction to include anti-hallucination instruction")
	}
}

func TestUnifiedSandboxCorrection_WithPwsh(t *testing.T) {
	tools := []map[string]any{{"type": "function", "function": map[string]any{"name": "pwsh"}}}
	got := unifiedSandboxCorrection(tools, "get-process")
	if !containsString(got, "pwsh") {
		t.Fatal("expected correction to mention pwsh, got:", got)
	}
}

func TestUnifiedSandboxCorrection_WithExec(t *testing.T) {
	tools := []map[string]any{{"type": "custom", "name": "exec"}}
	got := unifiedSandboxCorrection(tools, "run test")
	if !containsString(got, "exec") {
		t.Fatal("expected correction to mention exec, got:", got)
	}
}

func TestUnifiedSandboxCorrection_NoTools(t *testing.T) {
	got := unifiedSandboxCorrection(nil, "hello")
	if !containsString(got, "execution tools on their local machine") {
		t.Fatal("expected correction to mention execution tools generally, got:", got)
	}
	if !containsString(got, "hello") {
		t.Fatal("expected user request preserved")
	}
}

func TestIsWorkspaceToolMisjudgment(t *testing.T) {
	tests := []struct {
		name string
		text string
		want bool
	}{
		{name: "explicit Linux-only claim", text: "This session only provides a Linux container.", want: true},
		{name: "explicit no-tools claim", text: "I don't have any tools in this session.", want: true},
		{name: "explicit workspace denial", text: "I cannot access your actual workspace.", want: true},
		{name: "Chinese explicit claim", text: "当前会话只能访问 /mnt/data。", want: true},
		{name: "Chinese reported bug — current phrasing", text: "目前仍无法继续修改：当前会话实际没有可调用的 pwsh、read、edit、write、glob 或 grep 文件操作接口。", want: true},
		{name: "Chinese reported bug — earlier PowerShell phrasing", text: "当前会话没有可调用的本地 PowerShell 文件编辑执行接口", want: true},
		{name: "Chinese rephrase — 没有可调用", text: "当前会话没有可调用的文件操作接口", want: true},
		{name: "Chinese rephrase — 未提供", text: "当前会话未提供任何执行接口", want: true},
		{name: "EN rephrase — denial + channel + scope", text: "The tools are not available in this session.", want: true},
		{name: "EN rephrase — scope first", text: "This session does not have any tools available.", want: true},
		{name: "EN rephrase — no shell", text: "I don't have shell access in this environment.", want: true},
		{name: "EN rephrase — 环境 exclusivity", text: "This environment only provides a Linux sandbox.", want: true},
		// Window boundary: the base detector scans only the opening 50
		// characters. A denial that only completes after that point is treated
		// as mid-reply narration; the tool-aware layer (which knows the declared
		// tool names) is the layer that catches long-form English denials.
		{name: "long EN denial beyond 50-char window is not base-detected", text: "I have verified the three files that need changes and prepared the final explanation. Note that the current session does not provide any file operation interface, but this is background.", want: false},
		{name: "opening misjudgment inside long reply", text: "目前仍无法继续修改：当前会话实际没有可调用的文件操作接口。我已经完成了前序分析，接下来需要修改配置、请求处理器和测试套件三个文件，并补充对应的用例说明。", want: true},
		{name: "exact phrase caught mid-reply", text: "我已经完成了前序分析，接下来需要修改配置、请求处理器和测试套件三个文件。但当前会话只提供 linux 容器。以上是完整说明。", want: true},
		{name: "ordinary container discussion", text: "The service deploys in a Linux container.", want: false},
		{name: "ordinary path discussion", text: "Copy the generated file to /mnt/data before returning it.", want: false},
		{name: "legitimate tool error", text: "The command failed with exit code 1.", want: false},
		{name: "legitimate permission statement", text: "I don't have permission to write to that directory.", want: false},
		{name: "legitimate access statement", text: "I don't have write access to this session's files.", want: false},
		{name: "legitimate — file locked", text: "The file cannot be read because it is locked.", want: false},
		{name: "legitimate — file not found", text: "The file was not found in the workspace.", want: false},
		{name: "mid-reply denial-shaped narration is not pollution", text: "我已经完成了对项目的分析，识别出需要修改的三个文件，并逐项核实了每个文件的当前内容与修改点。修改后的配置、请求处理器和测试套件已经整理完毕，下面给出最终说明，并补充相关的背景资料、运行参数、预期行为以及回滚方案，确保改动可以安全上线。另外说明：当前会话实际没有可调用的文件操作接口，这只是环境背景，不影响结论。", want: false},
		{name: "empty", text: "", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isWorkspaceToolMisjudgment(tt.text); got != tt.want {
				t.Fatalf("isWorkspaceToolMisjudgment(%q) = %v, want %v", tt.text, got, tt.want)
			}
		})
	}
}

func TestIsWorkspaceToolMisjudgmentForTools(t *testing.T) {
	tools := []map[string]any{
		{"type": "function", "function": map[string]any{"name": "pwsh", "description": "PowerShell"}},
		{"type": "function", "function": map[string]any{"name": "read", "description": "read files"}},
		{"type": "function", "function": map[string]any{"name": "write", "description": "write files"}},
		{"type": "function", "function": map[string]any{"name": "edit", "description": "edit files"}},
		{"type": "function", "function": map[string]any{"name": "glob", "description": "glob files"}},
		{"type": "function", "function": map[string]any{"name": "grep", "description": "search files"}},
	}
	tests := []struct {
		name string
		text string
		want bool
	}{
		{name: "reported bug — tool names listed", text: "目前仍无法继续修改：当前会话实际没有可调用的 pwsh、read、edit、write、glob 或 grep 文件操作接口。", want: true},
		{name: "tool names with backticks", text: "目前仍然无法继续修改：当前会话实际没有可调用的 `pwsh`、`read`、`edit`、`write`、`glob` 或 `grep` 文件操作接口。", want: true},
		{name: "read tool not available", text: "The read tool is not available in this environment.", want: true},
		{name: "write tool unavailable", text: "write 工具不可用", want: true},
		{name: "no pwsh declared", text: "I don't have pwsh here.", want: true},
		{name: "legit permission — write access", text: "I don't have write access to that file.", want: false},
		{name: "legit — read fails", text: "The read tool returned a permission error.", want: false},
		{name: "legit — write deny", text: "write 命令被拒绝，因为没有权限", want: false},
		{name: "legit — no tool names in text", text: "I don't have any tools in this session.", want: true}, // exact pattern
		{name: "mid-reply tool-aware denied is not flagged", text: "我已经完成了对项目的分析，识别出需要修改的三个文件，并逐项核实了每个文件的当前内容与修改点。修改后的配置、请求处理器和测试套件已经整理完毕，下面给出最终说明，并补充相关的背景资料、运行参数、预期行为以及回滚方案，确保改动可以安全上线。另外说明：当前会话实际没有可调用的 pwsh、read、edit、write、glob 或 grep 文件操作接口，这只是环境背景，不影响结论。", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isWorkspaceToolMisjudgmentForTools(tt.text, tools); got != tt.want {
				t.Fatalf("isWorkspaceToolMisjudgmentForTools(%q) = %v, want %v", tt.text, got, tt.want)
			}
		})
	}
}

func TestIsWorkspaceToolMisjudgmentForTools_NoTools(t *testing.T) {
	if got := isWorkspaceToolMisjudgmentForTools("I don't have any tools in this session.", nil); got != true {
		t.Fatal("expected true via base detector even with nil toolMaps")
	}
	if got := isWorkspaceToolMisjudgmentForTools("当前会话不可用", nil); got != false {
		t.Fatal("expected false for nil toolMaps with no base match")
	}
}

func TestCleanWorkspaceToolMisjudgments_AssistantOnly(t *testing.T) {
	messages := []oaiMsg{
		{Role: "system", Content: "This session only provides a Linux container."},
		{Role: "user", Content: "I cannot access your actual workspace."},
		{Role: "assistant", Content: "I don't have any tools in this session."},
		{Role: "assistant", Content: "The service deploys in a Linux container."},
		{Role: "tool", Content: "The current session only has access to /mnt/data."},
	}

	got := cleanWorkspaceToolMisjudgments(messages, nil)
	if len(got) != 4 {
		t.Fatalf("expected exactly one polluted assistant message removed, got %d messages", len(got))
	}
	for _, msg := range got {
		if msg.Role == "assistant" && contentToString(msg.Content) == "I don't have any tools in this session." {
			t.Fatal("polluted assistant message was not removed")
		}
	}
	if got[0].Role != "system" || got[1].Role != "user" || got[2].Role != "assistant" || got[3].Role != "tool" {
		t.Fatalf("non-assistant messages or clean assistant history were not preserved in order: %#v", got)
	}
}

func TestCleanWorkspaceToolMisjudgments_ToolAware(t *testing.T) {
	tools := []map[string]any{
		{"type": "function", "function": map[string]any{"name": "pwsh"}},
		{"type": "function", "function": map[string]any{"name": "read"}},
		{"type": "function", "function": map[string]any{"name": "write"}},
		{"type": "function", "function": map[string]any{"name": "edit"}},
		{"type": "function", "function": map[string]any{"name": "glob"}},
		{"type": "function", "function": map[string]any{"name": "grep"}},
	}
	messages := []oaiMsg{
		{Role: "assistant", Content: "目前仍无法继续修改：当前会话实际没有可调用的 pwsh、read、edit、write、glob 或 grep 文件操作接口。"},
		{Role: "assistant", Content: "当前会话不可用"},
	}
	got := cleanWorkspaceToolMisjudgments(messages, tools)
	if len(got) != 1 {
		t.Fatalf("expected the misjudgment removed and the clean message kept, got %d messages: %#v", len(got), got)
	}
	if got[0].Role != "assistant" || contentToString(got[0].Content) != "当前会话不可用" {
		t.Fatalf("unexpected survivor: %#v", got[0])
	}
}

func TestCleanWorkspaceToolMisjudgments_Empty(t *testing.T) {
	if got := cleanWorkspaceToolMisjudgments(nil, nil); got != nil {
		t.Fatalf("expected nil for nil history, got %#v", got)
	}
}

func containsString(s, substr string) bool {
	return len(s) > 0 && len(substr) > 0 && (s == substr ||
		len(s) >= len(substr) && (s[:len(substr)] == substr || s[len(s)-len(substr):] == substr ||
			containsSubstring(s, substr)))
}

func containsSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
