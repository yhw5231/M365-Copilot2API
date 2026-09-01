package web

import (
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

// extractToolNames extracts a flat name set from tools that may be in either
// of two formats:
//
//	(a) wrapped: {"type":"function","function":{"name":"bash","description":"...","parameters":{...}}}
//	(b) flat:    {"type":"custom","name":"exec","description":"..."}
func extractToolNames(tools []map[string]any) map[string]bool {
	out := map[string]bool{}
	for _, t := range tools {
		// Wrapped format: the "function" sub-object carries the name.
		if f, ok := t["function"].(map[string]any); ok {
			if n, _ := f["name"].(string); n != "" {
				out[n] = true
			}
			continue
		}
		// Flat format (Responses API, custom tools): "name" at top level.
		if n, _ := t["name"].(string); n != "" {
			out[n] = true
		}
	}
	return out
}

// declaredToolName returns the name of the first tool matching candidates
// that the client actually declared, or "" if none.
func declaredToolName(allowed map[string]bool, candidates []string) string {
	for _, n := range candidates {
		if allowed[n] {
			return n
		}
	}
	return ""
}

// dynamicShellName returns the actual shell tool name declared by the caller,
// or "" if no shell-like tool was declared.
func dynamicShellName(tools []map[string]any) string {
	return declaredToolName(extractToolNames(tools), knownShellNames)
}

// hasDeclaredTools checks whether the caller declared any tool whose name
// matches one of the given candidates.
func hasDeclaredTools(tools []map[string]any, candidates []string) bool {
	allowed := extractToolNames(tools)
	for _, n := range candidates {
		if allowed[n] {
			return true
		}
	}
	return false
}

// shellDisplayName returns a human-readable description for a shell tool name,
// suitable for inclusion in model instructions.
func shellDisplayName(name string) string {
	switch name {
	case "workspace_shell":
		return "workspace_shell (runs shell commands in the project workspace)"
	case "bash":
		return "bash (runs Bash on the caller's local machine)"
	case "sh":
		return "sh (runs POSIX shell commands)"
	case "shell":
		return "shell (runs operating-system commands)"
	case "powershell":
		return "powershell (runs PowerShell on the caller's Windows machine)"
	case "pwsh":
		return "pwsh (runs PowerShell (Core) on the caller's machine)"
	case "cmd":
		return "cmd (runs Windows Command Prompt)"
	default:
		return name
	}
}

// shellRuntimeHint returns a one-sentence hint about how the shell tool
// actually executes — used to counter sandbox hallucinations.
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

// unifiedWorkspaceInstruction builds a workspace/execution instruction
// tailored to the actual tools the caller declared. If no execution or file
// tool is present it returns an empty string (caller should fall back to a
// generic instruction).
func unifiedWorkspaceInstruction(tools []map[string]any) string {
	allowed := extractToolNames(tools)
	shell := declaredToolName(allowed, knownShellNames)
	execTool := declaredToolName(allowed, knownExecToolNames)
	hasFileTools := hasDeclaredTools(tools, knownFileToolNames)
	hasShell := shell != ""
	hasExec := execTool != ""

	if !hasShell && !hasExec && !hasFileTools {
		return ""
	}

	var b strings.Builder
	b.WriteString("You are operating through the caller's local execution bridge. ")
	b.WriteString("Use only the tools declared in this session; do not attempt Microsoft 365/Copilot native tools. ")

	if hasShell {
		if shellType := shellRuntimeHint(shell); shellType != "" {
			b.WriteString(fmt.Sprintf("The **%s** tool is active and %s. ", shellDisplayName(shell), shellType))
		}
		b.WriteString(fmt.Sprintf("Run all code and commands with the **%s** tool — it executes directly on the caller's local machine. ", shell))
	} else if hasExec {
		b.WriteString("The execution tool is the caller-provided custom exec tool, which runs directly on the caller's local machine. ")
	}

	b.WriteString("You have full access to the caller's local machine and its files. ")

	if hasFileTools {
		b.WriteString("The file tools **read**, **write**, **edit**, **glob**, and **grep** are available and active in this session. ")
		b.WriteString("Modify files within the scope of the caller's project workspace. ")
		b.WriteString("Work with the paths in the workspace: use absolute project paths or paths relative to the caller-selected project workspace. ")
	}

	b.WriteString("Only report a file as created, modified, or verified after ")
	if hasShell || hasExec {
		target := shell
		if target == "" {
			target = execTool
		}
		b.WriteString(fmt.Sprintf("the **%s** tool ", target))
	} else {
		b.WriteString("the appropriate tool ")
	}
	b.WriteString("returns a successful result. After every execution, verify the result using the same tool.")

	return b.String()
}

// unifiedSandboxCorrection builds a correction prompt for when the model
// hallucinates a sandbox/cloud execution environment instead of using the
// caller's declared tools. Returns the full correction text ready to be
// prepended to the user request.
func unifiedSandboxCorrection(tools []map[string]any, userRequest string) string {
	allowed := extractToolNames(tools)
	shell := declaredToolName(allowed, knownShellNames)
	execTool := declaredToolName(allowed, knownExecToolNames)
	target := shell
	if target == "" {
		target = execTool
	}

	var b strings.Builder
	b.WriteString("CRITICAL: You must NOT use any built-in code interpreter, Python sandbox, or cloud execution environment. ")

	if target != "" {
		hint := shellRuntimeHint(target)
		b.WriteString(fmt.Sprintf("The caller has provided the **%s** tool which %s. ", shellDisplayName(target), hint))
		b.WriteString("Use it to execute any commands or code. ")
	} else {
		b.WriteString("The caller has provided execution tools on their local machine. ")
	}

	b.WriteString("Do NOT say you cannot run code. ")
	b.WriteString("Do NOT say you only have a Linux container. ")
	b.WriteString("Do NOT say you have no Windows execution channel. ")
	b.WriteString("Do NOT claim the project path is unmounted, missing, or NOT_FOUND: the caller's project directory is real and mounted on the caller's local machine, and the declared file/execution tools can access it. If a check reports NOT_FOUND, re-verify with the available tools before concluding the path is inaccessible. ")

	if target != "" {
		b.WriteString(fmt.Sprintf("You DO have the **%s** tool. Call it NOW with the appropriate command. ", target))
	} else {
		b.WriteString("You DO have execution tools. Call the appropriate tool NOW. ")
	}

	b.WriteString("\n\nUser request:\n" + userRequest)
	return b.String()
}

// targetedMisjudgmentCorrection quotes the model's own misjudged claim back at
// it after a generic sandbox correction already failed, so the model can see
// exactly which availability wording was wrong, instead of a list of forbidden
// phrases that the model may echo back.
func targetedMisjudgmentCorrection(misjudged string, tools []map[string]any, userRequest string) string {
	base := unifiedSandboxCorrection(tools, userRequest)
	quoted := strLimit(misjudged, 600)
	var b strings.Builder
	b.WriteString(base)
	b.WriteString("\n\nYour previous reply again claimed the workspace or tools are unavailable. ")
	b.WriteString("That claim is FALSE: the caller's tools are real and have been providing real results. ")
	b.WriteString("Your exact misjudged wording was:\n\"")
	b.WriteString(quoted)
	b.WriteString("\"\n")
	b.WriteString("Re-read the actual file or run the tool again to verify the true state, then give a correct answer. ")
	b.WriteString("Do NOT repeat that claim.")
	return b.String()
}
