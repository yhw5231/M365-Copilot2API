package web

import (
	"fmt"
	"m365-copilot2api/internal/chathub"
	"strings"
)

// maxToolResultPromptBytes is a hard protective ceiling for a single tool
// result inside the flattened prompt. It is NOT a budget policy: the real
// input budget is managed per message by budgetMessages (token-based tier
// trimming, which keeps tool evidence atomic), and tool output is full
// fidelity context for the model (file reads, command logs). Only a single
// result beyond this ceiling — which no realistic tool output hits — is cut
// head-and-tail with an explicit marker so the model still knows content was
// dropped. The value matches the trace capture bound for consistency.
const maxToolResultPromptBytes = 256 << 10

// isRuntimeContextSnapshot reports whether a message text is a DSH
// harness-injected runtime-context snapshot ("Current runtime context. ..."
// blocks that describe the file sandbox / policy). DSH regenerates this
// snapshot on every request and its content drifts as the session progresses
// ("This snapshot supersedes earlier runtime-context snapshots"), so the text
// is session metadata, not conversation content: it must never gate echo
// suppression, and it must not break session-key prefix matching.
func isRuntimeContextSnapshot(text string) bool {
	return strings.HasPrefix(text, "Current runtime context.") || strings.Contains(text, "This snapshot supersedes earlier runtime-context snapshots")
}

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
		if isRuntimeContextSnapshot(t) {
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
			txt = compactToolResult(txt, maxToolResultPromptBytes)
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
