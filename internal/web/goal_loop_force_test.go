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
	}
	for _, c := range cases {
		got := forceGoalRoundToolChoice(c.messages, task, tools)
		if got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func mustRaw(s string) json.RawMessage { return json.RawMessage(s) }
