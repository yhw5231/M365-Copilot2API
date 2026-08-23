package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
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
type taskLedger struct {
	OriginalGoal   string         `json:"original_goal,omitempty"`
	Constraints    []string       `json:"constraints,omitempty"`
	Executed       []string       `json:"executed,omitempty"`
	ToolResults    []toolEvidence `json:"tool_results,omitempty"`
	Remaining      []string       `json:"remaining,omitempty"`
	AccountID      string         `json:"account_id,omitempty"`
	ConversationID string         `json:"conversation_id,omitempty"`
	SessionID      string         `json:"session_id,omitempty"`
	Failures       []string       `json:"failures,omitempty"`
	Switches       []string       `json:"switches,omitempty"`
	UpdatedAt      time.Time      `json:"updated_at,omitempty"`
}

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
	b.WriteString("TASK_RULE: Continue the original goal from where the evidence left off. Never restart the task or re-ask what it is. A completed step must not be repeated.")
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
func (t *taskLedger) mergeEvidence(l agentLedger) {
	if t == nil {
		return
	}
	for _, e := range l.Completed {
		t.ToolResults = appendUniqueEvidence(t.ToolResults, e)
		step := compactTaskText(e.Name+"("+e.Arguments+")", taskStepMax)
		t.Executed = appendUniqueString(t.Executed, step)
	}
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
	if id := strings.TrimSpace(r.Header.Get(sessionHeaderName)); id != "" {
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
	if strings.TrimSpace(body.User) != "" && s.userSessions != nil {
		if us, ok := s.userSessions.Get(body.User); ok {
			if b, ok := s.sessionResolver.GetSession(us.SessionID); ok {
				return b.Task
			}
		}
	}
	return nil
}