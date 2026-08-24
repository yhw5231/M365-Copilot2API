package web

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"m365-copilot2api/internal/chathub"
)

func TestBuildTaskLedgerCapturesGoalAndConstraints(t *testing.T) {
	body := &oaiReq{Messages: []oaiMsg{
		{Role: "system", Content: "Never touch files outside the project."},
		{Role: "user", Content: "Refactor the auth module and run the tests."},
		{Role: "user", Content: "keep going"},
	}}
	task := buildTaskLedger(body)
	if task.OriginalGoal != "Refactor the auth module and run the tests." {
		t.Fatalf("goal=%q", task.OriginalGoal)
	}
	found := false
	for _, c := range task.Constraints {
		if strings.Contains(c, "Never touch files") {
			found = true
		}
	}
	if !found {
		t.Fatalf("constraints missing: %#v", task.Constraints)
	}
}

func TestTaskLedgerContextAnchorsGoal(t *testing.T) {
	task := &taskLedger{
		OriginalGoal:   "Deploy the service",
		Constraints:    []string{"no downtime"},
		Executed:       []string{"build binary", "upload artifact"},
		AccountID:      "acc-2",
		ConversationID: "conv-9",
	}
	ctx := task.Context()
	if !strings.Contains(ctx, "ORIGINAL_GOAL: Deploy the service") {
		t.Fatalf("goal missing: %s", ctx)
	}
	if !strings.Contains(ctx, "never restart the task") && !strings.Contains(strings.ToLower(ctx), "never restart") {
		t.Fatalf("task rule missing: %s", ctx)
	}
	if !strings.Contains(ctx, "acc-2") || !strings.Contains(ctx, "conv-9") {
		t.Fatalf("account/conversation missing: %s", ctx)
	}
	if withTaskLedger("current", nil) != "current" {
		t.Fatal("nil task must not alter prompt")
	}
	with := withTaskLedger("current", task)
	if !strings.HasPrefix(with, "[TASK_LEDGER]") || !strings.Contains(with, "current") {
		t.Fatalf("withTaskLedger: %s", with)
	}
}

func TestTaskLedgerMergeEvidenceIsIdempotent(t *testing.T) {
	task := buildTaskLedger(&oaiReq{Messages: []oaiMsg{{Role: "user", Content: "goal"}}})
	l := agentLedger{Completed: []toolEvidence{
		{ID: "call_1", Name: "exec", Arguments: `{"input":"go test ./..."}`, Result: "ok"},
	}}
	task.mergeEvidence(l)
	task.mergeEvidence(l) // duplicate call id
	if len(task.ToolResults) != 1 {
		t.Fatalf("duplicate evidence not deduped: %#v", task.ToolResults)
	}
	if len(task.Executed) != 1 {
		t.Fatalf("duplicate step not deduped: %#v", task.Executed)
	}
	// Same name+args but different call id (idempotency by signature).
	task.mergeEvidence(agentLedger{Completed: []toolEvidence{{ID: "call_2", Name: "exec", Arguments: `{"input":"go test ./..."}`, Result: "ok"}}})
	if len(task.ToolResults) != 1 {
		t.Fatalf("signature dedupe failed: %#v", task.ToolResults)
	}
}

func TestTaskLedgerRecordsFailuresAndSwitches(t *testing.T) {
	task := &taskLedger{}
	task.recordFailure(errForTest("boom"))
	task.recordSwitch("acc-1", "acc-2")
	if len(task.Failures) != 1 || len(task.Switches) != 1 {
		t.Fatalf("failure/switch not recorded: %#v", task)
	}
	task.recordSwitch("acc-1", "acc-1")
	if len(task.Switches) != 1 {
		t.Fatalf("no-op switch recorded: %#v", task.Switches)
	}
}

type testErr string

func errForTest(s string) error { return testErr(s) }
func (e testErr) Error() string { return string(e) }

func TestTaskLedgerPersistsThroughSessionResolver(t *testing.T) {
	t.Setenv("M365_SESSION_CACHE", t.TempDir()+"/sessions.json")
	t.Setenv("M365_CONVERSATION_CACHE", t.TempDir()+"/conversations.json")
	t.Setenv("M365_USER_SESSION_CACHE", t.TempDir()+"/users.json")
	sr := openSessionResolver()
	task := &taskLedger{OriginalGoal: "ship the fix"}
	body := &oaiReq{Messages: []oaiMsg{{Role: "user", Content: "ship the fix"}}}
	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	sr.BindWithTask("sess-1", "conv-1", "acc-1", body, "", r, task)

	got, ok := sr.GetSession("sess-1")
	if !ok || got.Task == nil || got.Task.OriginalGoal != "ship the fix" {
		t.Fatalf("task not persisted: %#v ok=%t", got, ok)
	}
	// A later bind without a task keeps the existing ledger (goal preserved).
	body2 := &oaiReq{Messages: []oaiMsg{{Role: "user", Content: "continue"}}}
	r2 := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	sr.BindWithTask("sess-1", "conv-1", "acc-1", body2, "done", r2, nil)
	got2, _ := sr.GetSession("sess-1")
	if got2.Task == nil || got2.Task.OriginalGoal != "ship the fix" {
		t.Fatalf("existing task lost: %#v", got2.Task)
	}
	// SetTask replaces the ledger for the session.
	sr.SetTask("sess-1", &taskLedger{OriginalGoal: "new goal"})
	got3, _ := sr.GetSession("sess-1")
	if got3.Task.OriginalGoal != "new goal" {
		t.Fatalf("SetTask failed: %#v", got3.Task)
	}
}

func TestTaskLedgerCompleteLifecycle(t *testing.T) {
	task := buildTaskLedger(&oaiReq{Messages: []oaiMsg{
		{Role: "user", Content: "Refactor and verify the auth module"},
	}})
	if task.IsComplete() {
		t.Fatal("fresh ledger must not be complete")
	}
	if ctx := task.Context(); !strings.Contains(ctx, "GOAL_STATUS: active") {
		t.Fatalf("active status missing: %s", ctx)
	}
	task.markComplete("all tests green")
	if !task.IsComplete() {
		t.Fatal("markComplete must set complete")
	}
	if task.CompletedAt == nil || task.CompletedReason != "all tests green" {
		t.Fatalf("completion metadata missing: %#v", task)
	}
	ctx := task.Context()
	if !strings.Contains(ctx, "GOAL_STATUS: complete") {
		t.Fatalf("complete status missing: %s", ctx)
	}
	if !strings.Contains(ctx, "TASK_COMPLETE_RULE") {
		t.Fatalf("completion rule missing: %s", ctx)
	}
	if strings.Contains(strings.ToLower(ctx), "continue the original goal") {
		t.Fatalf("complete ledger must not ask to continue: %s", ctx)
	}
}

func TestTaskLedgerGoalToolEvidenceClosesGoal(t *testing.T) {
	task := buildTaskLedger(&oaiReq{Messages: []oaiMsg{{Role: "user", Content: "goal"}}})
	// create_goal registers the goal id.
	task.mergeEvidence(agentLedger{Completed: []toolEvidence{
		{ID: "call_1", Name: "create_goal", Arguments: `{"objective":"train the model"}`, Result: `{"goal_id":"goal-abc123","status":"active"}`},
	}})
	if task.GoalID != "goal-abc123" {
		t.Fatalf("create_goal did not record goal id: %#v", task)
	}
	if task.IsComplete() {
		t.Fatal("create_goal must not complete")
	}
	// update_goal(action=complete) closes it.
	task.mergeEvidence(agentLedger{Completed: []toolEvidence{
		{ID: "call_2", Name: "update_goal", Arguments: `{"action":"complete","goal_id":"goal-abc123"}`, Result: "marked complete"},
	}})
	if !task.IsComplete() {
		t.Fatalf("update_goal(complete) must close goal: %#v", task)
	}
	// A later round must not silently reopen a closed goal.
	if suffix := task.goalRoundInjectedContext(); !strings.Contains(suffix, "GOAL_STATUS: complete") {
		t.Fatalf("complete goal round context missing: %s", suffix)
	}
	if ctx := task.Context(); strings.Contains(strings.ToLower(ctx), "continue the original goal") {
		t.Fatalf("closed goal must not inject continue rule: %s", ctx)
	}
}

func TestGoalCompletionSignal(t *testing.T) {
	withEvidence := agentLedger{Completed: []toolEvidence{
		{ID: "c1", Name: "exec", Arguments: `{}`, Result: "ok"},
	}}
	// Process-record-only report closes the goal.
	report := "唯一未完成的是流程记录：当前会话没有可用的目标状态读取或更新入口，因此无法将仍显示为 active 的目标 goal-7d6b6b23-ff34-49ca-95ca-6f54246d9d66 正式标记为 complete；功能实现与验证本身已经全部完成。"
	if !goalCompletionSignal(report, withEvidence) {
		t.Fatal("process-record-only report must count as completion")
	}
	// Plain success closes the goal.
	if !goalCompletionSignal("已完成全部功能并验证通过", withEvidence) {
		t.Fatal("explicit completion must count")
	}
	if !goalCompletionSignal("Deployment completed successfully", withEvidence) {
		t.Fatal("english completion must count")
	}
	if !goalCompletionSignal("目标已达成：所有任务已完成", withEvidence) {
		t.Fatal("holistic goal completion must count")
	}
	// Explicit denial must not close.
	denied := agentLedger{Completed: []toolEvidence{{ID: "c2", Name: "exec", Arguments: `{}`, Result: "exit code 1\nerror: build failed"}}}
	if goalCompletionSignal("尚未完成，构建失败", denied) {
		t.Fatal("denial must not close the goal")
	}
	if goalCompletionSignal("功能未完成，需要继续修复", withEvidence) {
		t.Fatal("mid-task denial must not close the goal")
	}
	// No evidence must not close.
	if goalCompletionSignal("all done", agentLedger{}) {
		t.Fatal("no tool evidence must not close")
	}
	// Pending tool results must not close.
	pending := agentLedger{Completed: []toolEvidence{{ID: "c3", Name: "exec", Arguments: `{}`, Result: "ok"}}, Pending: []toolEvidence{{ID: "c4", Name: "exec", Arguments: `{}`}}}
	if goalCompletionSignal("done", pending) {
		t.Fatal("pending tool results must not close")
	}
}

// TestGoalCompletionSignalNoMisjudgment guards the state-correction detector
// against false positives: mid-task progress reports, single-step completions,
// and continuation phrasing must never auto-close a still-active goal.
func TestGoalCompletionSignalNoMisjudgment(t *testing.T) {
	ev := agentLedger{Completed: []toolEvidence{{ID: "c1", Name: "exec", Arguments: `{}`, Result: "ok"}}}
	cases := []string{
		"第一步完成了，接下来继续第二步",
		"完成信息收集，现在开始实现",
		"我完成了模块A的代码",
		"Deployment completed successfully for stage 1, continuing to stage 2",
		"Step 1 done, next step is testing",
		"已实现但尚未验证",
		"功能尚未完成，还需要调试",
		"只完成了部分工作",
		"唯一未完成的是第一个模块", // 不是流程记录，不能误关
		"我还在继续实现剩余步骤",
		"not yet done",
		"still working on it",
	}
	for _, c := range cases {
		if goalCompletionSignal(c, ev) {
			t.Errorf("mid-task answer must not close the goal: %q", c)
		}
	}
}

func TestGoalRoundContextOnComplete(t *testing.T) {
	task := &taskLedger{OriginalGoal: "x"}
	task.markComplete("done")
	msgs := []oaiMsg{{Role: "user", Content: "<goal_round>\nObjective: x\nRound: 3/256"}}
	// A complete goal with a declared goal tool must be detected as a goal round.
	tools := []chathub.Tool{{Type: "function", Function: json.RawMessage(`{"name":"update_goal","parameters":{"type":"object","properties":{}}}`)}}
	if !goalRoundRequest(msgs, task, tools) {
		t.Fatal("goal round not detected with declared goal tool")
	}
	// A complete goal with a GoalID but no declared tool must also be detected.
	task2 := &taskLedger{OriginalGoal: "x", GoalID: "goal-abc"}
	if !goalRoundRequest(msgs, task2, nil) {
		t.Fatal("goal round not detected with GoalID")
	}
	// A message that merely mentions <goal_round> without the Round: N/M
	// counter must NOT be treated as a goal round, even with a GoalID.
	noCounter := []oaiMsg{{Role: "user", Content: "请解释什么是 <goal_round> 标签"}}
	if goalRoundRequest(noCounter, task2, tools) {
		t.Fatal("bare <goal_round> without Round: N/M must not trigger the goal flow")
	}
	// The round-structure signal (Round: N/M) is the discriminator: even with
	// no GoalID and no declared tool, the protocol-injected structure counts.
	structured := []oaiMsg{{Role: "user", Content: "<goal_round>\nObjective: x\nRound: 5/256"}}
	if !goalRoundRequest(structured, &taskLedger{OriginalGoal: "x"}, nil) {
		t.Fatal("Round: N/M counter must be detected as a goal round even without tool/GoalID")
	}
	suffix := task.goalRoundInjectedContext()
	if suffix == "" || !strings.Contains(suffix, "no further update_goal call is required") {
		t.Fatalf("completion context missing: %s", suffix)
	}
	incomplete := &taskLedger{OriginalGoal: "x"}
	if incomplete.goalRoundInjectedContext() != "" {
		t.Fatal("active goal must not inject completion context")
	}
}

func TestGoalRoundStructuredSignals(t *testing.T) {
	// Layer 1: GoalID + round structure.
	taskWithID := &taskLedger{GoalID: "goal-1"}
	msgs := []oaiMsg{{Role: "user", Content: "<goal_round>\nObjective: x\nRound: 4/256"}}
	if !goalRoundRequest(msgs, taskWithID, nil) {
		t.Fatal("GoalID + round structure must be detected")
	}
	// Layer 2: declared goal tool + round structure.
	tools := []chathub.Tool{{Type: "function", Function: json.RawMessage(`{"name":"get_goal"}`)}}
	if !goalRoundRequest(msgs, &taskLedger{}, tools) {
		t.Fatal("declared tool + round structure must be detected")
	}
	// Layer 3: round structure alone (no GoalID, no tool).
	noID := &taskLedger{}
	if !goalRoundRequest(msgs, noID, nil) {
		t.Fatal("Round: N/M counter alone must be detected as a goal round")
	}
	// A tag with no counter is NOT a round, regardless of GoalID or tools.
	loose := []oaiMsg{{Role: "user", Content: "见 <goal_round> 文档"}}
	if goalRoundRequest(loose, taskWithID, tools) {
		t.Fatal("tag without counter must not be a goal round even with GoalID+tools")
	}
	// Round counter parser.
	cur, max, ok := goalRoundCounter(msgs)
	if !ok || cur != 4 || max != 256 {
		t.Fatalf("goalRoundCounter got %d %d %t, want 4 256 true", cur, max, ok)
	}
	if _, _, ok := goalRoundCounter([]oaiMsg{{Role: "user", Content: "no counter here"}}); ok {
		t.Fatal("goalRoundCounter must not match without Round:")
	}
}
