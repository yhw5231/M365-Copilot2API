package web

import (
	"fmt"
	"strings"
	"testing"
)

func TestCompactToolResultKeepsHeadTailAndError(t *testing.T) {
	s := "start\n" + strings.Repeat("progress line\n", 1000) + "ERROR: build failed\nexit code 1"
	got := compactToolResult(s, 800)
	if len(got) > 900 || !strings.Contains(got, "start") || !strings.Contains(got, "ERROR: build failed") || !strings.Contains(got, "exit code 1") || !strings.Contains(got, "truncated") {
		t.Fatalf("bad compact result: %d %q", len(got), got)
	}
}

func TestAgentLedgerRepeatedFailureBlocksContinuation(t *testing.T) {
	msgs := []oaiMsg{
		{Role: "assistant", ToolCalls: []map[string]any{{"id": "c1", "type": "function", "function": map[string]any{"name": "run", "arguments": "{\"cmd\":\"build\"}"}}}},
		{Role: "tool", ToolCallID: "c1", Content: "exit code 1: failed"},
		{Role: "assistant", ToolCalls: []map[string]any{{"id": "c2", "type": "function", "function": map[string]any{"name": "run", "arguments": "{\"cmd\":\"build\"}"}}}},
		{Role: "tool", ToolCallID: "c2", Content: "exit code 1: failed"},
	}
	l := buildAgentLedger(msgs)
	if !l.RepeatedFailure {
		t.Fatalf("expected repeated failure: %+v", l)
	}
	if err := l.CanContinue(32); err == nil || !strings.Contains(err.Error(), "repeated tool failure") {
		t.Fatalf("expected repeated failure error, got: %v", err)
	}
}

// TestAgentLedgerSkipsEmptyNameCall ensures a tool call with an empty name
// (transport-level corruption, not a model strategy loop) does not contribute
// to repeated-failure or stuck-loop detection, and does not leave a pending
// entry that blocks continuation.
func TestAgentLedgerSkipsEmptyNameCall(t *testing.T) {
	msgs := []oaiMsg{
		{Role: "assistant", ToolCalls: []map[string]any{{"id": "c1", "type": "function", "function": map[string]any{"name": "", "arguments": "{\"command\":\"x\"}"}}}},
		{Role: "tool", ToolCallID: "c1", Content: "error: unknown tool"},
		{Role: "assistant", ToolCalls: []map[string]any{{"id": "c2", "type": "function", "function": map[string]any{"name": "", "arguments": "{\"command\":\"x\"}"}}}},
		{Role: "tool", ToolCallID: "c2", Content: "error: unknown tool"},
	}
	l := buildAgentLedger(msgs)
	if l.RepeatedFailure {
		t.Fatalf("empty-name call must not trigger repeated failure: %+v", l)
	}
	if l.RepeatedCall {
		t.Fatalf("empty-name call must not trigger repeated call: %+v", l)
	}
	if len(l.Pending) > 0 {
		t.Fatalf("empty-name call must not leave pending: %+v", l)
	}
	if err := l.CanContinue(32); err != nil {
		t.Fatalf("empty-name call must not block continuation: %v", err)
	}
}

func TestAgentLedgerEvidenceAndUniqueCallIDs(t *testing.T) {
	a := scopedCallID("run", "{}", 0, "turn-a")
	b := scopedCallID("run", "{}", 0, "turn-b")
	if a == b {
		t.Fatal("call IDs collide across turns")
	}
	l := buildAgentLedger([]oaiMsg{{Role: "assistant", ToolCalls: []map[string]any{{"id": "c1", "type": "function", "function": map[string]any{"name": "create", "arguments": "{}"}}}}, {Role: "tool", ToolCallID: "c1", Content: "created"}})
	if len(l.Completed) != 1 || !strings.Contains(l.RouterContext(), "c1") {
		t.Fatalf("missing evidence: %+v", l)
	}
}

func TestAgentLedgerDetectsRepeatedCallAndRoundLimit(t *testing.T) {
	var msgs []oaiMsg
	for i := 0; i < 4; i++ {
		id := fmt.Sprintf("c%d", i)
		msgs = append(msgs, oaiMsg{Role: "assistant", ToolCalls: []map[string]any{{"id": id, "type": "function", "function": map[string]any{"name": "poll", "arguments": "{\"id\":1}"}}}}, oaiMsg{Role: "tool", ToolCallID: id, Content: "still pending"})
	}
	l := buildAgentLedger(msgs)
	if !l.RepeatedCall || l.ToolRounds != 4 {
		t.Fatalf("loop not detected: %+v", l)
	}
	if err := l.CanContinue(3); err == nil {
		t.Fatal("expected round limit")
	}
}

// TestAgentLedgerUnlimitedRounds ensures maxRounds=0 disables the round
// limit without suppressing the other loop guards.
func TestAgentLedgerUnlimitedRounds(t *testing.T) {
	// Distinct calls (unique arguments) so only the round limit — not the
	// stuck-loop guard — is exercised. Under maxRounds=0 a large number of
	// calls must be allowed.
	var msgs []oaiMsg
	for i := 0; i < 8; i++ {
		id := fmt.Sprintf("u%d", i)
		msgs = append(msgs, oaiMsg{Role: "assistant", ToolCalls: []map[string]any{{"id": id, "type": "function", "function": map[string]any{"name": "poll", "arguments": fmt.Sprintf("{\"id\":%d}", i)}}}}, oaiMsg{Role: "tool", ToolCallID: id, Content: "pending"})
	}
	l := buildAgentLedger(msgs)
	if l.ToolRounds != 8 {
		t.Fatalf("expected 8 rounds, got %d", l.ToolRounds)
	}
	if err := l.CanContinue(0); err != nil {
		t.Fatalf("maxRounds=0 must not impose a round limit: %v", err)
	}
	// A distinct repeated failure must still stop continuation under 0.
	failMsgs := []oaiMsg{
		{Role: "assistant", ToolCalls: []map[string]any{{"id": "f1", "type": "function", "function": map[string]any{"name": "run", "arguments": "{\"cmd\":\"x\"}"}}}},
		{Role: "tool", ToolCallID: "f1", Content: "exit code 1: failed"},
		{Role: "assistant", ToolCalls: []map[string]any{{"id": "f2", "type": "function", "function": map[string]any{"name": "run", "arguments": "{\"cmd\":\"x\"}"}}}},
		{Role: "tool", ToolCallID: "f2", Content: "exit code 1: failed"},
	}
	if err := buildAgentLedger(failMsgs).CanContinue(0); err == nil || !strings.Contains(err.Error(), "repeated tool failure") {
		t.Fatalf("repeated failure must block continuation even when rounds are unlimited, got: %v", err)
	}
}

func TestActiveMessagesIgnoresOlderToolHistory(t *testing.T) {
	var msgs []oaiMsg
	for i := 0; i < 20; i++ {
		id := fmt.Sprintf("old%d", i)
		msgs = append(msgs,
			oaiMsg{Role: "assistant", ToolCalls: []map[string]any{{"id": id, "type": "function", "function": map[string]any{"name": "old", "arguments": "{}"}}}},
			oaiMsg{Role: "tool", ToolCallID: id, Content: "done"},
		)
	}
	msgs = append(msgs, oaiMsg{Role: "user", Content: "continue with a new model"})
	full := buildAgentLedger(msgs)
	active := buildAgentLedger(activeMessages(msgs))
	if full.ToolRounds < 20 {
		t.Fatalf("expected full history tools, got %d", full.ToolRounds)
	}
	if active.ToolRounds != 0 {
		t.Fatalf("new user turn should reset round limit scope, got %d", active.ToolRounds)
	}
	if err := active.CanContinue(16); err != nil {
		t.Fatalf("new user turn blocked by old history: %v", err)
	}
}

func TestCompletionGuardRejectsPendingAndUnsupportedSuccess(t *testing.T) {
	l := buildAgentLedger([]oaiMsg{
		{Role: "assistant", ToolCalls: []map[string]any{
			{"id": "p1", "type": "function", "function": map[string]any{"name": "deploy", "arguments": "{}"}},
		}},
	})
	if completionEvidenceAllows("Deployment completed successfully", l) {
		t.Fatal("pending action allowed as complete")
	}
}

func TestCompletionGuardRejectsUnsupportedSuccess(t *testing.T) {
	if completionEvidenceAllows("Installed, started, and verified successfully", buildAgentLedger(nil)) {
		t.Fatal("unsupported success allowed")
	}
	if !completionEvidenceAllows("I cannot confirm completion because no tool results were returned.", buildAgentLedger(nil)) {
		t.Fatal("honest incomplete response rejected")
	}
	// A tool-less conversational answer that makes no completion claim must
	// still pass: the guard only rejects self-congratulatory success with no
	// supporting tool evidence.
	if !completionEvidenceAllows("Here is the code you asked for.", buildAgentLedger(nil)) {
		t.Fatal("plain conversational answer rejected")
	}
}

// TestAgentLedgerHasCompletedSkipsFailedCalls ensures hasCompleted returns true
// only for non-failed (successful) calls. A failed call with the same name and
// arguments must NOT be treated as completed, so the model can retry it.
func TestAgentLedgerHasCompletedSkipsFailedCalls(t *testing.T) {
	success := agentLedger{Completed: []toolEvidence{{Name: "edit", Arguments: `{"file_path":"x.py","old_string":"a","new_string":"b"}`, Failed: false}}}
	if !success.hasCompleted("edit", `{"file_path":"x.py","old_string":"a","new_string":"b"}`) {
		t.Fatal("successful call must be reported as completed")
	}
	failed := agentLedger{Completed: []toolEvidence{{Name: "edit", Arguments: `{"file_path":"x.py","old_string":"a","new_string":"b"}`, Failed: true}}}
	if failed.hasCompleted("edit", `{"file_path":"x.py","old_string":"a","new_string":"b"}`) {
		t.Fatal("failed call must NOT be reported as completed")
	}
	empty := agentLedger{}
	if empty.hasCompleted("edit", `{}`) {
		t.Fatal("empty ledger must not report completed")
	}
	canonLedger := agentLedger{Completed: []toolEvidence{{Name: "read", Arguments: `{"file_path": "x.py", "limit": 10}`, Failed: false}}}
	if !canonLedger.hasCompleted("read", `{"limit":10,"file_path":"x.py"}`) {
		t.Fatal("canonical args must match regardless of key order")
	}
}

func TestAgentLedgerHasEditIdentityFailure(t *testing.T) {
	// No completed calls -> false
	if (agentLedger{}).hasEditIdentityFailure() {
		t.Fatal("empty ledger must not flag edit identity failure")
	}
	// Successful edit -> false
	ok := agentLedger{Completed: []toolEvidence{{Name: "edit", Arguments: `{}`, Result: "file updated", Failed: false}}}
	if ok.hasEditIdentityFailure() {
		t.Fatal("successful edit must not flag identity failure")
	}
	// edit failed with exactly the old==new error -> true
	ident := agentLedger{Completed: []toolEvidence{{Name: "edit", Arguments: `{"file_path":"x.py","old_string":"a","new_string":"a"}`, Result: "Error: old_string and new_string must differ", Failed: true}}}
	if !ident.hasEditIdentityFailure() {
		t.Fatal("edit failing with old_string==new_string must be flagged")
	}
	// Different old/new content but same failure signature -> still true
	// (this is the case that defeats same-signature repeated-failure tracking)
	ident2 := agentLedger{Completed: []toolEvidence{{Name: "edit", Arguments: `{"file_path":"x.py","old_string":"@router.get...","new_string":"@router.get..."}`, Result: "Error: old_string and new_string must differ", Failed: true}}}
	if !ident2.hasEditIdentityFailure() {
		t.Fatal("edit with different args but same identity failure must still be flagged")
	}
	// A different edit failure (old_string not found) -> false
	notFound := agentLedger{Completed: []toolEvidence{{Name: "edit", Arguments: `{}`, Result: "Error: old_string not found in file", Failed: true}}}
	if notFound.hasEditIdentityFailure() {
		t.Fatal("edit failing with old_string-not-found must NOT be flagged as identity failure")
	}
	// A non-edit tool failure -> false
	pwshFail := agentLedger{Completed: []toolEvidence{{Name: "pwsh", Arguments: `{}`, Result: "Error: command not found", Failed: true}}}
	if pwshFail.hasEditIdentityFailure() {
		t.Fatal("pwsh failure must not be flagged as edit identity failure")
	}
}

func TestAgentLedgerHasPwshIdentityWrite(t *testing.T) {
	// Empty ledger -> false
	if (agentLedger{}).hasPwshIdentityWrite() {
		t.Fatal("empty ledger must not flag pwsh identity write")
	}
	// pwsh command assigning a line to the same value it asserts -> true
	identityCmd := "$path = 'x.py'; $lines = Get-Content -LiteralPath $path; if ($lines[70].Trim() -ne ') -> list') { throw 'x' }; $lines[70] = ') -> list'; Set-Content -LiteralPath $path -Value $lines"
	ident := agentLedger{Completed: []toolEvidence{{Name: "pwsh", Arguments: fmt.Sprintf(`{"command":%q}`, identityCmd), Result: "Error: SyntaxError", Failed: true}}}
	if !ident.hasPwshIdentityWrite() {
		t.Fatal("pwsh assigning line to asserted value must be flagged as identity write")
	}
	// pwsh assigning a line to a DIFFERENT value -> false
	fixCmd := "$path = 'x.py'; $lines = Get-Content -LiteralPath $path; if ($lines[70].Trim() -ne ') -> list') { throw 'x' }; $lines[70] = ') -> list[UserResponse]:'; Set-Content -LiteralPath $path -Value $lines"
	fix := agentLedger{Completed: []toolEvidence{{Name: "pwsh", Arguments: fmt.Sprintf(`{"command":%q}`, fixCmd), Result: "ok", Failed: false}}}
	if fix.hasPwshIdentityWrite() {
		t.Fatal("pwsh assigning a corrected value must NOT be flagged")
	}
	// pwsh with no line assignment -> false
	noAssign := agentLedger{Completed: []toolEvidence{{Name: "pwsh", Arguments: `{"command":"Get-Location"}`, Result: "ok", Failed: false}}}
	if noAssign.hasPwshIdentityWrite() {
		t.Fatal("pwsh without line assignment must not be flagged")
	}
	// non-shell tool -> false
	editOnly := agentLedger{Completed: []toolEvidence{{Name: "edit", Arguments: `{}`, Result: "ok", Failed: false}}}
	if editOnly.hasPwshIdentityWrite() {
		t.Fatal("edit call must not be flagged as pwsh identity write")
	}
	// Real command captured from the failing goal-loop trace: asserts line 70
	// is ') -> list' then assigns line 70 to the same broken value -> true.
	realCmd := `$path = 'D:\NET\ai\ythh-1\src\recruitment_agent\api\routes\admin.py'; $lines = Get-Content -LiteralPath $path -Encoding UTF8; if ($lines.Count -lt 71 -or $lines[70].Trim() -ne ') -> list') { throw 'Expected invalid signature was not found at line 71.' }; $lines[70] = ') -> list'; Set-Content -LiteralPath $path -Value $lines -Encoding UTF8; python -m py_compile src\recruitment_agent\api\routes\admin.py src\recruitment_agent\api\routes\auth.py src\recruitment_agent\api\app.py`
	real := agentLedger{Completed: []toolEvidence{{Name: "pwsh", Arguments: fmt.Sprintf(`{"command":%q}`, realCmd), Result: "Error: SyntaxError expected ':'", Failed: true}}}
	if !real.hasPwshIdentityWrite() {
		t.Fatal("real trace pwsh command must be flagged as identity write")
	}
	// Same command but assigning the CORRECTED line -> false.
	fixedCmd := `$path = 'D:\NET\ai\ythh-1\src\recruitment_agent\api\routes\admin.py'; $lines = Get-Content -LiteralPath $path -Encoding UTF8; if ($lines.Count -lt 71 -or $lines[70].Trim() -ne ') -> list') { throw 'x' }; $lines[70] = ') -> list[UserResponse]:'; Set-Content -LiteralPath $path -Value $lines -Encoding UTF8`
	fixed := agentLedger{Completed: []toolEvidence{{Name: "pwsh", Arguments: fmt.Sprintf(`{"command":%q}`, fixedCmd), Result: "ok", Failed: false}}}
	if fixed.hasPwshIdentityWrite() {
		t.Fatal("real trace command assigning corrected line must NOT be flagged")
	}
}
