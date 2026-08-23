package web

import (
	"net/http/httptest"
	"strings"
	"testing"
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
