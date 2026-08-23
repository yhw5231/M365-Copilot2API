package web

import (
	"encoding/json"
	"regexp"
	"strings"
)

var fencedToolCall = regexp.MustCompile("(?s)```([A-Za-z0-9_-]+)\\s*\\n(.*?)\\n```")

// declaredShell returns the shell-ish tool name the client actually
// declared (bash/sh/shell/powershell/pwsh/cmd/workspace_shell), or "" if
// none. Forcing an undeclared bash call on clients that don't support it
// (issue #12) makes them error out and loop, so conversion only happens for
// declared tools.
func declaredShell(allowed map[string]bool) string {
	for _, n := range knownShellNames {
		if allowed[n] {
			return n
		}
	}
	return ""
}

// isToolCallShapedAnswer reports whether an assistant answer is dominated by
// fenced code blocks — the shape models use when returning tool calls inside
// the response text. A prose answer that merely *contains* code samples (an
// essay, a tutorial, a generated script) is a text response and must never be
// turned into tool invocations: doing so discards the answer and, when the
// detected call fails validation, can fail the whole request.
func isToolCallShapedAnswer(text string) bool {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return false
	}
	outside, inside := 0, 0
	remaining := trimmed
	for {
		start := strings.Index(remaining, "```")
		if start < 0 {
			outside += len(remaining)
			break
		}
		outside += start
		rest := remaining[start+3:]
		end := strings.Index(rest, "```")
		if end < 0 {
			outside += len(rest)
			break
		}
		inside += end
		remaining = rest[end+3:]
	}
	// A tool-call answer is essentially one or more fenced blocks with only a
	// short preamble/trailer. Long prose around the blocks means this is an
	// ordinary text answer that happens to show code.
	return inside > 0 && outside <= 240
}

func fencedToolCalls(text string, tools []map[string]any, choice any) []detectedToolCall {
	if !isToolCallShapedAnswer(text) {
		return nil
	}
	allowed := allowedToolNames(tools)
	shell := declaredShell(allowed)
	var out []detectedToolCall
	for _, m := range fencedToolCall.FindAllStringSubmatch(text, -1) {
		name := m[1]
		args := strings.TrimSpace(m[2])
		var v any
		_ = json.Unmarshal([]byte(args), &v)
		// Auto-convert bash/shell code blocks to tool calls, but only when
		// the client declared the tool.
		if name == "bash" || name == "sh" || name == "shell" || name == "powershell" || name == "pwsh" || name == "cmd" {
			converted := name
			if !allowed[name] {
				if shell == "" {
					continue
				}
				converted = shell
			}
			if m, ok := v.(map[string]any); ok {
				if cmd, hasCmd := m["command"]; hasCmd && cmd != "" {
					cmdBytes, _ := json.Marshal(map[string]any{"command": cmd, "timeout": m["timeout"], "workdir": m["workdir"]})
					out = append(out, detectedToolCall{ID: callID(converted, string(cmdBytes), len(out)), Type: "function", Name: converted, Arguments: cmdBytes})
					continue
				}
			}
			if v == nil {
				cmdBytes, _ := json.Marshal(map[string]any{"command": args})
				out = append(out, detectedToolCall{ID: callID(converted, string(cmdBytes), len(out)), Type: "function", Name: converted, Arguments: cmdBytes})
				continue
			}
			continue
		}
		if !allowed[name] || !toolChoiceAllows(choice, name) {
			continue
		}
		if v == nil {
			continue
		}
		b, _ := json.Marshal(v)
		out = append(out, detectedToolCall{ID: callID(name, string(b), len(out)), Type: toolType(name, tools), Name: name, Arguments: b})
	}
	// Also check for plain JSON objects with a "command" field (not in fenced blocks)
	if len(out) == 0 && shell != "" {
		for i := 0; i < len(text); i++ {
			if text[i] != '{' {
				continue
			}
			end := strings.Index(text[i:], "\n")
			if end < 0 {
				end = len(text) - i
			}
			line := text[i : i+end]
			braceEnd := strings.LastIndex(line, "}")
			if braceEnd < 0 {
				continue
			}
			if !strings.Contains(line[:braceEnd+1], `"command"`) {
				continue
			}
			var obj map[string]any
			if json.Unmarshal([]byte(line[:braceEnd+1]), &obj) != nil {
				continue
			}
			if cmd, hasCmd := obj["command"]; hasCmd && cmd != "" {
				cmdBytes, _ := json.Marshal(map[string]any{"command": cmd, "timeout": obj["timeout"], "workdir": obj["workdir"]})
				out = append(out, detectedToolCall{ID: callID(shell, string(cmdBytes), len(out)), Type: "function", Name: shell, Arguments: cmdBytes})
				break
			}
		}
	}
	return out
}
