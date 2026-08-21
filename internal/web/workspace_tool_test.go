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