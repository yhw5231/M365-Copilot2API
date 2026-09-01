package web

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

type toolEvidence struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	Result    string `json:"result"`
	Failed    bool   `json:"failed"`
}
type agentLedger struct {
	Completed           []toolEvidence `json:"completed"`
	Pending             []toolEvidence `json:"pending"`
	ToolRounds          int            `json:"tool_rounds"`
	RepeatedCall        bool           `json:"repeated_call"`
	RepeatedFailure     bool           `json:"repeated_failure"`
	RepetitionSignature string         `json:"repetition_signature,omitempty"`
	StuckLoop           bool           `json:"stuck_loop"`
}

var failureSignal = regexp.MustCompile(`(?i)(exit\s*(code|status)?\s*[:=]?\s*[1-9]\d*|\berror\b|\bfailed\b|\bfailure\b|exception|traceback|timed?\s*out|permission denied|not found|refused)`)
var unsupportedSuccess = regexp.MustCompile(`(?i)\b(installed|created|written|executed|ran|started|deployed|deleted|verified|completed|succeeded|successful(?:ly)?)\b`)

func compactToolResult(s string, limit int) string {
	s = strings.TrimSpace(s)
	if limit < 200 {
		limit = 200
	}
	if len(s) <= limit {
		return s
	}
	head := limit / 3
	tail := limit - head - 80
	if tail < 80 {
		tail = 80
	}
	return s[:head] + fmt.Sprintf("\n... [truncated %d bytes] ...\n", len(s)-head-tail) + s[len(s)-tail:]
}

// scopedCallID returns a globally unique tool call id. The scope parameters
// are kept for signature compatibility with callers that pass per-turn
// context; the id itself must not depend on call content or scope text,
// otherwise repeating the same tool+arguments across turns collides
// (duplicate tool call id errors from clients).
func scopedCallID(name, args string, index int, scope string) string {
	return "call_" + uuid.NewString()
}
func buildAgentLedger(messages []oaiMsg) agentLedger {
	calls := map[string]toolEvidence{}
	order := []string{}
	for _, m := range messages {
		if m.Role == "assistant" {
			for _, raw := range m.ToolCalls {
				id, _ := raw["id"].(string)
				fn, _ := raw["function"].(map[string]any)
				name, _ := fn["name"].(string)
				args := fmt.Sprint(fn["arguments"])
				if id != "" {
					calls[id] = toolEvidence{ID: id, Name: name, Arguments: args}
					order = append(order, id)
				}
			}
		}
		if m.Role == "tool" {
			if e, ok := calls[m.ToolCallID]; ok {
				e.Result = compactToolResult(contentToString(m.Content), 4000)
				e.Failed = failureSignal.MatchString(e.Result)
				calls[m.ToolCallID] = e
			}
		}
	}
	l := agentLedger{}
	seenCall := map[string]int{}
	seenFailure := map[string]int{}
	for _, id := range order {
		e := calls[id]
		l.ToolRounds++
		// An empty tool name is a transport-level corruption (the client saw
		// "unknown tool" and never executed it), not a model strategy loop.
		// Exclude it from repetition/pending accounting so a corrupted call
		// cannot 409 the whole turn.
		if e.Name == "" {
			l.Completed = append(l.Completed, e)
			continue
		}
		sig := e.Name + "\x00" + canonicalToolArguments(e.Arguments)
		seenCall[sig]++
		if seenCall[sig] >= 2 {
			l.RepeatedCall = true
			l.RepetitionSignature = sig
		}
		if seenCall[sig] >= 3 {
			l.StuckLoop = true
		}
		if e.Result == "" {
			l.Pending = append(l.Pending, e)
		} else {
			l.Completed = append(l.Completed, e)
			if e.Failed {
				fs := e.Name + "\x00" + canonicalToolArguments(e.Arguments) + "\x00" + normalizeFailure(e.Result)
				seenFailure[fs]++
				if seenFailure[fs] >= 2 {
					l.RepeatedFailure = true
					l.RepetitionSignature = fs
				}
				if seenFailure[fs] >= 3 {
					l.StuckLoop = true
				}
			}
		}
	}
	return l
}
func normalizeFailure(s string) string {
	s = strings.ToLower(s)
	s = regexp.MustCompile(`\d+`).ReplaceAllString(s, "#")
	if len(s) > 500 {
		s = s[:500]
	}
	return s
}
func (l agentLedger) RouterContext() string {
	type compact struct {
		Completed    []toolEvidence `json:"completed"`
		Pending      []toolEvidence `json:"pending"`
		RepeatedCall bool           `json:"repeated_call"`
	}
	b, _ := json.Marshal(compact{l.Completed, l.Pending, l.RepeatedCall})
	hint := "Use only this compact evidence. A completed call is final evidence; do not issue the same name and arguments again."
	if l.RepeatedFailure {
		hint += " The same call failed repeatedly; change strategy instead of retrying unchanged."
	}
	return hint + "\nEVIDENCE_LEDGER: " + string(b)
}
func canonicalToolArguments(s string) string {
	s = strings.TrimSpace(s)
	var v any
	if json.Unmarshal([]byte(s), &v) == nil {
		b, _ := json.Marshal(v)
		return string(b)
	}
	return s
}

// hasFailed reports whether any completed tool call in the ledger failed.
// A failed call is the model's strongest trigger for sandbox hallucination:
// after a tool error (e.g. an edit old_string mismatch) the model may
// retroactively claim the whole environment is unavailable, denying even the
// calls that succeeded earlier in the same turn ("pwsh is a fictional
// context"). The workspace/tool misjudgment gate must therefore stay armed
// when failures are present, even though successes also exist.
func (l agentLedger) hasFailed() bool {
	for _, e := range l.Completed {
		if e.Failed {
			return true
		}
	}
	return false
}

// hasFailedCount returns how many completed tool calls failed. Used for
// diagnostics when injecting the identity-write correction.
func (l agentLedger) hasFailedCount() int {
	n := 0
	for _, e := range l.Completed {
		if e.Failed {
			n++
		}
	}
	return n
}

// hasEditIdentityFailure reports whether any edit call in the ledger failed
// because old_string and new_string were identical. Unlike CanContinue's
// repeated-failure detection — which keys on identical arguments — the model
// may submit different old==new content each time, so same-signature tracking
// never fires. This check catches that specific failure on a single occurrence.
func (l agentLedger) hasEditIdentityFailure() bool {
	for _, e := range l.Completed {
		if e.Name == "edit" && e.Failed && strings.Contains(strings.ToLower(e.Result), "old_string and new_string must differ") {
			return true
		}
	}
	return false
}

// pwshLineAssignValue extracts the string assigned to a $lines[N] element in a
// pwsh command, e.g. `$lines[70] = ') -> list'`. Empty when none found.
var pwshLineAssignRe = regexp.MustCompile(`\$lines\[\d+\]\s*=\s*'([^']*)'`)

// pwshLineAssertValue extracts the single-quoted literal a pwsh command asserts
// the current line content against, e.g. `$lines[70].Trim() -ne ') -> list'`.
// Empty when none found.
var pwshLineAssertRe = regexp.MustCompile(`\$lines\[\d+\][^;]*?(?:\.Trim\(\))?\s*(?:-ne|-eq)\s*'([^']*)'`)

// hasPwshIdentityWrite reports whether any pwsh call in the ledger assigned a
// line to the same value it asserted the line already contains — an identity
// write that changes nothing (the pwsh analogue of an edit with
// old_string == new_string). The model's goal-loop workaround is to "fix" a
// line by writing back the exact broken text, which py_compile then still
// rejects. Because each pwsh command may differ slightly, same-signature
// repeated-failure tracking never fires; this checks the semantic pattern on a
// single occurrence.
func (l agentLedger) hasPwshIdentityWrite() bool {
	for _, e := range l.Completed {
		if e.Name != "pwsh" && e.Name != "powershell" && e.Name != "bash" && e.Name != "sh" {
			continue
		}
		var args struct {
			Command string `json:"command"`
		}
		if json.Unmarshal([]byte(e.Arguments), &args) != nil || args.Command == "" {
			continue
		}
		assign := pwshLineAssignRe.FindStringSubmatch(args.Command)
		assert := pwshLineAssertRe.FindStringSubmatch(args.Command)
		if len(assign) < 2 || len(assert) < 2 {
			continue
		}
		if assign[1] == assert[1] {
			return true
		}
	}
	return false
}

func (l agentLedger) hasCompleted(name, args string) bool {
	want := canonicalToolArguments(args)
	for _, e := range l.Completed {
		if e.Name == name && canonicalToolArguments(e.Arguments) == want && !e.Failed {
			return true
		}
	}
	return false
}
// goalProtocolTools are the goal-state tools that the agent loop invokes
// repeatedly across rounds: get_goal is a read of the (possibly advanced)
// goal revision and update_goal carries its own optimistic-revision guard, so
// re-invoking them is legitimate — pruning them would leave a forced
// tool_choice=required round with zero calls and trigger the misleading
// "model did not select a required tool after constrained retry" 502 while the
// model actually DID choose a tool.
var goalProtocolTools = map[string]bool{
	"create_goal": true,
	"get_goal":    true,
	"update_goal": true,
}

func filterCompletedCalls(calls []detectedToolCall, l agentLedger) []detectedToolCall {
	out := calls[:0]
	for _, c := range calls {
		if goalProtocolTools[c.Name] || !l.hasCompleted(c.Name, string(c.Arguments)) {
			out = append(out, c)
		}
	}
	return out
}
func (l agentLedger) CanContinue(maxRounds int) error {
	if maxRounds > 0 && l.ToolRounds >= maxRounds {
		return fmt.Errorf("tool round limit reached: %d", maxRounds)
	}
	if l.StuckLoop {
		return fmt.Errorf("stuck tool loop detected: same call repeated 3+ times")
	}
	if l.RepeatedFailure {
		return fmt.Errorf("repeated tool failure detected: %s", l.RepetitionSignature)
	}
	if len(l.Pending) > 0 {
		return fmt.Errorf("pending tool results must be returned before another turn")
	}
	return nil
}
func maxToolRounds() int {
	if raw, ok := os.LookupEnv("M365_MAX_TOOL_ROUNDS"); ok {
		if n, e := strconv.Atoi(strings.TrimSpace(raw)); e == nil && n >= 0 && n <= 512 {
			return n
		}
		return 0
	}
	if n := currentSettings().MaxToolRounds; n >= 0 && n <= 512 {
		return n
	}
	return 0
}
func activeMessages(messages []oaiMsg) []oaiMsg {
	last := -1
	for i, m := range messages {
		if m.Role == "user" {
			last = i
		}
	}
	if last <= 0 {
		return messages
	}
	return messages[last:]
}

// identityFailureDetail collects evidence about a concrete identity-write
// failure (edit old==new or pwsh line-self-assign) from the ledger so the
// correction message can reference the model's own content instead of a generic
// "old must differ" hint. Reports the file path, the identical content the
// model tried to write back unchanged, and any associated py_compile syntax
// error from a pwsh call.
type identityFailureDetail struct {
	FilePath    string
	OldText     string // the identical old/new content (truncated)
	SyntaxError string // py_compile error from a pwsh result (truncated)
}

func (l agentLedger) identityFailureDetail() identityFailureDetail {
	var d identityFailureDetail
	for _, e := range l.Completed {
		if e.Name == "edit" && e.Failed && strings.Contains(strings.ToLower(e.Result), "old_string and new_string must differ") && d.OldText == "" {
			var args struct {
				FilePath string `json:"file_path"`
				Old      string `json:"old_string"`
			}
			if json.Unmarshal([]byte(e.Arguments), &args) == nil && args.Old != "" {
				d.FilePath = args.FilePath
				d.OldText = args.Old
			}
		}
		if (e.Name == "pwsh" || e.Name == "powershell" || e.Name == "bash" || e.Name == "sh") && d.SyntaxError == "" {
			if i := strings.Index(e.Result, "SyntaxError"); i >= 0 {
				start := i - 120
				if start < 0 {
					start = 0
				}
				end := i + 200
				if end > len(e.Result) {
					end = len(e.Result)
				}
				d.SyntaxError = strings.TrimSpace(e.Result[start:end])
			}
		}
	}
	if len(d.OldText) > 400 {
		d.OldText = d.OldText[:400]
	}
	if len(d.SyntaxError) > 400 {
		d.SyntaxError = d.SyntaxError[:400]
	}
	return d
}
func completionEvidenceAllows(answer string, l agentLedger) bool {
	if len(l.Pending) > 0 {
		return false
	}
	if len(l.Completed) == 0 && len(l.Pending) == 0 {
		return !unsupportedSuccess.MatchString(answer)
	}
	low := strings.ToLower(answer)
	failureKeywords := []string{"cannot confirm", "not confirmed", "unable to confirm", "no tool result", "no matching tool results were returned", "no external action has been verified"}
	hasFailure := false
	for _, h := range failureKeywords {
		if strings.Contains(low, h) {
			hasFailure = true
			break
		}
	}
	if len(l.Completed) > 0 {
		// Real tool evidence exists; only an explicit "cannot confirm" the
		// results guards the final claim.
		return !hasFailure
	}
	if hasFailure {
		// Honest "I cannot confirm" without any tool evidence is allowed.
		return true
	}
	// No completed tools and no honest denial: reject self-congratulatory
	// success claims that have no underlying tool evidence.
	if unsupportedSuccess.MatchString(answer) {
		return false
	}
	return true
}
func completedCallIDs(l agentLedger) []string {
	o := make([]string, 0, len(l.Completed))
	for _, e := range l.Completed {
		o = append(o, e.ID)
	}
	sort.Strings(o)
	return o
}
