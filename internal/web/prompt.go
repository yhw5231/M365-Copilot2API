package web

import (
	"fmt"
	"m365-copilot2api/internal/chathub"
	"strings"
)

// userEchoCheckPrompt returns the user's genuine question text for the
// workspace-vocabulary echo check. The full flattened prompt
// (flattenPromptMessages) includes harness-injected blocks — the DSH
// runtime-context snapshot ("Current runtime context ... sandbox ...") and
// goal-round continuation templates — whose vocabulary (e.g. "sandbox") must
// NOT count as the user having asked about the workspace. Feeding the whole
// prompt to userPromptMentionsWorkspace permanently disables the misjudgment
// gate (echo suppression gate 3) for every request, because the snapshot's
// "sandbox" always matches a workspaceEchoTerm. Only the caller's own user-role
// messages (minus injected blocks) are eligible for echo suppression.
func userEchoCheckPrompt(messages []oaiMsg) string {
	var parts []string
	for _, m := range messages {
		if m.Role != "user" {
			continue
		}
		t := strings.TrimSpace(contentToString(m.Content))
		if t == "" {
			continue
		}
		// Skip harness-injected runtime-context snapshots (DSH "Current runtime
		// context. ..." blocks that describe the file sandbox / policy).
		if strings.HasPrefix(t, "Current runtime context.") || strings.Contains(t, "This snapshot supersedes earlier runtime-context snapshots") {
			continue
		}
		// Skip goal-round continuation templates injected by the harness; the
		// Objective inside is still covered by the caller's original question.
		if strings.Contains(t, "<goal_round>") {
			continue
		}
		parts = append(parts, t)
	}
	return strings.Join(parts, "\n")
}

func flattenPromptMessages(messages []oaiMsg, attachments []chathub.Attachment) (string, []chathub.Attachment) {
	var systemParts []string
	var rest []oaiMsg
	for _, m := range messages {
		role := strings.ToLower(strings.TrimSpace(m.Role))
		if role == "system" || role == "developer" {
			txt, sysFiles := parseContent(m.Content)
			attachments = append(attachments, sysFiles...)
			txt = strings.TrimSpace(txt)
			if txt != "" {
				systemParts = append(systemParts, txt)
			}
		} else {
			rest = append(rest, m)
		}
	}
	var b strings.Builder
	if len(systemParts) > 0 {
		b.WriteString("\n[system]\n")
		b.WriteString(strings.Join(systemParts, "\n"))
		b.WriteString("\n")
	}
	for _, m := range rest {
		role := strings.ToLower(strings.TrimSpace(m.Role))
		if role == "" {
			role = "user"
		}
		content := m.Content
		if role == "tool" {
			switch v := content.(type) {
			case nil:
				content = ""
			case string:
			default:
				content = mustJSON(v)
			}
		}
		txt, files := parseContent(content)
		attachments = append(attachments, files...)
		txt = strings.TrimSpace(txt)
		if len(m.ToolCalls) > 0 {
			if txt != "" {
				b.WriteString(fmt.Sprintf("\n[%s]\n%s\n", role, txt))
			}
			b.WriteString(fmt.Sprintf("\n[%s tool_calls]\n%s\n", role, mustJSON(m.ToolCalls)))
			continue
		}
		if role == "tool" {
			txt = compactToolResult(txt, 4000)
			b.WriteString(fmt.Sprintf("\n[tool result id=%s]\n%s\n", m.ToolCallID, txt))
			continue
		}
		if txt == "" {
			continue
		}
		b.WriteString(fmt.Sprintf("\n[%s]\n%s\n", role, txt))
	}
	return strings.TrimSpace(b.String()), attachments
}
