package web

import (
	"strconv"
	"strings"
	"testing"
)

// completedWithContent is a complete server-side ledger that has actually run
// work — the transient content that must be wiped once the client goal is
// closed too.
func completedWithContent() *taskLedger {
	t := &taskLedger{
		OriginalGoal: closeSpinObjective,
		GoalID:       "goal-7f21",
		Constraints:  []string{"不得修改生产数据"},
		Executed:     []string{"create_goal(...)", "pwsh(pytest -q)"},
		ToolResults: []toolEvidence{
			{ID: "fc_1", Name: "pwsh", Arguments: `{"command":"pytest -q"}`, Result: "470 passed"},
		},
		Remaining: []string{"撰写总结"},
		Failures:  []string{"一次探针超时"},
		Switches:  []string{"acc-a→acc-b"},
	}
	t.markComplete("验证完成：470 项测试全部通过")
	return t
}

// TestClearAfterClientGoalClosedWipesContentKeepsTerminalState verifies the
// cleanup itself: the transient fields go, the terminal record (including the
// completed objective, which maybeRotateGoal compares against) stays, and the
// ledger stops rendering any prompt context.
func TestClearAfterClientGoalClosedWipesContentKeepsTerminalState(t *testing.T) {
	task := completedWithContent()
	task.clearAfterClientGoalClosed()

	if len(task.Constraints) != 0 || len(task.Executed) != 0 ||
		len(task.ToolResults) != 0 || len(task.Remaining) != 0 || len(task.Failures) != 0 || len(task.Switches) != 0 {
		t.Fatalf("transient content must be wiped: %+v", task)
	}
	if !task.ContentCleared {
		t.Fatal("the clear flag must be set")
	}
	if !task.IsComplete() || task.GoalID != "goal-7f21" || task.CompletedAt == nil ||
		!strings.Contains(task.CompletedReason, "470 项测试全部通过") {
		t.Fatalf("terminal state must survive the clear: status=%q goal=%q reason=%q", task.Status, task.GoalID, task.CompletedReason)
	}
	// The completed objective is deliberately kept: without it, maybeRotateGoal
	// would treat a replay of the finished goal's own rounds as a new
	// objective and re-open the closed ledger.
	if task.OriginalGoal != closeSpinObjective {
		t.Fatalf("completed objective must be retained for rotation comparison, got %q", task.OriginalGoal)
	}
	if ctx := task.contextForClientGoal(false); ctx != "" {
		t.Fatalf("cleared ledger must render no context: %q", ctx)
	}
	if suffix := task.goalRoundInjectedContext(closeSpinHistory()); suffix != "" {
		t.Fatalf("cleared ledger must render no completion suffix: %q", suffix)
	}
}

// TestClearAfterClientGoalClosedOnlyAppliesToComplete guards the cleanup
// precondition: only the complete state may be wiped. An active or blocked
// ledger keeps its content — an open goal still needs the injected context to
// stay on track (and the closure rounds need it to close the client goal).
func TestClearAfterClientGoalClosedOnlyAppliesToComplete(t *testing.T) {
	active := &taskLedger{OriginalGoal: closeSpinObjective, GoalID: "goal-7f21"}
	active.clearAfterClientGoalClosed()
	if active.OriginalGoal == "" {
		t.Fatal("an active ledger must not be cleared")
	}
	blocked := &taskLedger{OriginalGoal: closeSpinObjective, Status: taskStatusBlocked}
	blocked.clearAfterClientGoalClosed()
	if blocked.OriginalGoal == "" {
		t.Fatal("a blocked ledger must not be cleared")
	}
}

// TestClearedLedgerStopsInjectionIntoNewTaskRequest is the regression for the
// reported symptom: after the goal completed and the CLIENT closed it, the
// next plain message in the same conversation must not receive the completed
// ledger block nor the completion suffix — both made the model open its answer
// with the previous goal's closing summary ("对 … 的全面代码审查已经结束并关
// 闭") before handling the new task.
func TestClearedLedgerStopsInjectionIntoNewTaskRequest(t *testing.T) {
	// The request after the harness closed the goal: the old goal rounds and
	// the closure call replay in the history, the terminal <goal_complete>
	// block is present, and the newest user message is a plain new task.
	history := append(append(append([]oaiMsg{}, closeSpinHistory()...),
		closeSpinToolCall("fc_close", "update_goal", `{"action":"complete","goal_id":"goal-7f21","revision":2}`),
		oaiMsg{Role: "tool", ToolCallID: "fc_close", Content: `{"goal":{"id":"goal-7f21","phase":"complete"}}`},
		closeSpinRound(3),
		oaiMsg{Role: "assistant", Content: "对 ythh-1 的全面代码审查已经结束并关闭。"},
		oaiMsg{Role: "user", Content: "<goal_complete>\nObjective: \"" + closeSpinObjective + "\"\nDo not call any more tools; write the closing message."},
	), oaiMsg{Role: "user", Content: "下一个任务：审查 D:\\NET\\ai\\other-proj 的依赖安全。"})

	task := completedWithContent()
	// Mirror the request path: rotate first, then the client-goal split, then
	// clear when complete + client closed.
	task.maybeRotateGoal(history)
	if task.IsComplete() && !clientGoalClosed(history, task) {
		t.Fatal("the client closed the goal: the request must not be classified as an open client goal")
	}
	if !goalRoundRequest(history, task, closeSpinTools()) {
		t.Fatal("baseline: the replayed goal rounds must register as a goal round, or this test no longer guards the injection path")
	}
	// Without the clear this request DID receive the old-goal context — prove
	// the regression exists, then prove the clear fixes it.
	dirty := completedWithContent()
	if prompt := withTaskLedgerFor("下一个任务：审查 D:\\NET\\ai\\other-proj 的依赖安全。", dirty, false); !strings.Contains(prompt, "[TASK_LEDGER]") {
		t.Fatal("baseline: an uncleared complete ledger must inject the ledger block")
	}
	if suffix := dirty.goalRoundInjectedContext(history); !strings.Contains(suffix, "State the recorded outcome") {
		t.Fatal("baseline: an uncleared complete ledger must inject the completion suffix")
	}

	task.clearAfterClientGoalClosed()
	newTask := "下一个任务：审查 D:\\NET\\ai\\other-proj 的依赖安全。"
	prompt := withTaskLedgerFor(newTask, task, false)
	if prompt != newTask {
		t.Fatalf("plain continuation must not receive the completed ledger block:\n%s", prompt)
	}
	if suffix := task.goalRoundInjectedContext(history); suffix != "" {
		t.Fatalf("plain continuation must not receive the completion suffix: %q", suffix)
	}
	// The terminal protection must survive the clear: the ledger stays
	// complete, so nothing re-opens the finished goal.
	task.maybeRotateGoal(closeSpinHistory())
	if !task.IsComplete() {
		t.Fatal("a replay of the SAME objective must not re-open the cleared ledger")
	}
}

// TestClearedLedgerStillRotatesForNewGoal verifies that a goal chained onto
// the same session after the cleared completion still opens a fresh active
// ledger for the new objective.
func TestClearedLedgerStillRotatesForNewGoal(t *testing.T) {
	task := completedWithContent()
	task.clearAfterClientGoalClosed()

	chained := []oaiMsg{closeSpinRoundObjective("第二个目标：评估 other-proj 的上游依赖", 1)}
	if !task.maybeRotateGoal(chained) {
		t.Fatal("a new objective must rotate the cleared ledger")
	}
	if task.Status != "" || task.ContentCleared {
		t.Fatalf("rotated ledger must be active and re-injectable: status=%q cleared=%v", task.Status, task.ContentCleared)
	}
	if !sameGoalObjective(task.OriginalGoal, "第二个目标：评估 other-proj 的上游依赖") {
		t.Fatalf("rotated ledger must carry the new objective, got %q", task.OriginalGoal)
	}
}

// closeSpinRoundObjective builds a goal-protocol round message for an arbitrary
// objective (closeSpinRound is pinned to the shared closeSpinObjective).
func closeSpinRoundObjective(objective string, n int) oaiMsg {
	return oaiMsg{Role: "user", Content: "<goal_round>\nObjective: \"" + objective + "\"\nRound: " +
		strconv.Itoa(n) + "/20\n\nContinue working toward the objective …\n</goal_round>"}
}
