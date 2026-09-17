package web

import (
	"encoding/json"
	"testing"

	"m365-copilot2api/internal/chathub"
)

// TestForceGoalRoundToolChoice verifies the goal-loop stall detector that
// forces tool_choice=required: a goal-protocol continuation round whose most
// recent assistant reply carried no tool call must be forced, while a round
// whose previous reply did call tools must not be re-forced.
func TestForceGoalRoundToolChoice(t *testing.T) {
	tools := []chathub.Tool{
		{Type: "function", Function: mustRaw(`{"name":"pwsh"}`)},
		{Type: "function", Function: mustRaw(`{"name":"create_goal"}`)},
		{Type: "function", Function: mustRaw(`{"name":"get_goal"}`)},
		{Type: "function", Function: mustRaw(`{"name":"update_goal"}`)},
	}
	task := &taskLedger{GoalID: "goal-1"}

	goalRound := oaiMsg{Role: "user", Content: `<goal_round>\nObjective: "fix mypy"\nRound: 2/256\nContinue...`}

	mkAssistant := func(text string, calls int) oaiMsg {
		m := oaiMsg{Role: "assistant", Content: text}
		for i := 0; i < calls; i++ {
			m.ToolCalls = append(m.ToolCalls, map[string]any{"id": "c", "type": "function", "function": map[string]any{"name": "pwsh", "arguments": `{"cmd":"dir"}`}})
		}
		return m
	}

	cases := []struct {
		name     string
		messages []oaiMsg
		want     bool
	}{
		{"text-only previous reply in goal round forces required",
			[]oaiMsg{mkAssistant("目标尚未完成，现有证据确认...", 0), goalRound}, true},
		{"previous reply with tool call is not forced",
			[]oaiMsg{mkAssistant("", 1), goalRound}, false},
		{"no goal round structure is not forced",
			[]oaiMsg{mkAssistant("普通问题", 0), {Role: "user", Content: "请继续"}}, false},
		{"empty assistant text is skipped (no text-only stall to break)",
			[]oaiMsg{mkAssistant("", 0), mkAssistant("", 0), goalRound}, false},
		{"reasoning fragment followed by its tool call is not a status report",
			[]oaiMsg{mkAssistant("<thinking>need to run the full regression</thinking>", 0), mkAssistant("", 1), goalRound}, false},
		{"a real text-only report after a tool round still forces required",
			[]oaiMsg{mkAssistant("", 1), mkAssistant("目标尚未完成，现有证据确认...", 0), goalRound}, true},
	}
	for _, c := range cases {
		got := forceGoalRoundToolChoice(c.messages, task, tools)
		if got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

// TestForceGoalRoundToolChoiceGoalComplete verifies that a completed goal never
// forces tool_choice=required. After completion the text-only reply IS the
// deliverable (the closing message the client's <goal_complete> block demands);
// forcing a tool call would recreate the closure loop where the model echoes
// harmless Write-Output calls instead of delivering the final answer.
func TestForceGoalRoundToolChoiceGoalComplete(t *testing.T) {
	tools := []chathub.Tool{
		{Type: "function", Function: mustRaw(`{"name":"pwsh"}`)},
		{Type: "function", Function: mustRaw(`{"name":"update_goal"}`)},
	}
	active := &taskLedger{GoalID: "goal-1"}
	done := &taskLedger{GoalID: "goal-1", Status: taskStatusComplete}

	goalRound := oaiMsg{Role: "user", Content: `<goal_round>\nObjective: "fix mypy"\nRound: 2/256\nContinue...`}
	mkText := func(text string) oaiMsg { return oaiMsg{Role: "assistant", Content: text} }
	mkTool := func() oaiMsg {
		return oaiMsg{Role: "assistant", ToolCalls: []map[string]any{{"id": "c", "type": "function", "function": map[string]any{"name": "pwsh", "arguments": `{"cmd":"dir"}`}}}}
	}

	// Baseline: an ACTIVE ledger with a text-only previous reply still forces.
	if got := forceGoalRoundToolChoice([]oaiMsg{mkText("目标尚未完成"), goalRound}, active, tools); !got {
		t.Fatal("active ledger + text-only reply must force required (baseline)")
	}
	// Completed ledger: text-only reply must NOT force.
	if got := forceGoalRoundToolChoice([]oaiMsg{mkText("目标已完成，正在收尾"), goalRound}, done, tools); got {
		t.Fatal("completed ledger must not force required on a text-only closing reply")
	}
	// Completed ledger with a tool-call reply: still not forced (already covered
	// by "no text-only stall", but assert for completeness).
	if got := forceGoalRoundToolChoice([]oaiMsg{mkTool(), goalRound}, done, tools); got {
		t.Fatal("completed ledger must not force required after a tool call")
	}
	// The CLIENT-declared <goal_complete> block exempts the round even with an
	// active server-side ledger.
	complete := oaiMsg{Role: "user", Content: "<goal_complete>\nObjective: \"x\"\nDo not call any more tools; write the closing message."}
	if got := forceGoalRoundToolChoice([]oaiMsg{mkText("收尾中"), complete}, active, tools); got {
		t.Fatal("<goal_complete> in the round must not force tool_choice=required")
	}
}

func mustRaw(s string) json.RawMessage { return json.RawMessage(s) }
