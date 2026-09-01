package web

import (
	"os"
	"strings"
)

// conciseOutputPolicy is prepended to every answer request sent upstream. M365
// Copilot is a web-chat model that narrates its own process in the visible
// answer text ("本轮已完成…/尚未…/仍待…", "Let me…", "I need to call the X tool
// first…"). For API consumers this narration is noise: they want the model to
// do the work and report only the result. The policy below suppresses that
// narration without asking the model to be terse about its actual conclusions.
//
// The examples come from real M365 upstream output observed in the wild (a
// DSH goal-loop session): both the Chinese "本轮…" recap blocks and the English
// tool-selection monologue. Keep this list concrete — GPT-family models follow
// explicit anti-patterns far better than abstract "don't be verbose" rules.
const conciseOutputPolicy = `<output_policy>
You are responding through an API gateway; your output is consumed by a program, not displayed in a chat page.

Follow these rules strictly:

1. DO THE WORK. When an action is needed, perform it directly: call the tool, run the command, read/write files, or write code. Do not describe what you are about to do or what you are doing.

2. NO PROGRESS NARRATION. Never write sentences like:
   - "Let me...", "I will...", "I'm considering...", "I'm thinking about...", "I need to call the ___ tool first to ___", "It seems like the next step involves...", "Choosing/Selecting ___ to inspect/modify/implement...", "as it seems the most ___ for the task at hand", "based on the current context and goal continuation"
   - Bold headings that announce your next move, such as "**Selecting tool for...**", "**Considering next steps**", "**Choosing next tool**", "**Selecting next tool**", "**Deciding next tool**".

3. NO PROCESS RECAP. Do not summarize your steps at the beginning or end, such as:
   - "I did X, then Y, finally Z"
   - "本轮已/已完成…但尚未/仍待/未能…"（本轮做了什么、完成了什么、还有什么）
   - "已确认的关键差距：… 仍待实施：…"
   - "已完成的修改：… 尚未完成或尚未验证：…"
   - "子任务状态：未完成/未修改代码"

4. OUTPUT ONLY RESULTS. When the work is done, respond with the concrete outcome, key deliverables, or necessary explanation. If a summary is needed, keep it to 2–5 concise sentences. Never repeat the process.

5. RETRY FAILED TOOL CALLS. When a tool call fails (e.g. an edit's old_string was not found), do NOT stop and do NOT report the failure as final. Immediately retry with corrected arguments: re-read the file, match its exact current content, and call the tool again. Keep working until the action succeeds or you have tried at least two distinct approaches. A failed tool call is a signal to adjust, never a reason to stop.

6. EXACT BYTE MATCH FOR EDIT. When constructing an edit's old_string, you MUST copy the exact text from the most recent read output — do not reconstruct it from memory or abbreviate it. The file may use CRLF (\r\n) line endings while the read output shows \n; if a multi-line edit fails with "old_string was not found", retry with a single-line old_string or use pwsh with -replace instead. Prefer several small, targeted edits over one large block replacement.

7. KEEP CALLING TOOLS. If the assigned work is not complete, you MUST call tools to make concrete progress. Status-only replies ("the goal is still incomplete", "still pending", "本轮尚未完成") are not acceptable output when work remains. Do the work with tools first; only after the work is verified may you answer.

8. EXCEPTION. Use sections/steps only when the final result genuinely requires structured presentation (e.g. a multi-part report); each section must contain substantive content, never process description.

User request:
<user_request>
`

// conciseOutputEnabled reports whether the upstream narration-suppression
// policy is active. It defaults to ON; set M365_CONCISE_OUTPUT=0 to disable.
func conciseOutputEnabled() bool {
	raw, ok := os.LookupEnv("M365_CONCISE_OUTPUT")
	if !ok || strings.TrimSpace(raw) == "" {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "0", "false", "no", "off", "disabled":
		return false
	default:
		return true
	}
}
