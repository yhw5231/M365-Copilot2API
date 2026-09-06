package web

import (
	"encoding/json"
	"fmt"
	"strings"
)

func modelToolRouterPrompt(prompt string, tools []map[string]any, choice any) string {
	defs, _ := json.Marshal(tools)
	mode := normalizedToolChoiceMode(choice)
	rules := `- If a tool is needed, respond with: CALL_TOOL: tool_name({"arg1":"value1"})
- If no tool is needed, respond with: NO_TOOL_NEEDED
- Only use tools from the available list above
- Validate all arguments against the tool's schema
- Do not invent tools that are not in the list`
	if mode == "required" {
		rules = `- Respond with exactly one CALL_TOOL: tool_name({"arg1":"value1"}) line. You MUST select and call a tool from the available list above — this request requires a tool call.
- NO_TOOL_NEEDED is NOT acceptable: if the user's goal is not yet achieved, you must make progress with a tool call.
- Only use tools from the available list above
- Validate all arguments against the tool's schema
- Do not invent tools that are not in the list`
	}
	// Multi-turn: completed tool evidence (tool[...], tool_calls:) was already
	// acted upon, so re-invoking those tools would duplicate work.
	if strings.Contains(prompt, "tool_calls:") || strings.Contains(prompt, "tool[call_") {
		rules += `
- Completed evidence must not be repeated: tool_calls/tool[call_x] rows are prior results already delivered to the user, never re-invoke them
- Only start a new tool call when fresh unfinished work remains on the current request`
	}
	return fmt.Sprintf(`You are a tool selection assistant. Based on the user request, decide which tool to call next.

Available tools: %s

MODE: %s

Rules:
%s

User request and evidence:
%s`, defs, mode, rules, prompt)
}

// toolCallIntentPrefix reports whether a response text opens with the explicit
// CALL_TOOL protocol token. The router prompt teaches this shape, and models
// sometimes emit it even in modes that were told to use fenced blocks — a
// broken (unescaped / truncated) one must never reach the client as answer
// prose.
func toolCallIntentPrefix(text string) bool {
	t := strings.TrimSpace(text)
	return strings.HasPrefix(t, "CALL_TOOL:") || strings.HasPrefix(t, "call_tool:")
}

// hasBrokenToolCallIntent reports whether the response opens with a CALL_TOOL
// token that failed to parse into a validated tool call. Such text is a
// malformed tool invocation, not an answer: it gets one targeted repair retry
// (re-emit as a properly escaped fenced block) instead of being forwarded to
// the client as content.
func hasBrokenToolCallIntent(text string, tools []map[string]any, choice any) bool {
	if !toolCallIntentPrefix(text) {
		return false
	}
	if calls, parsed := parseModelToolDecision(text, tools, choice); parsed && len(calls) > 0 {
		return false
	}
	return true
}

// stripToolCallProtocolLine removes a leading, unparseable CALL_TOOL protocol
// line (and a trailing truncated protocol fragment on its own line) from a
// response whose tool-call repair failed. The remainder — usually the model's
// own planning prose — is what the client should see instead of the raw
// protocol fragment.
func stripToolCallProtocolLine(text string) string {
	t := strings.TrimSpace(text)
	if !toolCallIntentPrefix(t) {
		return text
	}
	if idx := strings.IndexByte(t, '\n'); idx >= 0 {
		t = strings.TrimSpace(t[idx+1:])
	} else {
		// The whole response is one broken protocol line.
		return ""
	}
	// A truncated trailing fragment (e.g. "text(JSON.stringify(*") that the
	// model appended to the broken call must not survive as prose either.
	for {
		lineEnd := strings.IndexByte(t, '\n')
		line := t
		if lineEnd >= 0 {
			line = t[:lineEnd]
		}
		trimmedLine := strings.TrimSpace(line)
		if trimmedLine == "" || strings.HasPrefix(trimmedLine, "text(JSON.stringify") || strings.HasPrefix(trimmedLine, "CALL_TOOL:") || strings.HasPrefix(trimmedLine, "call_tool:") {
			if lineEnd < 0 {
				return ""
			}
			t = strings.TrimSpace(t[lineEnd+1:])
			continue
		}
		break
	}
	return t
}

// toolShapeReleaseText picks what a held tool-shaped response releases as
// content when it never converted into a tool call: the model's own prose
// with the protocol line stripped, or — when nothing survives the strip
// (the whole response was a single protocol line) — the original text. An
// empty release ends the stream with zero content and surfaces downstream
// as a misleading "empty completion" failure.
func toolShapeReleaseText(text string) string {
	if stripped := stripToolCallProtocolLine(text); stripped != "" {
		return stripped
	}
	return text
}

func parseModelToolDecision(text string, tools []map[string]any, choice any) ([]detectedToolCall, bool) {
	text = strings.TrimSpace(text)
	// Try the new natural language format first: CALL_TOOL: name({...})
	if strings.HasPrefix(text, "CALL_TOOL:") || strings.HasPrefix(text, "call_tool:") {
		parts := strings.SplitN(text, ":", 2)
		if len(parts) == 2 {
			rest := strings.TrimSpace(parts[1])
			start := strings.Index(rest, "(")
			end := strings.LastIndex(rest, ")")
			if start > 0 && end > start {
				name := strings.TrimSpace(rest[:start])
				argsStr := rest[start+1 : end]
				var args map[string]any
				if json.Unmarshal([]byte(argsStr), &args) == nil && toolChoiceAllows(choice, name) {
					fn := toolFunction(name, tools)
					if fn != nil && schemaValid(args, fn) == nil {
						b, _ := json.Marshal(args)
						return []detectedToolCall{{ID: callID(name, string(b), 0), Type: toolType(name, tools), Name: name, Arguments: b}}, true
					}
				}
			}
		}
	}
	if strings.Contains(text, "NO_TOOL_NEEDED") || strings.Contains(text, "no_tool_needed") {
		return nil, true
	}
	// Fallback: try the old JSON format
	if i := strings.Index(text, "```"); i >= 0 {
		text = strings.TrimSpace(strings.TrimPrefix(strings.TrimSuffix(text[i+3:], "```"), "json"))
	}
	start, end := strings.Index(text, "{"), strings.LastIndex(text, "}")
	if start < 0 || end <= start {
		return nil, false
	}
	var probe map[string]json.RawMessage
	if json.Unmarshal([]byte(text[start:end+1]), &probe) != nil {
		return nil, false
	}
	if _, ok := probe["calls"]; !ok {
		return nil, false
	}
	var envelope struct {
		Calls []struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		} `json:"calls"`
	}
	if json.Unmarshal([]byte(text[start:end+1]), &envelope) != nil {
		return nil, false
	}
	out := make([]detectedToolCall, 0, len(envelope.Calls))
	for i, c := range envelope.Calls {
		fn := toolFunction(c.Name, tools)
		if fn == nil || c.Arguments == nil || !toolChoiceAllows(choice, c.Name) || schemaValid(c.Arguments, fn) != nil {
			continue
		}
		b, _ := json.Marshal(c.Arguments)
		out = append(out, detectedToolCall{ID: callID(c.Name, string(b), i), Type: toolType(c.Name, tools), Name: c.Name, Arguments: b})
	}
	return out, true
}
