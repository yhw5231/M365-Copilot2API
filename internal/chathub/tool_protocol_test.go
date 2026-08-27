package chathub

import (
	"encoding/json"
	"testing"
)

func TestToolNames_FromFunctionWrapper(t *testing.T) {
	tools := []Tool{
		{Type: "function", Function: json.RawMessage(`{"name":"bash","description":"Bash shell","parameters":{"type":"object"}}`)},
		{Type: "function", Function: json.RawMessage(`{"name":"read","description":"Read file","parameters":{"type":"object"}}`)},
	}
	names := toolNames(tools)
	if !names["bash"] {
		t.Fatal("expected bash in tool names")
	}
	if !names["read"] {
		t.Fatal("expected read in tool names")
	}
	if len(names) != 2 {
		t.Fatalf("expected 2 names, got %d", len(names))
	}
}

func TestDeclaredToolName(t *testing.T) {
	names := map[string]bool{"bash": true, "read": true}
	if got := declaredToolName(names, knownShellNames); got != "bash" {
		t.Fatalf("expected bash, got %q", got)
	}
	names = map[string]bool{"pwsh": true}
	if got := declaredToolName(names, knownShellNames); got != "pwsh" {
		t.Fatalf("expected pwsh, got %q", got)
	}
	names = map[string]bool{"workspace_shell": true, "powershell": true}
	if got := declaredToolName(names, knownShellNames); got != "workspace_shell" {
		t.Fatalf("expected workspace_shell (highest priority), got %q", got)
	}
	names = map[string]bool{"exec": true}
	if got := declaredToolName(names, knownShellNames); got != "" {
		t.Fatalf("expected empty for exec (not a known shell), got %q", got)
	}
}

func TestBuildExecutionInstruction_NoTools(t *testing.T) {
	got := buildExecutionInstruction(nil)
	if got != "" {
		t.Fatalf("expected empty for no tools, got: %s", got)
	}
}

func TestBuildExecutionInstruction_Bash(t *testing.T) {
	tools := []Tool{
		{Type: "function", Function: json.RawMessage(`{"name":"bash","description":"Bash","parameters":{"type":"object"}}`)},
	}
	got := buildExecutionInstruction(tools)
	if got == "" {
		t.Fatal("expected non-empty instruction")
	}
	if !containsString(got, "bash") {
		t.Fatal("expected instruction to mention bash")
	}
	if !containsString(got, "Windows paths like D:\\") {
		t.Fatal("expected instruction to mention Windows paths")
	}
}

func TestBuildExecutionInstruction_Pwsh(t *testing.T) {
	tools := []Tool{
		{Type: "function", Function: json.RawMessage(`{"name":"pwsh","description":"PowerShell Core","parameters":{"type":"object"}}`)},
	}
	got := buildExecutionInstruction(tools)
	if got == "" {
		t.Fatal("expected non-empty instruction")
	}
	if !containsString(got, "pwsh") {
		t.Fatal("expected instruction to mention pwsh, got:", got)
	}
}

func TestBuildExecutionInstruction_WorkspaceShell(t *testing.T) {
	tools := []Tool{
		{Type: "function", Function: json.RawMessage(`{"name":"workspace_shell","description":"Workspace shell","parameters":{"type":"object"}}`)},
	}
	got := buildExecutionInstruction(tools)
	if got == "" {
		t.Fatal("expected non-empty instruction")
	}
	if !containsString(got, "workspace_shell") {
		t.Fatal("expected instruction to mention workspace_shell, got:", got)
	}
}

func TestBuildExecutionInstruction_Exec(t *testing.T) {
	tools := []Tool{
		{Type: "custom", Function: json.RawMessage(`{"name":"exec","description":"Custom exec","parameters":{"type":"object"}}`)},
	}
	got := buildExecutionInstruction(tools)
	if got == "" {
		t.Fatal("expected non-empty instruction")
	}
	if !containsString(got, "custom exec tool") {
		t.Fatal("expected instruction to mention custom exec tool, got:", got)
	}
}

func TestBuildExecutionInstruction_FileToolsOnly(t *testing.T) {
	tools := []Tool{
		{Type: "function", Function: json.RawMessage(`{"name":"read","description":"Read","parameters":{"type":"object"}}`)},
		{Type: "function", Function: json.RawMessage(`{"name":"write","description":"Write","parameters":{"type":"object"}}`)},
	}
	got := buildExecutionInstruction(tools)
	if got == "" {
		t.Fatal("expected non-empty instruction for file tools")
	}
	if !containsString(got, "never infer") {
		t.Fatal("expected instruction to forbid path inference, got:", got)
	}
}

func TestBuildExecutionInstruction_BashPlusRead(t *testing.T) {
	tools := []Tool{
		{Type: "function", Function: json.RawMessage(`{"name":"bash","description":"Bash","parameters":{"type":"object"}}`)},
		{Type: "function", Function: json.RawMessage(`{"name":"read","description":"Read","parameters":{"type":"object"}}`)},
	}
	got := buildExecutionInstruction(tools)
	if !containsString(got, "bash") || !containsString(got, "never infer") {
		t.Fatal("expected instruction to include both bash and file tool guidance, got:", got)
	}
}

func containsString(s, substr string) bool {
	if len(s) == 0 || len(substr) == 0 {
		return false
	}
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// mkCompatTools builds a Tool slice from plain tool names for prompt tests.
func mkCompatTools(names ...string) []Tool {
	var out []Tool
	for _, n := range names {
		f, _ := json.Marshal(map[string]any{"name": n, "description": n, "parameters": map[string]any{"type": "object"}})
		out = append(out, Tool{Type: "function", Function: f})
	}
	return out
}

// TestCodexPureSetKeepsToolsInFencedPrompt: a pure Codex declaration (only
// exec_command / write_stdin / view_image) registers zero M365 plugins and
// keeps all three tools visible through the fenced-call protocol.
func TestCodexPureSetKeepsToolsInFencedPrompt(t *testing.T) {
	tools := mkCompatTools("exec_command", "write_stdin", "view_image")
	plugins := clientPlugins(tools, "")
	if len(plugins) != 0 {
		t.Fatalf("pure Codex set must register zero plugins, got %d", len(plugins))
	}
	hasPlugins := len(plugins) > 0
	p := toolProtocolPrompt("do it", tools, "auto", hasPlugins)
	if !containsString(p, "exec_command") || !containsString(p, "write_stdin") || !containsString(p, "view_image") {
		t.Fatalf("fenced protocol must keep all three Codex tools, got:\n%s", p)
	}
}

// TestMixedSetKeepsExecCommandViaFencedProtocol: a mixed declaration
// (read + exec_command) registers read as a native plugin but must still
// announce exec_command through the fenced protocol so the model can call it
// locally. Regression guard for the hasPlugins branch.
func TestMixedSetKeepsExecCommandViaFencedProtocol(t *testing.T) {
	tools := mkCompatTools("read", "exec_command")
	plugins := clientPlugins(tools, "")
	names := []string{}
	for _, p := range plugins {
		if m, ok := p.(map[string]any); ok {
			names = append(names, m["Id"].(string))
		}
	}
	if len(names) != 1 || names[0] != "read" {
		t.Fatalf("mixed set must register only read plugin, got %v", names)
	}
	hasPlugins := len(plugins) > 0
	if !hasPlugins {
		t.Fatal("mixed set must have plugins (read is a normal tool)")
	}
	p := toolProtocolPrompt("do it", tools, "auto", hasPlugins)
	if !containsString(p, "exec_command") {
		t.Fatalf("mixed set must keep exec_command visible via fenced protocol, got:\n%s", p)
	}
	if containsString(p, "```read") {
		t.Fatalf("read is a native plugin and must not be duplicated in the fenced protocol, got:\n%s", p)
	}
}

// TestOrdinaryToolsNoFencedProtocol: ordinary tool sets (read/write/edit) keep
// the plain prompt; no fenced block protocol is needed when every declared
// tool is a native M365 plugin.
func TestOrdinaryToolsNoFencedProtocol(t *testing.T) {
	tools := mkCompatTools("read", "write", "edit")
	plugins := clientPlugins(tools, "")
	if len(plugins) != 3 {
		t.Fatalf("ordinary set must register all three plugins, got %d", len(plugins))
	}
	p := toolProtocolPrompt("do it", tools, "auto", len(plugins) > 0)
	if containsString(p, "<tools>") {
		t.Fatalf("ordinary set must not use the fenced protocol, got:\n%s", p)
	}
}
