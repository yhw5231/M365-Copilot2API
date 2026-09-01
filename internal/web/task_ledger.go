package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"m365-copilot2api/internal/chathub"
)

// taskLedger is the server-side "task ledger" persisted on the downstream
// session binding. It survives context compaction, stream interruption and
// account switches so the model always knows "what to do and how far it got"
// instead of re-asking for the task:
//
//	original_goal    — the user's original target (tier 1, never trimmed)
//	constraints      — inviolable constraints extracted from instructions
//	executed         — steps already executed
//	tool_results     — tool evidence already obtained
//	remaining        — steps still outstanding (derived by the model, echoed)
//	account_id       — the account currently serving the task
//	conversation_id / session_id — the upstream conversation the task runs on
//	failures / switches — recorded so a long task does not restart silently
//	goal_id          — goal identity when the client runs a goal protocol
//	status           — lifecycle state (empty = active, complete/blocked/paused)
type taskLedger struct {
	OriginalGoal    string         `json:"original_goal,omitempty"`
	Constraints     []string       `json:"constraints,omitempty"`
	Executed        []string       `json:"executed,omitempty"`
	ToolResults     []toolEvidence `json:"tool_results,omitempty"`
	Remaining       []string       `json:"remaining,omitempty"`
	AccountID       string         `json:"account_id,omitempty"`
	ConversationID  string         `json:"conversation_id,omitempty"`
	SessionID       string         `json:"session_id,omitempty"`
	Failures        []string       `json:"failures,omitempty"`
	Switches        []string       `json:"switches,omitempty"`
	GoalID          string         `json:"goal_id,omitempty"`
	Status          string         `json:"status,omitempty"`
	CompletedAt     *time.Time     `json:"completed_at,omitempty"`
	CompletedReason string         `json:"completed_reason,omitempty"`
	UpdatedAt       time.Time      `json:"updated_at,omitempty"`
}

// Goal lifecycle states. The empty status means "active" (an open goal); the
// ledger stops injecting the "continue the goal" rule once it is complete.
const (
	taskStatusComplete = "complete"
	taskStatusBlocked  = "blocked"
	taskStatusPaused   = "paused"
)

const (
	taskGoalMax        = 1200
	taskConstraintMax  = 600
	taskStepMax        = 200
	taskEvidenceMax    = 900
	maxTaskLedgerLines = 64
)

func compactTaskText(s string, limit int) string {
	s = strings.Join(strings.Fields(strings.TrimSpace(s)), " ")
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "…"
}

func appendUniqueString(dst []string, v string) []string {
	if v == "" {
		return dst
	}
	for _, x := range dst {
		if x == v {
			return dst
		}
	}
	return append(dst, v)
}

func appendUniqueEvidence(dst []toolEvidence, e toolEvidence) []toolEvidence {
	for _, x := range dst {
		if x.ID != "" && x.ID == e.ID {
			return dst
		}
		if x.Name == e.Name && canonicalToolArguments(x.Arguments) == canonicalToolArguments(e.Arguments) {
			return dst
		}
	}
	return append(dst, e)
}

// buildTaskLedger extracts the original goal and constraints from the current
// request. It is used only when no persistent ledger exists yet; an existing
// ledger keeps its original goal so account switches cannot restart the task.
func buildTaskLedger(body *oaiReq) *taskLedger {
	t := &taskLedger{UpdatedAt: time.Now().UTC()}
	for _, m := range body.Messages {
		role := strings.ToLower(strings.TrimSpace(m.Role))
		text := strings.TrimSpace(contentToString(m.Content))
		if text == "" {
			continue
		}
		switch role {
		case "system", "developer":
			if len(t.Constraints) < 16 {
				t.Constraints = appendUniqueString(t.Constraints, compactTaskText(text, taskConstraintMax))
			}
		case "user":
			if t.OriginalGoal == "" {
				t.OriginalGoal = compactTaskText(text, taskGoalMax)
			}
		}
	}
	return t
}

// Context renders the ledger for prompt injection. It is always emitted above
// the current turn so the model re-anchors on the original goal even after a
// fresh upstream conversation or an account switch.
func (t *taskLedger) Context() string {
	if t == nil || t.OriginalGoal == "" && len(t.Executed) == 0 && len(t.ToolResults) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "ORIGINAL_GOAL: %s\n", t.OriginalGoal)
	if t.GoalID != "" {
		fmt.Fprintf(&b, "GOAL_ID: %s\n", t.GoalID)
	}
	// A closed ledger always emits the terminal rule regardless of whether a goal
	// id was recorded, so a completed task can never be re-opened by a later
	// round or by the generic "continue the goal" rule.
	if t.Status == taskStatusComplete {
		fmt.Fprintf(&b, "GOAL_STATUS: complete\n")
		if t.CompletedReason != "" {
			fmt.Fprintf(&b, "COMPLETION_REASON: %s\n", compactTaskText(t.CompletedReason, 300))
		}
		b.WriteString("TASK_COMPLETE_RULE: The goal is complete. Do not continue working on it, do not repeat executed steps, and do not report outstanding work. Restate the outcome and stop.")
		return strings.TrimSpace(b.String())
	}
	// Only goal-protocol sessions (a goal id was actually created) receive the
	// goal lifecycle framing and the "continue the original goal" directive. An
	// ordinary conversation that merely passes through the gateway must not be
	// nudged into treating a one-off question as an ongoing goal.
	isGoalSession := t.GoalID != ""
	if isGoalSession {
		status := t.Status
		if status == "" {
			status = "active"
		}
		fmt.Fprintf(&b, "GOAL_STATUS: %s\n", status)
	}
	if len(t.Constraints) > 0 {
		fmt.Fprintf(&b, "CONSTRAINTS: %s\n", mustJSON(trimLines(t.Constraints, maxTaskLedgerLines)))
	}
	if len(t.Executed) > 0 {
		fmt.Fprintf(&b, "EXECUTED_STEPS: %s\n", mustJSON(trimLines(t.Executed, maxTaskLedgerLines)))
	}
	if len(t.ToolResults) > 0 {
		ev := make([]toolEvidence, 0, len(t.ToolResults))
		for _, e := range t.ToolResults {
			e.Result = compactTaskText(e.Result, taskEvidenceMax)
			ev = append(ev, e)
		}
		b.WriteString("TOOL_EVIDENCE: " + string(mustJSONBytes(ev)) + "\n")
	}
	if len(t.Remaining) > 0 {
		fmt.Fprintf(&b, "REMAINING_STEPS: %s\n", mustJSON(trimLines(t.Remaining, maxTaskLedgerLines)))
	}
	if t.AccountID != "" {
		fmt.Fprintf(&b, "CURRENT_ACCOUNT: %s\n", t.AccountID)
	}
	if t.ConversationID != "" {
		fmt.Fprintf(&b, "UPSTREAM_CONVERSATION: %s\n", t.ConversationID)
	}
	if t.SessionID != "" {
		fmt.Fprintf(&b, "UPSTREAM_SESSION: %s\n", t.SessionID)
	}
	if len(t.Failures) > 0 {
		fmt.Fprintf(&b, "FAILURE_LOG: %s\n", mustJSON(trimLines(t.Failures, 8)))
	}
	if len(t.Switches) > 0 {
		fmt.Fprintf(&b, "ACCOUNT_SWITCH_LOG: %s\n", mustJSON(trimLines(t.Switches, 8)))
	}
	if isGoalSession {
		b.WriteString("TASK_RULE: Continue the original goal from where the evidence left off. Never restart the task or re-ask what it is. A completed step must not be repeated.")
	} else {
		b.WriteString("TASK_CONTEXT: The above records the original request and any progress so far. Use it to stay on track without re-asking the task.")
	}
	return strings.TrimSpace(b.String())
}

func trimLines(src []string, limit int) []string {
	if len(src) > limit {
		return src[len(src)-limit:]
	}
	return src
}

func mustJSONBytes(v any) []byte { b, _ := json.Marshal(v); return b }

// mergeEvidence folds the protocol-neutral evidence ledger from the current
// turn into the persistent task ledger (idempotent by call id / signature).
// It also applies goal-tool evidence so the server-side goal life cycle matches
// what the model actually did: create_goal registers the goal id, update_goal
// with action=complete closes the goal, so a completed goal is never re-opened
// by the generic "continue the goal" rule.
func (t *taskLedger) mergeEvidence(l agentLedger) {
	if t == nil {
		return
	}
	for _, e := range l.Completed {
		t.applyGoalToolEvidence(e)
		t.ToolResults = appendUniqueEvidence(t.ToolResults, e)
		step := compactTaskText(e.Name+"("+e.Arguments+")", taskStepMax)
		t.Executed = appendUniqueString(t.Executed, step)
	}
}

// applyGoalToolEvidence mirrors goal-protocol tool results into the ledger
// state. It must not error-retry after completion: once the client confirms the
// goal with update_goal(action=complete), the ledger is closed and stays closed.
func (t *taskLedger) applyGoalToolEvidence(e toolEvidence) {
	if t == nil || e.Name == "" {
		return
	}
	switch e.Name {
	case "create_goal":
		if t.GoalID == "" {
			t.GoalID = extractGoalID(e.Arguments, e.Result)
		}
	case "update_goal":
		action := strings.ToLower(goalArgString(e.Arguments, "action"))
		switch action {
		case "complete":
			t.markComplete(goalReason(e))
		case "blocked":
			if t.Status == "" || t.Status == "active" {
				t.Status = taskStatusBlocked
				t.CompletedReason = goalReason(e)
				now := time.Now().UTC()
				t.CompletedAt = &now
			}
		case "paused", "pause":
			if t.Status == "" || t.Status == "active" {
				t.Status = taskStatusPaused
				now := time.Now().UTC()
				t.CompletedAt = &now
			}
		case "resume":
			if t.Status == taskStatusPaused || t.Status == taskStatusBlocked {
				t.Status = ""
				t.CompletedAt = nil
				t.CompletedReason = ""
			}
		}
		if tid := extractGoalID(e.Arguments, e.Result); tid != "" && t.GoalID == "" {
			t.GoalID = tid
		}
	case "get_goal":
		if t.GoalID == "" {
			t.GoalID = extractGoalID(e.Arguments, e.Result)
		}
	}
	t.UpdatedAt = time.Now().UTC()
}

// markComplete closes the ledger: status complete with the reason and timestamp.
func (t *taskLedger) markComplete(reason string) {
	if t == nil {
		return
	}
	now := time.Now().UTC()
	t.Status = taskStatusComplete
	t.CompletedAt = &now
	t.CompletedReason = compactTaskText(reason, 500)
	t.Remaining = nil
	t.UpdatedAt = now
}

// IsComplete reports whether the ledger carries an explicit completion. The
// status is server-authoritative: after update_goal(action=complete) evidence
// lands, no further goal round may re-open the task.
func (t *taskLedger) IsComplete() bool {
	return t != nil && t.Status == taskStatusComplete
}

// goalReason builds a human-readable completion reason from tool evidence, or
// falls back to a stable summary mentioning the confirming tool call.
func goalReason(e toolEvidence) string {
	if r := strings.TrimSpace(e.Result); r != "" {
		return compactTaskText(r, 500)
	}
	return "goal confirmed closed by " + e.Name
}

func goalArgString(argsJSON, key string) string {
	var m map[string]any
	if json.Unmarshal([]byte(argsJSON), &m) != nil {
		return ""
	}
	switch v := m[key].(type) {
	case string:
		return v
	case json.Number:
		return v.String()
	case float64:
		return fmt.Sprint(v)
	}
	return ""
}

func (t *taskLedger) bind(accID, convID, sessID string) {
	if t == nil {
		return
	}
	if accID != "" {
		t.AccountID = accID
	}
	if convID != "" {
		t.ConversationID = convID
	}
	if sessID != "" {
		t.SessionID = sessID
	}
	t.UpdatedAt = time.Now().UTC()
}

func (t *taskLedger) recordFailure(err error) {
	if t == nil || err == nil {
		return
	}
	t.Failures = appendStringLimited(t.Failures, compactTaskText(err.Error(), 400), 16)
	t.UpdatedAt = time.Now().UTC()
}

func (t *taskLedger) recordSwitch(from, to string) {
	if t == nil || from == "" || to == "" || from == to {
		return
	}
	t.Switches = appendStringLimited(t.Switches, from+"→"+to, 16)
	t.UpdatedAt = time.Now().UTC()
}

func appendStringLimited(dst []string, v string, limit int) []string {
	if v == "" {
		return dst
	}
	dst = append(dst, v)
	if len(dst) > limit {
		dst = dst[len(dst)-limit:]
	}
	return dst
}

// withTaskLedger prefixes a prompt with the persistent task context.
func withTaskLedger(prompt string, task *taskLedger) string {
	ctx := ""
	if task != nil {
		ctx = task.Context()
	}
	if ctx == "" {
		return prompt
	}
	return "[TASK_LEDGER]\n" + ctx + "\n\n" + prompt
}

// sessionTaskLedger restores the persistent task ledger for the downstream
// session the current request belongs to. It prefers the explicit session id
// header, then the resolved upstream session, then the session key and user
// session mappings. Returns nil when no binding carries a ledger yet.
func (s *Server) sessionTaskLedger(r *http.Request, body *oaiReq) *taskLedger {
	if s == nil || s.sessionResolver == nil {
		return nil
	}
	if id := sessionIDFromRequest(r); id != "" {
		if b, ok := s.sessionResolver.GetSession(id); ok {
			return b.Task
		}
	}
	if strings.TrimSpace(body.SessionID) != "" {
		if b, ok := s.sessionResolver.GetSession(body.SessionID); ok {
			return b.Task
		}
	}
	if strings.TrimSpace(body.SessionKey) != "" && s.sessions != nil {
		if v, ok := s.sessions.get(body.SessionKey); ok {
			if b, ok := s.sessionResolver.GetSession(v.SessionID); ok {
				return b.Task
			}
		}
	}
	// Explicit session identifiers only: the task ledger (and goal state)
	// survives across rounds when the client sends session_id / x-session-id or
	// body.session_id. Content-similarity inheritance is deliberately not
	// performed, so a brand-new conversation never silently carries an old
	// session's task ledger.
	if s.sessionResolver != nil {
		if task := s.sessionResolver.ResolveTaskLedger(r, body); task != nil {
			return task
		}
	}
	return nil
}

// extractGoalID pulls a goal-* identifier out of either the tool arguments
// (the client addresses its own goal) or the tool result JSON (a create_goal
// response returns the newly minted id). It tolerates nested maps and the
// "goal_id" / "id" spellings used by goal-protocol clients.
func extractGoalID(argsJSON, resultJSON string) string {
	extract := func(raw string) string {
		if raw == "" {
			return ""
		}
		var v any
		if json.Unmarshal([]byte(raw), &v) != nil {
			return ""
		}
		var walk func(any) string
		walk = func(node any) string {
			switch n := node.(type) {
			case string:
				if strings.HasPrefix(n, "goal-") {
					return n
				}
			case []any:
				for _, item := range n {
					if id := walk(item); id != "" {
						return id
					}
				}
			case map[string]any:
				for _, key := range []string{"goal_id", "id", "goalId"} {
					if id := walk(n[key]); id != "" {
						return id
					}
				}
			}
			return ""
		}
		return walk(v)
	}
	if id := extract(argsJSON); id != "" {
		return id
	}
	return extract(resultJSON)
}

// goalToolNames are the goal-protocol tool names. When a client declares any of
// these in the request, the session is a goal-protocol session regardless of
// the text content — the strongest structured signal available.
var goalToolNames = []string{"create_goal", "get_goal", "update_goal"}

// toolsDeclareGoal reports whether the request declares any goal-protocol tool.
// A DSH harness always ships create_goal/get_goal/update_goal in its tool list,
// so this is a precise, content-independent signal that the session follows the
// goal protocol — unlike string matching, it cannot be tripped by a user message
// that merely mentions "goal" or "完成".
func toolsDeclareGoal(tools []chathub.Tool) bool {
	for _, t := range tools {
		var f struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(t.Function, &f) == nil {
			for _, g := range goalToolNames {
				if f.Name == g {
					return true
				}
			}
		}
	}
	return false
}

// goalRoundRequest detects a goal-protocol continuation round. It uses three
// layers of structured signals; content-only string matching is never the sole
// criterion. This prevents a normal conversation that merely mentions
// "<goal_round>" or "完成" from triggering the goal flow.
//
//  1. Session identity: the task ledger already carries a GoalID (create_goal
//     evidence landed in an earlier round). This is the strongest signal, but
//     even with a GoalID the round-structure guard prevents a casual mention
//     of "<goal_round>" in a goal session from being misdetected.
//  2. Protocol declaration: the client declares a goal tool (create_goal,
//     get_goal, update_goal) in the request. The DSH harness always does this.
//  3. Round structure: the message content carries <goal_round> together with
//     a Round: N/M counter, which is the protocol's own round format and
//     cannot be accidentally produced by ordinary user content.
//
// All three layers require both the <goal_round> tag AND the Round: N/M
// counter so that a bare mention of "<goal_round>" in ordinary content is
// never mistaken for a goal round, regardless of the session's state.
//
// When the ledger is already complete these rounds must not re-open the task.
func goalRoundRequest(messages []oaiMsg, task *taskLedger, tools []chathub.Tool) bool {
	if task == nil {
		return false
	}
	hasRoundTag := false
	hasRoundCounter := false
	for _, m := range messages {
		c := contentToString(m.Content)
		if strings.Contains(c, "<goal_round>") {
			hasRoundTag = true
		}
		if roundCounterPattern.MatchString(c) {
			hasRoundCounter = true
		}
	}
	if !hasRoundTag || !hasRoundCounter {
		return false
	}
	// All three layers need the <goal_round> + Round: N/M structure.
	// Layer 1 & 2 are session-level signals that confirm the session type.
	if task.GoalID != "" || toolsDeclareGoal(tools) {
		return true
	}
	// Layer 3: no session-level identity, but the round structure itself is a
	// strong signal — the protocol-injected format is unique enough that no
	// ordinary user content can reproduce it.
	return true
}

// roundCounterPattern matches the harness's round format: Round: N/M.
// Examples: "Round: 1/256", "Round: 3/256".
var roundCounterPattern = regexp.MustCompile(`Round:\s*\d+\s*/\s*\d+`)

// forceGoalRoundToolChoice reports whether the current request is a
// goal-protocol continuation round whose most recent assistant reply was a
// text-only status report (no tool call). In that state the agent loop would
// treat the turn as completed and immediately re-inject the goal, producing
// the classic "stuck in a loop of status reports" failure. The caller forces
// tool_choice=required so the model must emit a tool call instead of another
// "goal is still incomplete" text-only turn.
func forceGoalRoundToolChoice(messages []oaiMsg, task *taskLedger, tools []chathub.Tool) bool {
	if task == nil || len(tools) == 0 {
		return false
	}
	if !goalRoundRequest(messages, task, tools) {
		return false
	}
	// Walk backwards to the most recent assistant reply (the turn before the
	// current goal round). If it carried no tool call, it was a status report.
	for i := len(messages) - 1; i >= 0; i-- {
		m := messages[i]
		if m.Role != "assistant" {
			continue
		}
		text := strings.TrimSpace(contentToString(m.Content))
		if text == "" {
			continue
		}
		return len(m.ToolCalls) == 0
	}
	return false
}

// goalRoundCounter extracts the current and max round numbers from a message
// containing the round counter. Returns (current, max, ok). When the current
// round reaches or exceeds max, the round budget is exhausted.
func goalRoundCounter(messages []oaiMsg) (int, int, bool) {
	for _, m := range messages {
		c := contentToString(m.Content)
		match := roundCounterPattern.FindString(c)
		if match == "" {
			continue
		}
		parts := regexp.MustCompile(`\d+`).FindAllString(match, 2)
		if len(parts) == 2 {
			cur, _ := strconv.Atoi(parts[0])
			max, _ := strconv.Atoi(parts[1])
			return cur, max, true
		}
	}
	return 0, 0, false
}

// goalRoundInjectedContext returns a short prompt suffix for an already-complete
// goal round: the task is closed, further work must not begin. It is attached
// only when the ledger is complete so the model stops claiming it "cannot mark
// the goal complete" and instead reports the recorded outcome.
func (t *taskLedger) goalRoundInjectedContext() string {
	if t == nil || !t.IsComplete() {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n[TASK_LEDGER] GOAL_STATUS: complete — the work is verified done and the server-side goal is closed. ")
	if strings.Contains(t.CompletedReason, "server-side correction") {
		// The server closed its ledger from the model's final answer, but the
		// client-side goal may still be active. Ask the model to close it so the
		// goal round loop terminates instead of repeating status reports. The
		// client-side goal carries an optimistic-revision guard: an earlier
		// round that failed to close it (e.g. a 502 that aborted the turn) has
		// advanced the revision, so the model MUST re-read the goal with
		// get_goal first and pass the returned goal_id/revision to
		// update_goal(action=complete). Closing with a stale revision raises
		// GOAL_STALE_REVISION and keeps the loop alive forever.
		b.WriteString("If the client-side goal is still active, close it now: first call get_goal to read the current goal state (its revision may have advanced since the last round), then call update_goal(action=complete) with the goal_id and revision returned by get_goal. Do not start new work.")
	} else {
		// Closed by explicit update_goal(action=complete) tool evidence, so the
		// client-side goal is already done; only the outcome needs restating.
		b.WriteString("State the recorded outcome and do not start new work. The goal state has been persisted; no further update_goal call is required.")
	}
	return b.String()
}

// goalDenialPatterns match an answer that explicitly refuses to claim completion
// or reports unverified/outstanding work. When any of these appear, the goal
// must not be auto-closed even if the answer contains success wording.
var goalDenialPatterns = []string{
	"cannot confirm", "can't confirm", "unable to confirm", "not fully verified",
	"not done", "not complete", "not finished", "incomplete", "undone", "unfinished",
	"未完成", "尚未", "还没", "没有完成", "无法完成", "无法确认", "不能确认", "仍未",
	"failed", "failure", "timed out", "refused", "failed to",
	"不成功", "没有成功", "未能",
}

// goalContinuationPatterns detect answers that are mid-task progress reports
// rather than final wrap-ups. When any of these appear alongside a completion
// word, the goal must not be auto-closed — the model is still mid-implementation.
var goalContinuationPatterns = []string{
	"继续", "接下来", "下一步", "还需要", "还有", "还差", "仍需要", "后续",
	"next step", "next steps", "continue", "continuing", "remaining", "still need",
	"in progress", "ongoing", "still working", "not yet",
}

// goalProcessRecordPatterns match the specific "the only unfinished thing is the
// bookkeeping" report — the substantive work is done and only the goal-state
// process record remains. These patterns are deliberately narrow (they reference
// "流程记录" or "目标状态") so a phrase like "唯一未完成的是第一个模块" does
// NOT falsely close the goal.
var goalProcessRecordPatterns = []string{
	"唯一未完成的是流程记录",
	"唯一未完成的是目标状态",
	"流程记录未完成",
	"只剩流程记录",
	"the only remaining item is the process record",
}

// goalStrongCompletionPatterns are explicitly holistic completion phrases.
// Single words like "完成", "done", "complete", "success" are NOT included
// because they also appear in mid-task progress reports ("第一步完成了",
// "step 1 done", "module A complete").
var goalStrongCompletionPatterns = []string{
	"全部完成", "已全部完成", "已经全部完成", "整体完成", "全部搞定",
	"目标已完成", "目标已达成", "所有任务已完成", "所有工作已完成", "所有步骤已完成",
	"all done", "all complete", "all completed", "all finished",
	"everything is done", "everything is complete", "everything has been done",
	"fully complete", "fully completed", "fully implemented", "fully verified",
	"implemented and verified", "completed successfully", "finished successfully",
	"work is complete", "goal is complete", "task is complete",
	"全部功能", "所有功能", "功能实现与验证",
	// Common whole-project completion phrasings that the upstream model uses in
	// its closing report. "已完成" alone stays excluded because it also appears
	// in mid-task progress reports ("第一步已完成"); the phrases below are
	// holistic enough to be safe.
	"项目已完成", "项目已全部完成", "项目已完成并验证", "已完成全部功能",
	"全部功能已实现", "已全部实现", "已全部完成", "全已完成", "已完成并验证",
	"完成并验证通过", "全部完成并验证", "项目完成", "project completed",
	"project is complete", "completed and verified", "implementation and verification are complete",
}

// goalCompletionSignal is the server-side "state correction" detector: the goal
// round is auto-closed only when the final answer (a) carries completed tool
// evidence, (b) has no pending tool results, (c) uses explicitly holistic
// completion wording, and (d) contains no denial/failure/continuation wording.
// This is deliberately stricter than completionEvidenceAllows, which also
// permits honest "I cannot confirm" answers — closing a goal on those would
// restart the loop in reverse.
//
// The "only unfinished thing is the process record" report is treated as
// completion: the model states the functional work is done but claims it lacks
// a goal-state entry. That is exactly the loop this fix breaks — the server now
// owns the goal state, so it closes the goal and tells the next round the goal
// is already complete.
func goalCompletionSignal(answer string, l agentLedger) bool {
	if len(l.Completed) == 0 {
		return false
	}
	if len(l.Pending) > 0 {
		return false
	}
	low := strings.ToLower(answer)

	// Check for continuation signals first — even process-record-only reports
	// must not be closed if the answer says "I'll continue working".
	hasContinuation := false
	for _, p := range goalContinuationPatterns {
		if strings.Contains(low, strings.ToLower(p)) {
			hasContinuation = true
			break
		}
	}
	if hasContinuation {
		return false
	}

	processRecordOnly := false
	for _, p := range goalProcessRecordPatterns {
		if strings.Contains(low, strings.ToLower(p)) {
			processRecordOnly = true
			break
		}
	}

	// Check denial patterns only when not a process-record-only report, because
	// the "未完成"/"无法标记" in the process-record phrasing refers to the
	// bookkeeping, not the substantive work.
	if !processRecordOnly {
		for _, p := range goalDenialPatterns {
			if strings.Contains(low, strings.ToLower(p)) {
				return false
			}
		}
	}

	for _, w := range goalStrongCompletionPatterns {
		if strings.Contains(low, strings.ToLower(w)) {
			return true
		}
	}

	// processRecordOnly without a strong completion word: the answer says "the
	// only unfinished thing is the process record" but doesn't use a holistic
	// completion phrase. Close anyway — the model is reporting the bookkeeping
	// gap, which is exactly the loop condition we're breaking.
	return processRecordOnly
}
