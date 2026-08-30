package web

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"m365-copilot2api/internal/chathub"
)

func reminderTools() []chathub.Tool {
	raw := func(s string) json.RawMessage { return json.RawMessage(s) }
	return []chathub.Tool{
		{Type: "function", Function: raw(`{"name":"read","description":"Read a file","parameters":{}}`)},
		{Type: "function", Function: raw(`{"name":"edit","description":"Edit a file","parameters":{}}`)},
		{Type: "function", Function: raw(`{"name":"pwsh","description":"Run PowerShell","parameters":{}}`)},
		// duplicate should be de-duplicated
		{Type: "function", Function: raw(`{"name":"read","description":"again","parameters":{}}`)},
		// malformed entry should be skipped
		{Type: "function", Function: raw(`not-json`)},
		// empty name should be skipped
		{Type: "function", Function: raw(`{"name":"","parameters":{}}`)},
	}
}

func TestToolNamesFrom(t *testing.T) {
	names := toolNamesFrom(reminderTools())
	want := []string{"read", "edit", "pwsh"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("names=%v want=%v", names, want)
	}
}

func TestInjectToolReminderAppendsOnce(t *testing.T) {
	os.Setenv("M365_INJECT_TOOL_REMINDER", "1")
	defer os.Unsetenv("M365_INJECT_TOOL_REMINDER")
	msgs := []oaiMsg{{Role: "user", Content: "hello"}}
	out := injectToolReminder(msgs, reminderTools())
	if len(out) != len(msgs)+1 {
		t.Fatalf("len=%d want %d", len(out), len(msgs)+1)
	}
	last := out[len(out)-1]
	if last.Role != "system" {
		t.Fatalf("role=%q want system", last.Role)
	}
	text, _ := last.Content.(string)
	for _, needle := range []string{"[TOOL_REMINDER]", "read, edit, pwsh", "old_string and new_string DIFFER", "Do NOT claim these tools are unavailable"} {
		if !strings.Contains(text, needle) {
			t.Fatalf("missing %q in reminder:\n%s", needle, text)
		}
	}
	// Injecting again over the ORIGINAL msgs (which never contain the reminder)
	// must add exactly one more copy — simulating the next round.
	out2 := injectToolReminder(msgs, reminderTools())
	if len(out2) != len(msgs)+1 {
		t.Fatalf("second round len=%d want %d", len(out2), len(msgs)+1)
	}
}

func TestInjectToolReminderDisabled(t *testing.T) {
	os.Setenv("M365_INJECT_TOOL_REMINDER", "0")
	defer os.Unsetenv("M365_INJECT_TOOL_REMINDER")
	msgs := []oaiMsg{{Role: "user", Content: "hello"}}
	out := injectToolReminder(msgs, reminderTools())
	if len(out) != len(msgs) {
		t.Fatalf("disabled: len=%d want %d", len(out), len(msgs))
	}
}

func TestInjectToolReminderNoTools(t *testing.T) {
	os.Setenv("M365_INJECT_TOOL_REMINDER", "1")
	defer os.Unsetenv("M365_INJECT_TOOL_REMINDER")
	msgs := []oaiMsg{{Role: "user", Content: "hello"}}
	if out := injectToolReminder(msgs, nil); len(out) != len(msgs) {
		t.Fatalf("nil tools: len=%d want %d", len(out), len(msgs))
	}
	// tools that all fail to parse produce no injection
	bad := []chathub.Tool{{Type: "function", Function: json.RawMessage(`garbage`)}}
	if out := injectToolReminder(msgs, bad); len(out) != len(msgs) {
		t.Fatalf("bad tools: len=%d want %d", len(out), len(msgs))
	}
}

func TestToolReminderEnabledParsing(t *testing.T) {
	// envBoolDefault is the underlying boolean parser; test it directly by
	// setting the named variable.
	const name = "M365_ENVBOOL_TEST"
	cases := map[string]bool{
		"":      true,
		"1":     true,
		"true":  true,
		"yEs":   true,
		"on":    true,
		"0":     false,
		"off":   false,
		"no":    false,
		"FALSE": false,
		"junk":  true, // unrecognized → fallback
	}
	for raw, want := range cases {
		os.Setenv(name, raw)
		if got := envBoolDefault(name, true); got != want {
			t.Fatalf("envBoolDefault(%q): got=%v want=%v", raw, got, want)
		}
	}
	os.Unsetenv(name)
	// toolReminderEnabled: explicit env wins.
	os.Setenv("M365_INJECT_TOOL_REMINDER", "0")
	if got := toolReminderEnabled(); got {
		t.Fatal("toolReminderEnabled=true want=false when env=0")
	}
	os.Setenv("M365_INJECT_TOOL_REMINDER", "1")
	if got := toolReminderEnabled(); !got {
		t.Fatal("toolReminderEnabled=false want=true when env=1")
	}
	os.Unsetenv("M365_INJECT_TOOL_REMINDER")
}
