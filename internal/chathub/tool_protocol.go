package chathub

import (
	"encoding/json"
	"fmt"
	"strings"
)

// knownShellNames is the precedence-ordered list of shell-like tool names
// that the model can use to execute commands on the caller's machine.
var knownShellNames = []string{"workspace_shell", "bash", "sh", "shell", "powershell", "pwsh", "cmd"}

// knownFileToolNames is the set of file-operation tool names that the model
// may have been given for reading/writing/globbing in the workspace.
var knownFileToolNames = []string{"read", "write", "edit", "glob", "grep"}

// knownExecToolNames are execution tools that can run arbitrary commands
// but aren't traditional shells (OpenCode custom exec bridge, etc).
var knownExecToolNames = []string{"exec"}

// declaredToolName returns the first tool name from candidates that the
// client actually declared, or "" if none.
func declaredToolName(allowed map[string]bool, candidates []string) string {
	for _, n := range candidates {
		if allowed[n] {
			return n
		}
	}
	return ""
}

// toolNames returns a set of declared tool names from the given tools slice.
func toolNames(tools []Tool) map[string]bool {
	out := map[string]bool{}
	for _, t := range tools {
		var f struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(t.Function, &f) != nil || f.Name == "" {
			continue
		}
		out[f.Name] = true
	}
	return out
}

// shellDisplayName returns a human-readable description for a shell tool name.
func shellDisplayName(name string) string {
	switch name {
	case "workspace_shell":
		return "the workspace_shell tool (runs shell commands in the caller's project workspace)"
	case "bash":
		return "the bash tool (runs Bash on the caller's local machine)"
	case "sh":
		return "the sh tool (runs POSIX shell commands)"
	case "shell":
		return "the shell tool (runs operating-system commands on the caller's machine)"
	case "powershell":
		return "the powershell tool (runs Windows PowerShell on the caller's Windows machine)"
	case "pwsh":
		return "the pwsh tool (runs PowerShell Core on the caller's machine)"
	case "cmd":
		return "the cmd tool (runs Windows Command Prompt on the caller's machine)"
	default:
		return fmt.Sprintf("the %s tool (runs on the caller's local machine)", name)
	}
}

// shellRuntimeHint returns a one-sentence hint about how the shell tool
// actually executes.
func shellRuntimeHint(name string) string {
	switch name {
	case "workspace_shell", "bash", "sh", "shell":
		return "runs on the caller's local machine"
	case "powershell", "pwsh":
		return "runs Windows PowerShell on the caller's local machine"
	case "cmd":
		return "runs Windows Command Prompt on the caller's local machine"
	default:
		return "runs locally on the caller's machine"
	}
}

// buildExecutionInstruction builds a dynamic execution-environment instruction
// tailored to the tools the caller actually declared. Returns "" when no
// shell, exec, or file tools are present (caller should fall back to a generic
// instruction).
func buildExecutionInstruction(tools []Tool) string {
	names := toolNames(tools)
	shell := declaredToolName(names, knownShellNames)
	execTool := declaredToolName(names, knownExecToolNames)
	hasFileTools := false
	for _, n := range knownFileToolNames {
		if names[n] {
			hasFileTools = true
			break
		}
	}

	if shell == "" && execTool == "" && !hasFileTools {
		return ""
	}

	var b strings.Builder
	b.WriteString("You are an execution agent on the caller's machine. ")
	b.WriteString("Never use, request, or mention Microsoft 365/Copilot native tools. ")

	if shell != "" {
		b.WriteString(fmt.Sprintf("The caller has provided %s which %s. ", shellDisplayName(shell), shellRuntimeHint(shell)))
		b.WriteString("Windows paths like D:\\ are directly accessible. ")
		b.WriteString("Do NOT use any built-in code interpreter, Python sandbox, or cloud execution environment. ")
		b.WriteString(fmt.Sprintf("If you need to run code or commands, use %s. ", shellDisplayName(shell)))
	} else if execTool != "" {
		b.WriteString("The only permitted execution tool is the caller-provided custom exec tool. ")
		b.WriteString("Do NOT use any built-in code interpreter, Python sandbox, or cloud execution environment. ")
	}

	b.WriteString("Do NOT mention Linux containers, /mnt/data, cloud sandboxes, or claim the execution environment has changed. ")

	if hasFileTools {
		b.WriteString("File-reading and file-writing tools (read, write, edit, glob, grep) are available when declared by the caller. ")
		b.WriteString("Modify only files within the scope explicitly specified by the caller. ")
		b.WriteString("Every file or directory path must be explicitly provided by the caller; never infer, guess, discover, or substitute a path. ")
		b.WriteString("A caller-provided path may be either an absolute project path or a path relative to the caller-selected project workspace. ")
		b.WriteString("Do not assume paths such as /root, /workspace, /tmp, or /mnt/data. ")
	}

	b.WriteString("Never claim a file was created, modified, or verified until ")
	if shell != "" {
		b.WriteString(fmt.Sprintf("%s returns a successful result. ", shellDisplayName(shell)))
	} else if execTool != "" {
		b.WriteString("the exec tool returns a successful result. ")
	} else {
		b.WriteString("the appropriate tool returns a successful result. ")
	}
	b.WriteString("After every execution, verify the result using the same tool.")

	return b.String()
}

// compatibilityOnlyDefinitions renders fenced <tools> definitions for just the
// compatibility-only tools the client declared (exec_command, write_stdin,
// view_image). Returns "" when none were declared. Used by toolProtocolPrompt
// when the M365 native plugin list is non-empty, so a mixed declaration (e.g.
// read + exec_command) still announces the local-execution tools the model
// would otherwise never learn about.
func compatibilityOnlyDefinitions(tools []Tool) string {
	var defs []string
	for _, t := range tools {
		var f struct {
			Name, Description string
			Parameters        json.RawMessage `json:"parameters"`
		}
		if json.Unmarshal(t.Function, &f) != nil || f.Name == "" {
			continue
		}
		if !IsCompatibilityOnlyTool(f.Name) {
			continue
		}
		params := strings.TrimSpace(string(f.Parameters))
		if params == "" || params == "null" {
			params = "{}"
		}
		defs = append(defs, fmt.Sprintf("%s — %s\n```%s\n%s\n```", f.Name, f.Description, f.Name, params))
	}
	return strings.Join(defs, "\n\n")
}

// toolProtocolPrompt follows the community-compatible M365 convention:
// definitions are wrapped in <tools>, and calls are emitted as a fenced block
// whose info string is the exact tool name.
func toolProtocolPrompt(text string, tools []Tool, choice any, hasPlugins bool) string {
	if len(tools) == 0 || strings.EqualFold(fmt.Sprint(choice), "none") {
		if hasPlugins {
			return text
		}
		return fmt.Sprintf("Please answer the following request in full. Do not truncate or abbreviate your response.\n\n%s", text)
	}
	if hasPlugins {
		// M365 native plugins handle the ordinary tools. Compatibility-only
		// tools (exec_command, write_stdin, view_image) are excluded from the
		// plugin list so ChatHub does not bind them to a hosted environment;
		// they must still be announced through the fenced-call protocol or the
		// model will never learn they exist in a mixed declaration.
		compat := compatibilityOnlyDefinitions(tools)
		if compat == "" {
			return text
		}
		execInst := buildExecutionInstruction(tools)
		if execInst == "" {
			execInst = "You are an execution agent on the caller's machine. The tools below are real, active, and callable right now. Do NOT use any built-in code interpreter, Python sandbox, or cloud execution environment. Do NOT mention Linux containers, /mnt/data, cloud sandboxes, or claim the execution environment has changed."
		}
		return fmt.Sprintf("%s\nWhen the user's request requires one of the following local-execution tools, call it by emitting a fenced block. Each block's info string is the exact tool name and its body is a JSON object of arguments. Do not say a tool is unavailable. Wait for the tool result before claiming completion.\n\n<tools>\n%s\n</tools>\n\nUser request:\n%s", execInst, compat, text)
	}
	var defs []string
	for _, t := range tools {
		var f struct {
			Name, Description string
			Parameters        json.RawMessage `json:"parameters"`
		}
		if json.Unmarshal(t.Function, &f) != nil || f.Name == "" {
			continue
		}
		params := strings.TrimSpace(string(f.Parameters))
		if params == "" || params == "null" {
			params = "{}"
		}
		defs = append(defs, fmt.Sprintf("%s — %s\n```%s\n%s\n```", f.Name, f.Description, f.Name, params))
	}
	if len(defs) == 0 {
		return text
	}

	execInst := buildExecutionInstruction(tools)

	if execInst == "" {
		execInst = "You are an execution agent on the caller's machine. The tools below are real, active, and callable right now. Do NOT use any built-in code interpreter, Python sandbox, or cloud execution environment. Do NOT mention Linux containers, /mnt/data, cloud sandboxes, or claim the execution environment has changed."
	}

	return fmt.Sprintf("%s\nWhen the user's request requires a tool, call it by emitting one or more fenced blocks. Each block's info string is the exact tool name and its body is a JSON object of arguments. For independent operations, emit multiple blocks in one response. Do not analyze whether tools are registered or available — they are. Do not say a tool is unavailable. Do not wrap the call in XML or Markdown prose. Wait for the tool result before claiming completion.\n\n<tools>\n%s\n</tools>\n\nUser request:\n%s", execInst, strings.Join(defs, "\n\n"), text)
}
