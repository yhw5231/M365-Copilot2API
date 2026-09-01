package web

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestValidateDetectedToolCallsRejectsEditOldEqualsNew(t *testing.T) {
	tools := []map[string]any{
		{"type": "function", "function": map[string]any{"name": "edit", "parameters": map[string]any{"type": "object", "properties": map[string]any{"file_path": map[string]any{"type": "string"}, "old_string": map[string]any{"type": "string"}, "new_string": map[string]any{"type": "string"}}, "required": []any{"file_path", "old_string", "new_string"}}}},
		{"type": "function", "function": map[string]any{"name": "read", "parameters": map[string]any{"type": "object", "properties": map[string]any{"file_path": map[string]any{"type": "string"}}, "required": []any{"file_path"}}}},
	}
	// An edit with old_string == new_string must be rejected.
	bad := detectedToolCall{Name: "edit", Arguments: json.RawMessage(`{"file_path":"x.py","old_string":"x","new_string":"x"}`)}
	valid, rejected := validateDetectedToolCalls([]detectedToolCall{bad}, tools, nil)
	if len(valid) != 0 {
		t.Fatal("edit with old==new must not be in valid calls")
	}
	if len(rejected) != 1 {
		t.Fatalf("edit with old==new must be rejected; got %d rejected", len(rejected))
	}
	if rejected[0].Reason != "old_string and new_string must differ" {
		t.Fatalf("wrong rejection reason: %q", rejected[0].Reason)
	}
	// An edit with OLD != new must pass.
	good := detectedToolCall{Name: "edit", Arguments: json.RawMessage(`{"file_path":"x.py","old_string":"x","new_string":"y"}`)}
	valid, rejected = validateDetectedToolCalls([]detectedToolCall{good}, tools, nil)
	if len(valid) != 1 {
		t.Fatal("edit with old!=new must be accepted")
	}
	if len(rejected) != 0 {
		t.Fatal("no rejection expected")
	}
	// Non-edit tools must not be affected.
	readCall := detectedToolCall{Name: "read", Arguments: json.RawMessage(`{"file_path":"x.py"}`)}
	valid, rejected = validateDetectedToolCalls([]detectedToolCall{readCall}, tools, nil)
	if len(valid) != 1 {
		t.Fatal("read must pass through")
	}
	// Mix: one bad edit + one good read.
	valid, rejected = validateDetectedToolCalls([]detectedToolCall{bad, readCall}, tools, nil)
	if len(valid) != 1 || valid[0].Name != "read" {
		t.Fatal("read must survive the mix; edit must be rejected")
	}
	if len(rejected) != 1 {
		t.Fatal("edit must be rejected in mixed calls")
	}
	// Edit with empty old_string must not be rejected (edge case: no-op bound by schema).
	emptyOld := detectedToolCall{Name: "edit", Arguments: json.RawMessage(`{"file_path":"x.py","old_string":"","new_string":"y"}`)}
	valid, rejected = validateDetectedToolCalls([]detectedToolCall{emptyOld}, tools, nil)
	if len(valid) != 1 {
		t.Fatal("edit with empty old_string must be accepted (schema decides)")
	}
}

func TestValidateDetectedToolCallsRejectsPwshIdentityWrite(t *testing.T) {
	tools := []map[string]any{
		{"type": "function", "function": map[string]any{"name": "pwsh", "parameters": map[string]any{"type": "object", "properties": map[string]any{"command": map[string]any{"type": "string"}}, "required": []any{"command"}}}},
		{"type": "function", "function": map[string]any{"name": "read", "parameters": map[string]any{"type": "object", "properties": map[string]any{"file_path": map[string]any{"type": "string"}}, "required": []any{"file_path"}}}},
	}
	// A pwsh identity-write: assert line holds X, then assign X — same value.
	badCmd := `$path = 'x.py'; $lines = Get-Content -LiteralPath $path -Encoding UTF8; if ($lines[70].Trim() -ne ') -> list') { throw 'not found' }; $lines[70] = ') -> list'; Set-Content -LiteralPath $path -Value $lines -Encoding UTF8; python -m py_compile x.py`
	bad := detectedToolCall{Name: "pwsh", Arguments: json.RawMessage(`{"command":"` + badCmd + `"}`)}
	valid, rejected := validateDetectedToolCalls([]detectedToolCall{bad}, tools, nil)
	if len(valid) != 0 {
		t.Fatal("pwsh identity-write must not be in valid calls")
	}
	if len(rejected) != 1 {
		t.Fatalf("pwsh identity-write must be rejected; got %d rejected", len(rejected))
	}
	if !strings.Contains(rejected[0].Reason, "identity write") {
		t.Fatalf("wrong rejection reason: %q", rejected[0].Reason)
	}
	// A pwsh command that writes a DIFFERENT value must pass.
	goodCmd := `$path = 'x.py'; $lines = Get-Content -LiteralPath $path -Encoding UTF8; if ($lines[70].Trim() -ne ') -> list') { throw 'not found' }; $lines[70] = ') -> list[UserResponse]:'; Set-Content -LiteralPath $path -Value $lines -Encoding UTF8`
	good := detectedToolCall{Name: "pwsh", Arguments: json.RawMessage(`{"command":"` + goodCmd + `"}`)}
	valid, rejected = validateDetectedToolCalls([]detectedToolCall{good}, tools, nil)
	if len(valid) != 1 {
		t.Fatal("pwsh with different value must be accepted")
	}
	if len(rejected) != 0 {
		t.Fatal("no rejection expected")
	}
	// A pwsh without line-assign pattern must pass.
	cleanCmd := `$path = 'x.py'; Get-Content -LiteralPath $path -Encoding UTF8 | Select-Object -First 5`
	clean := detectedToolCall{Name: "pwsh", Arguments: json.RawMessage(`{"command":"` + cleanCmd + `"}`)}
	valid, rejected = validateDetectedToolCalls([]detectedToolCall{clean}, tools, nil)
	if len(valid) != 1 {
		t.Fatal("pwsh without line-assign must pass")
	}
	// Mix: bad pwsh + good read.
	readCall := detectedToolCall{Name: "read", Arguments: json.RawMessage(`{"file_path":"x.py"}`)}
	valid, rejected = validateDetectedToolCalls([]detectedToolCall{bad, readCall}, tools, nil)
	if len(valid) != 1 || valid[0].Name != "read" {
		t.Fatal("read must survive the mix; pwsh identity-write must be rejected")
	}
	if len(rejected) != 1 {
		t.Fatal("pwsh identity-write must be rejected in mixed calls")
	}
}
