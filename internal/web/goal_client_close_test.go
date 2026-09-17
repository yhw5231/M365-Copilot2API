package web

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"m365-copilot2api/internal/chathub"
)

// The failing DSH session this suite guards: a goal round produced a complete
// deliverable, the gateway auto-closed its ledger from the model's completion
// wording, but the model never called the CLIENT's update_goal tool. Every
// following round the harness re-injected the same goal round, the model
// answered with the same text-only "目标已完成并关闭" summary, and the goal was
// finally ended by the harness as blocked(round-limit) after 20 rounds.
const closeSpinObjective = "检查当前项目对三阶段招聘系统建设计划的实际完成进度，并实现和验证所有尚未完成的部分。"

func closeSpinTools() []chathub.Tool {
	names := []string{"pwsh", "read", "edit", "create_goal", "get_goal", "update_goal"}
	out := make([]chathub.Tool, 0, len(names))
	for _, n := range names {
		fn, _ := json.Marshal(map[string]any{"name": n, "parameters": map[string]any{"type": "object"}})
		out = append(out, chathub.Tool{Type: "function", Function: fn})
	}
	return out
}

func closeSpinRound(n int) oaiMsg {
	return oaiMsg{Role: "user", Content: "<goal_round>\nObjective: \"" + closeSpinObjective + "\"\nRound: " +
		strconv.Itoa(n) + "/20\n\nContinue working toward the objective …\n</goal_round>"}
}

func closeSpinToolCall(id, name, args string) oaiMsg {
	return oaiMsg{Role: "assistant", ToolCalls: []map[string]any{
		{"id": id, "type": "function", "function": map[string]any{"name": name, "arguments": args}},
	}}
}

// closeSpinHistory is the request history of the round right after the failing
// one: the goal round that produced the deliverable, closed only in the model's
// own wording, followed by the next injected round.
func closeSpinHistory() []oaiMsg {
	return []oaiMsg{
		{Role: "system", Content: "You are an AI agent powered by DeepSeek Harness."},
		{Role: "user", Content: "第一阶段：1 至 2 周 … 现在检查完成进度并完成剩余部分"},
		closeSpinToolCall("fc_create", "create_goal", `{"max_goal_rounds":20,"objective":"`+closeSpinObjective+`"}`),
		{Role: "tool", ToolCallID: "fc_create", Content: `{"goal":{"id":"goal-7f21","revision":1,"phase":"active","roundsStarted":0,"maxGoalRounds":20}}`},
		closeSpinRound(1),
		closeSpinToolCall("fc_get", "get_goal", `{}`),
		{Role: "tool", ToolCallID: "fc_get", Content: `{"goal":{"id":"goal-7f21","revision":1,"phase":"active","roundsStarted":1}}`},
		{Role: "assistant", Content: "三阶段招聘系统建设目标已完成并通过验收。\n\n- 自动化验证：463 个测试全部通过，分支覆盖率 80.53%"},
		closeSpinRound(2),
	}
}

// completedLedger is the server-side ledger state after that first goal round:
// the completion-wording correction closed it, the client goal never was.
func completedLedger() *taskLedger {
	t := &taskLedger{OriginalGoal: closeSpinObjective, GoalID: "goal-7f21"}
	t.markComplete("server-side correction: final answer states completion with tool evidence")
	return t
}

// TestClientGoalOpenKeepsClosurePressure verifies the fix for the DSH "goal
// complete and closed" spin: while the client goal is open, a complete
// server-side ledger must NOT suppress the closure pressure — the round keeps
// the tool reminder, forces tool_choice=required, and the injected context
// names the client's update_goal call instead of telling the model to restate
// the outcome and stop.
func TestClientGoalOpenKeepsClosurePressure(t *testing.T) {
	t.Setenv("M365_INJECT_TOOL_REMINDER", "1")
	history, task, tools := closeSpinHistory(), completedLedger(), closeSpinTools()

	if clientGoalClosed(history, task) {
		t.Fatal("no update_goal call in the history: the client goal must count as open")
	}
	if goalCompleteRound(history, task) {
		t.Fatal("an open client goal must not be treated as a terminal round")
	}
	if got := injectToolReminder(history, tools, task); len(got) != len(history)+1 {
		t.Fatalf("tool reminder must stay for an open client goal: len=%d want %d", len(got), len(history)+1)
	}
	if !forceGoalRoundToolChoice(history, task, tools) {
		t.Fatal("text-only closing summary with an open client goal must force tool_choice=required")
	}

	suffix := task.goalRoundInjectedContext(history)
	for _, want := range []string{"CLIENT_GOAL: OPEN", "update_goal(action=complete)", "get_goal"} {
		if !strings.Contains(suffix, want) {
			t.Fatalf("closure context missing %q: %s", want, suffix)
		}
	}
	if strings.Contains(suffix, "no further update_goal call is required") {
		t.Fatalf("open client goal must not be told no further call is needed: %s", suffix)
	}
	if strings.Contains(suffix, "server-side goal is closed") {
		t.Fatalf("open client goal must not be described as closed: %s", suffix)
	}

	// The ledger block itself must not carry the terminal "restate and stop"
	// rule while the client goal is open.
	ctx := task.contextForClientGoal(true)
	if !strings.Contains(ctx, "CLIENT_GOAL: open") || !strings.Contains(ctx, "update_goal(action=complete)") {
		t.Fatalf("ledger block must name the remaining closure action: %s", ctx)
	}
	if strings.Contains(ctx, "Restate the outcome and stop") {
		t.Fatalf("ledger block must not tell the model to stop while the goal is open: %s", ctx)
	}
	// The terminal rendering is preserved for the closed case.
	if closed := task.contextForClientGoal(false); !strings.Contains(closed, "Restate the outcome and stop") {
		t.Fatalf("client-closed ledger must keep the terminal rule: %s", closed)
	}
}

// TestClientGoalClosedEndsClosurePressure verifies the other direction: once
// the client itself closed the goal — its own update_goal(complete) call, or
// the harness's <goal_complete> block — the round is terminal again and the
// closing text is the deliverable.
func TestClientGoalClosedEndsClosurePressure(t *testing.T) {
	t.Setenv("M365_INJECT_TOOL_REMINDER", "1")
	task, tools := completedLedger(), closeSpinTools()

	byCall := append(closeSpinHistory()[:len(closeSpinHistory())-1],
		closeSpinToolCall("fc_close", "update_goal", `{"action":"complete","goal_id":"goal-7f21","revision":2}`),
		oaiMsg{Role: "tool", ToolCallID: "fc_close", Content: `{"goal":{"id":"goal-7f21","phase":"complete"}}`},
		closeSpinRound(3),
	)
	if !clientGoalClosed(byCall, task) {
		t.Fatal("update_goal(action=complete) in the history must count as closed")
	}
	if !goalCompleteRound(byCall, task) {
		t.Fatal("closed client goal must be terminal")
	}
	if got := injectToolReminder(byCall, tools, task); len(got) != len(byCall) {
		t.Fatalf("closed client goal must not receive tool pressure: len=%d want %d", len(got), len(byCall))
	}
	if forceGoalRoundToolChoice(byCall, task, tools) {
		t.Fatal("closed client goal must not force tool_choice=required")
	}
	if suffix := task.goalRoundInjectedContext(byCall); !strings.Contains(suffix, "no further update_goal call is required") {
		t.Fatalf("closed client goal context must not ask for another call: %s", suffix)
	}

	byBlock := append(append([]oaiMsg{}, closeSpinHistory()...),
		oaiMsg{Role: "user", Content: "<goal_complete>\nObjective: \"" + closeSpinObjective + "\"\nDo not call any more tools; write the closing message."})
	if !clientGoalClosed(byBlock, task) || !goalCompleteRound(byBlock, task) {
		t.Fatal("<goal_complete> must count as a closed client goal")
	}
}

// TestClientGoalClosedIgnoresOtherAndRetiredGoals guards the id matching: a
// closure call for a different (or retired) goal must not mark the current goal
// closed, or the gateway would drop the pressure that the live goal still
// needs.
func TestClientGoalClosedIgnoresOtherAndRetiredGoals(t *testing.T) {
	task := completedLedger()

	other := []oaiMsg{closeSpinToolCall("fc_x", "update_goal", `{"action":"complete","goal_id":"goal-other","revision":9}`)}
	if clientGoalClosed(other, task) {
		t.Fatal("another goal's closure must not close this goal")
	}
	// Without an id in the call the completion is accepted (clients that omit
	// goal_id) — but never for a retired goal id.
	retired := completedLedger()
	retired.RetiredGoalIDs = map[string]bool{"goal-retired": true}
	if !clientGoalClosed([]oaiMsg{closeSpinToolCall("fc_y", "update_goal", `{"action":"complete"}`)}, retired) {
		t.Fatal("id-less update_goal(complete) must count as closed")
	}
	if clientGoalClosed([]oaiMsg{closeSpinToolCall("fc_z", "update_goal", `{"action":"complete","goal_id":"goal-retired"}`)}, retired) {
		t.Fatal("retired goal id must not count as closed")
	}
	// Non-complete actions never close anything.
	if clientGoalClosed([]oaiMsg{closeSpinToolCall("fc_p", "update_goal", `{"action":"pause","goal_id":"goal-7f21"}`)}, task) {
		t.Fatal("pause must not count as closed")
	}
	// A goal chained onto the same session (different objective) is not closed
	// by the previous goal's closure call.
	chained := append([]oaiMsg{
		closeSpinToolCall("fc_a", "update_goal", `{"action":"complete","goal_id":"goal-7f21","revision":4}`),
	}, oaiMsg{Role: "user", Content: "<goal_round>\nObjective: \"新的第二阶段目标\"\nRound: 1/20\nContinue..."})
	if clientGoalClosed(chained, task) {
		t.Fatal("a new goal's round must not inherit the previous goal's closure")
	}
}

// TestClosureSpinStallCountsRounds verifies the repeated-summary detector that
// states how many rounds were already spent restating the outcome.
func TestClosureSpinStallCountsRounds(t *testing.T) {
	history := closeSpinHistory()
	if got := textOnlyGoalRounds(history); got != 1 {
		t.Fatalf("one completed text-only goal round: got %d want 1", got)
	}
	// Two more identical rounds, the shape of the observed spin.
	spin := append(append([]oaiMsg{}, history...),
		oaiMsg{Role: "assistant", Content: "三阶段招聘系统建设目标已完成并关闭。"},
		closeSpinRound(3),
		oaiMsg{Role: "assistant", Content: "三阶段招聘系统建设目标已完成并关闭。"},
		closeSpinRound(4),
	)
	if got := textOnlyGoalRounds(spin); got != 3 {
		t.Fatalf("three text-only goal rounds: got %d want 3", got)
	}
	if suffix := completedLedger().goalRoundInjectedContext(spin); !strings.Contains(suffix, "3 goal rounds already ended") {
		t.Fatalf("stall count must be reported: %s", suffix)
	}
	// A round that ended in a tool call is progress, not a stall.
	withCall := append(append([]oaiMsg{}, spin...),
		closeSpinToolCall("fc_more", "pwsh", `{"command":"pytest -q"}`),
		closeSpinRound(5),
	)
	if got := textOnlyGoalRounds(withCall); got != 3 {
		t.Fatalf("tool-call round must not count as a stall: got %d want 3", got)
	}
}

// TestGoalRoundCounterUsesNewestRound verifies the round budget reads the
// current round. Reading the oldest counter pinned every session at round 1 and
// the exhausted-budget signal never fired.
func TestGoalRoundCounterUsesNewestRound(t *testing.T) {
	history := []oaiMsg{closeSpinRound(1), {Role: "assistant", Content: "…"}, closeSpinRound(2), closeSpinRound(18)}
	cur, max, ok := goalRoundCounter(history)
	if !ok || cur != 18 || max != 20 {
		t.Fatalf("newest round counter: got %d/%d ok=%v want 18/20", cur, max, ok)
	}
	if note := roundBudgetNote(cur, max, false); !strings.Contains(note, "18/20") {
		t.Fatalf("budget note must state the current round: %s", note)
	}
	// The open-client-goal wording asks for the closure call, not for a
	// continuation.
	note := roundBudgetNote(20, 20, true)
	if !strings.Contains(note, "update_goal(action=complete)") || strings.Contains(note, "if the work is done") {
		t.Fatalf("open client goal budget note must ask for the closure call: %s", note)
	}
}

// TestRequiredRoundKeepsOutcomeTextWhenLedgerComplete verifies the forced
// round's text-only escape also covers the completed-ledger state: the text is
// then the outcome statement (never a mid-task stall), so it must be delivered
// instead of being destroyed by the 502 gate.
func TestRequiredRoundKeepsOutcomeTextWhenLedgerComplete(t *testing.T) {
	// A terse closing line that the deliverable detector alone would reject.
	terse := "目标已完成。"
	if requiredRetryDeliverable(terse) {
		t.Fatal("baseline: a terse line is not a detected deliverable")
	}
	if !requiredRoundTextIsDeliverable(terse, completedLedger()) {
		t.Fatal("completed ledger: the outcome text must be deliverable, not a 502")
	}
	// An active ledger keeps the strict gate.
	if requiredRoundTextIsDeliverable(terse, &taskLedger{GoalID: "goal-7f21"}) {
		t.Fatal("active ledger: a terse status line must still be rejected")
	}
	// A nil ledger cannot exempt anything.
	if requiredRoundTextIsDeliverable(terse, nil) {
		t.Fatal("nil ledger must not exempt the required-round gate")
	}
}

// TestClosureSpinPromptEndToEnd renders the full injected prompt for the failing
// round and asserts the contradiction is gone: the model must not be told both
// "the goal is complete, restate and stop" and "close the client goal".
func TestClosureSpinPromptEndToEnd(t *testing.T) {
	t.Setenv("M365_INJECT_TOOL_REMINDER", "1")
	history, task, tools := closeSpinHistory(), completedLedger(), closeSpinTools()
	clientOpen := task.IsComplete() && !clientGoalClosed(history, task)
	if !clientOpen {
		t.Fatal("failing round must be classified as an open client goal")
	}
	reminded := injectToolReminder(history, tools, task)
	if len(reminded) != len(history)+1 {
		t.Fatal("the failing round must carry the tool reminder")
	}

	prompt := withTaskLedgerFor("[TOOL_REMINDER] Declared tools …", task, clientOpen)
	prompt += task.goalRoundInjectedContext(history)
	for _, want := range []string{"CLIENT_GOAL: open", "CLIENT_GOAL: OPEN", "update_goal(action=complete)"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("injected prompt missing %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "Restate the outcome and stop") {
		t.Fatalf("injected prompt still tells the model to stop:\n%s", prompt)
	}
}
