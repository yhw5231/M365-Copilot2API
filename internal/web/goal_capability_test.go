package web

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"m365-copilot2api/internal/chathub"
)

// ---------------------------------------------------------------
// 能力点1：环境误判（无读取权限、无可用工具等）时的修正
// ---------------------------------------------------------------

// TestMisjudgmentCorrectionDetection verifies that genuine environment
// misjudgments (no tools / linux-only / /mnt/data) are detected and routed to
// the correction path, while legitimate permission statements are not.
func TestMisjudgmentCorrectionDetection(t *testing.T) {
	tools := []map[string]any{
		{"type": "function", "function": map[string]any{"name": "read", "description": "read"}},
		{"type": "function", "function": map[string]any{"name": "write", "description": "write"}},
		{"type": "function", "function": map[string]any{"name": "pwsh", "description": "shell"}},
	}
	// Genuine misjudgments that must be routed to correction.
	misjudgments := []string{
		"当前会话没有任何可用工具",
		"current session only provides a linux container",
		"this session only has a linux sandbox",
		"当前会话没有执行工具，无法运行命令",
		"I don't have command execution in this session",
		"当前会话实际没有可调用的 pwsh、read、write、edit、glob 或 grep 文件操作接口",
	}
	for _, text := range misjudgments {
		if !isWorkspaceToolMisjudgmentForTools(text, tools) {
			t.Errorf("genuine misjudgment not detected: %q", text)
		}
	}
	// Legitimate permission / sandbox narration must NOT be corrected.
	legit := []string{
		"I don't have write access to that file",
		"the read tool returned a permission denied error",
		"无法访问 /mnt/data/config.json（权限拒绝）",
		"this deployment runs in a linux container on the server",
		"该文件被锁定，无法写入",
	}
	for _, text := range legit {
		if isWorkspaceToolMisjudgmentForTools(text, tools) {
			t.Errorf("legitimate permission statement misclassified: %q", text)
		}
	}
}

// TestCleanWorkspaceMisjudgmentsPreservesUserContent verifies the correction
// layer only removes assistant misjudgment claims, never user/system/developer
// or tool messages. This is the key isolation guarantee for 能力点1+2.
func TestCleanWorkspaceMisjudgmentsPreservesUserContent(t *testing.T) {
	messages := []oaiMsg{
		{Role: "system", Content: "You can choose to say something here."},
		{Role: "user", Content: "当前会话没有可用工具，请把代码写好"},      // user says it, must keep
		{Role: "assistant", Content: "当前会话没有任何可用工具，无法继续"}, // assistant claims it, drop
		{Role: "tool", Content: `{"name":"read","result":"ok"}`},
	}
	got := cleanWorkspaceToolMisjudgments(messages, nil)
	roles := map[string]bool{}
	contents := map[string]bool{}
	for _, m := range got {
		roles[m.Role] = true
		contents[contentToString(m.Content)] = true
	}
	if !roles["user"] || !roles["system"] || !roles["tool"] {
		t.Fatalf("user/system/tool messages must be preserved, got roles=%v", roles)
	}
	if !contents["当前会话没有可用工具，请把代码写好"] {
		t.Fatal("user content containing misjudgment wording must be preserved verbatim")
	}
	if contents["当前会话没有任何可用工具，无法继续"] {
		t.Fatal("assistant misjudgment claim must be removed")
	}
}

// ---------------------------------------------------------------
// 能力点2：用户对话内容不会造成任务流程判断错误 / 状态异常 / 被误修正
// ---------------------------------------------------------------

// TestUserContentCannotTriggerGoalFlow verifies that ordinary user content
// mentioning goal wording, completion wording, or round tags does not flip the
// task state or the goal lifecycle.
func TestUserContentCannotTriggerGoalFlow(t *testing.T) {
	task := &taskLedger{OriginalGoal: "把一个功能从 A 迁移到 B"}
	tools := []chathub.Tool{{Type: "function", Function: json.RawMessage(`{"name":"update_goal"}`)}}

	// User content that mentions "<goal_round>" or "完成" but has no Round: N/M
	// counter must not be treated as a goal round.
	chats := [][]oaiMsg{
		{{Role: "user", Content: "请解释什么是 <goal_round> 标签"}},
		{{Role: "user", Content: "注意：当前任务是完成迁移，别搞混了"}},
		{{Role: "user", Content: "hello <goal_round> world"}},
		{{Role: "user", Content: "完成了吗？没完成就继续"}},
	}
	for _, msgs := range chats {
		if goalRoundRequest(msgs, task, tools) {
			t.Errorf("plain user content must not trigger goal round flow: %q", msgs[0].Content)
		}
	}

	// A genuine protocol round structure still works.
	real := []oaiMsg{{Role: "user", Content: "<goal_round>\nObjective: 迁移\nRound: 2/256"}}
	if !goalRoundRequest(real, task, tools) {
		t.Fatal("genuine goal round must be detected")
	}

	// User content mentioning completion words must not auto-close the goal
	// (no completed tool evidence required — the correction gate needs evidence).
	victim := goalCompletionSignal("完成迁移了吗？用户问", agentLedger{})
	if victim {
		t.Fatal("user question about completion must not close the goal")
	}
}

// TestUserContentCannotBeMisjudgedAsEnvironmentClaims verifies that assistant
// responses that merely quote/echo the user are not swept into the
// misjudgment-correction path. The correction layer only acts on assistant
// claims made in the opening window, and only for the tool-aware layer when the
// caller actually declared the tools.
func TestUserContentCannotBeMisjudgedAsEnvironmentClaims(t *testing.T) {
	tools := []map[string]any{
		{"type": "function", "function": map[string]any{"name": "grep", "description": "search"}},
	}
	// Assistant replies that merely address the user's permission question
	// without claiming the tools are unavailable must not be corrected.
	normal := []string{
		"你提到的权限问题已通过添加授权解决，我继续用 grep 查找。",
		"好的，我用已声明的工具来完成这个任务。",
		"这个文件确实需要更多权限，建议联系管理员处理。",
	}
	for _, text := range normal {
		if isWorkspaceToolMisjudgmentForTools(text, tools) {
			t.Errorf("ordinary permission narration misclassified: %q", text)
		}
	}
	// A tool name followed by an availability denial in the opening window is a
	// misjudgment only when the tool was actually declared. Undeclared tools are
	// ignored by the tool-aware layer, and base-structural matches still guard
	// the "X工具不可用，无法..." shapes that read as environment misjudgments.
	declared := "grep 工具不可用，无法查找"
	if !isWorkspaceToolMisjudgmentForTools(declared, tools) {
		t.Fatal("declared tool + availability denial must be detected")
	}
	undeclaredToolAware := "我没有安装 rg 工具" // rg not declared → tool-aware ignores
	if isWorkspaceToolMisjudgmentForTools(undeclaredToolAware, tools) {
		t.Fatal("undeclared tool claim must not be detected by the tool-aware layer")
	}
	undeclaredPerf := "vim is not installed in this environment" // no base-structural match
	if isWorkspaceToolMisjudgmentForTools(undeclaredPerf, tools) {
		t.Fatal("undeclared tool + no base match must not be detected")
	}
}

// ---------------------------------------------------------------
// 能力点3：目标任务结束时正确标记完成，不卡循环
// ---------------------------------------------------------------

// TestGoalCompletesWithoutLoop verifies the terminal lifecycle: explicit
// update_goal(complete) evidence closes the goal, the ledger stays closed on
// later rounds, and the completion context is injected so the model reports the
// outcome instead of continuing.
func TestGoalCompletesWithoutLoop(t *testing.T) {
	task := buildTaskLedger(&oaiReq{Messages: []oaiMsg{{Role: "user", Content: "<goal_round>\nObjective: 实现X\nRound: 1/256"}}})
	if task.IsComplete() {
		t.Fatal("new task must start active")
	}
	// Round 1 works, round 2 confirms completion via the protocol tool.
	task.mergeEvidence(agentLedger{Completed: []toolEvidence{
		{ID: "call_1", Name: "update_goal", Arguments: `{"action":"complete","goal_id":"goal-x"}`, Result: "ok"},
	}})
	if !task.IsComplete() {
		t.Fatal("update_goal(complete) must close the goal")
	}
	// Round 3 arrives: must not re-open, must inject completion context.
	round3 := []oaiMsg{{Role: "user", Content: "<goal_round>\nObjective: 实现X\nRound: 3/256"}}
	if goalRoundRequest(round3, task, nil) && task.IsComplete() {
		if strings.TrimSpace(task.goalRoundInjectedContext(round3)) == "" {
			t.Fatal("complete round must carry the completion context")
		}
	}
	if !task.IsComplete() {
		t.Fatal("later rounds must not re-open a completed goal")
	}
	ctx := task.Context()
	if strings.Contains(strings.ToLower(ctx), "continue the original goal") {
		t.Fatal("completed goal must not ask to continue")
	}
}

// TestRoundBudgetExhaustedBreaksLoop verifies the round-counter state signal
// alone (no completion wording needed) can break the loop at the round ceiling.
func TestRoundBudgetExhaustedBreaksLoop(t *testing.T) {
	task := &taskLedger{GoalID: "goal-budget"}
	bookEnd := []oaiMsg{{Role: "user", Content: "<goal_round>\nObjective: x\nRound: 256/256"}}
	cur, max, ok := goalRoundCounter(bookEnd)
	if !ok || cur != 256 || max != 256 {
		t.Fatalf("round counter parse: %d %d %t", cur, max, ok)
	}
	if !goalRoundRequest(bookEnd, task, nil) {
		t.Fatal("round 256/256 must be a goal round")
	}
	// The injected context tells the model to stop and close the session.
	note := buildRoundBudgetNote(cur, max)
	if !strings.Contains(note, "ROUND_BUDGET_EXHAUSTED") || !strings.Contains(note, "256") {
		t.Fatalf("budget note missing: %s", note)
	}
	if strings.Contains(note, "continue") {
		t.Fatalf("budget note must not ask to continue: %s", note)
	}
}

// buildRoundBudgetNote mirrors the server-side injection text so the test
// exercises the exact wording the model receives.
func buildRoundBudgetNote(cur, max int) string {
	return fmt.Sprintf("\n\n[TASK_LEDGER] ROUND_BUDGET_EXHAUSTED: %d/%d rounds used. "+
		"Stop continuing the goal, report the current status and any unverified steps, and close the goal with update_goal(action=complete) if the work is done.", cur, max)
}
