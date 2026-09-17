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

// TestForceGoalRoundToolChoiceGoalComplete verifies the exemption is keyed on
// the CLIENT goal's closure, not on the server-side ledger's. A closed client
// goal (the harness's <goal_complete> block, or the client's own
// update_goal(complete) call) must not be forced: there the text-only reply IS
// the deliverable, and forcing a tool call recreates the closure loop where the
// model echoes harmless Write-Output calls instead of delivering the answer.
//
// A complete server-side ledger whose client goal is still OPEN is the opposite
// case: the only action that ends the goal-round loop is the client's
// update_goal call, so that round must be forced rather than exempted.
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
	mkClose := func() oaiMsg {
		return oaiMsg{Role: "assistant", ToolCalls: []map[string]any{{"id": "c2", "type": "function", "function": map[string]any{"name": "update_goal", "arguments": `{"action":"complete","goal_id":"goal-1","revision":3}`}}}}
	}

	// Baseline: an ACTIVE ledger with a text-only previous reply still forces.
	if got := forceGoalRoundToolChoice([]oaiMsg{mkText("目标尚未完成"), goalRound}, active, tools); !got {
		t.Fatal("active ledger + text-only reply must force required (baseline)")
	}
	// Completed ledger with the client goal still open: a text-only closing
	// summary must be forced — that call is what closes the client goal.
	if got := forceGoalRoundToolChoice([]oaiMsg{mkText("目标已完成并关闭"), goalRound}, done, tools); !got {
		t.Fatal("completed ledger + open client goal must force the closure call")
	}
	// Once the client closed its goal, the same text-only reply is the
	// deliverable and must not be forced.
	closedHistory := []oaiMsg{mkClose(), goalRound}
	if got := forceGoalRoundToolChoice(closedHistory, done, tools); got {
		t.Fatal("client-closed goal must not force required on a text-only closing reply")
	}
	// A tool-call reply in the previous round is never a text-only stall.
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
