package web

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"m365-copilot2api/internal/chathub"
)

// toolReminderEnabled reports whether the gateway injects the per-request
// tool-availability reminder into the flattened prompt. It is ON by default.
// Precedence: an explicit M365_INJECT_TOOL_REMINDER environment variable wins,
// then the persisted runtime setting (injectToolReminder), then true.
//
// Rationale: long-running goal sessions occasionally drift into a state where
// the model claims the workspace lacks file/shell tools even though the client
// declared them. The reminder is a cheap, deterministic correction that travels
// with every request.
func toolReminderEnabled() bool {
	if raw, ok := os.LookupEnv("M365_INJECT_TOOL_REMINDER"); ok && strings.TrimSpace(raw) != "" {
		return envBoolDefault("M365_INJECT_TOOL_REMINDER", true)
	}
	return currentSettings().InjectToolReminder
}

// toolNamesFrom extracts the declared function names from a client tool list.
func toolNamesFrom(tools []chathub.Tool) []string {
	seen := make(map[string]bool, len(tools))
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		var f struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(t.Function, &f) != nil || f.Name == "" {
			continue
		}
		if seen[f.Name] {
			continue
		}
		seen[f.Name] = true
		names = append(names, f.Name)
	}
	return names
}

// injectToolReminder appends one system message that re-states the declared
// tool list and guards against the two failure patterns observed in dead-loop
// goal sessions: (1) the model concluding tools are unavailable, and
// (2) submitting an edit whose old_string equals new_string. It appends AFTER
// the context budget ran, so the reminder is never trimmed away, and it is the
// only system message the gateway adds: the client's own history never carries
// it back, so each request receives exactly one copy.
func injectToolReminder(messages []oaiMsg, tools []chathub.Tool) []oaiMsg {
	if !toolReminderEnabled() || len(tools) == 0 {
		return messages
	}
	names := toolNamesFrom(tools)
	if len(names) == 0 {
		return messages
	}
	list := strings.Join(names, ", ")
	text := fmt.Sprintf(`[TOOL_REMINDER] Declared tools in this session: %s.

Do NOT claim these tools are unavailable, and do NOT claim the workspace lacks file or shell access: their schemas are in your instructions alongside this message. Before editing a file, read it first and make sure old_string and new_string DIFFER — an edit whose old and new strings are identical is rejected. End this round with at least one verified tool call or a completed change; a status-only message does not advance the task.`, list)
	return append(messages, oaiMsg{Role: "system", Content: text})
}
