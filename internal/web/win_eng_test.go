package web

import "testing"

// TestWindow150English verifies that a 150-byte scan window is sufficient for
// English misjudgment detection. English is 1 byte/char, so 150 bytes = 150
// characters — far more room than the 50 汉字 it represents for Chinese. Every
// genuine English misjudgment phrasing (short, medium, and long-form) must be
// caught within the window, and mid-reply English narration must not be
// flagged.
func TestWindow150English(t *testing.T) {
	toolSet := []map[string]any{
		{"type": "function", "function": map[string]any{"name": "pwsh", "description": "PowerShell"}},
		{"type": "function", "function": map[string]any{"name": "read", "description": "read files"}},
		{"type": "function", "function": map[string]any{"name": "write", "description": "write files"}},
		{"type": "function", "function": map[string]any{"name": "edit", "description": "edit files"}},
		{"type": "function", "function": map[string]any{"name": "glob", "description": "glob files"}},
		{"type": "function", "function": map[string]any{"name": "grep", "description": "search files"}},
	}
	// Genuine English misjudgments across lengths: 20-50 chars (typical),
	// 50-100 (verbose), 100-150 (long form). All must be caught at 150 bytes.
	mustCatch := []struct {
		name string
		text string
	}{
		{"short exact", "This session only provides a Linux container."},
		{"short no tools", "I don't have any tools in this session."},
		{"short denial scope", "The tools are not available in this session."},
		{"scope first", "This session does not have any tools available."},
		{"no shell access", "I don't have shell access in this environment."},
		{"exclusivity sandbox", "This environment only provides a Linux sandbox."},
		{"command exec", "I don't have command execution in this session."},
		{"current session linux", "current session only provides a linux container"},
		{"tool-aware read", "The read tool is not available in this environment."},
		{"medium verbose", "I am sorry, but the current session does not have any tools or shell available for me to use right now."},
		{"medium long-form", "Unfortunately, this environment does not provide access to a shell, and I have no command execution tool."},
		{"long form", "I must report that the current session does not provide any of the tools I need — no shell, no command execution, no file operation interface — so I cannot continue."},
		{"long with scope end", "After reviewing the situation, I can confirm that this session does not have any tool available for executing commands, so I cannot proceed."},
		{"long exclusivity", "To be clear, the current environment only provides a Linux sandbox with no Windows execution channel, so I cannot run your command."},
	}
	for _, c := range mustCatch {
		if got := isWorkspaceToolMisjudgment(c.text); !got {
			t.Errorf("150-byte window MISSED (FN) %-22s (%d bytes): %q", c.name, len(c.text), c.text)
		}
	}
	// tool-aware variants of long forms.
	toolAware := []struct {
		name string
		text string
	}{
		{"tool-aware pwsh short", "I don't have pwsh here."},
		{"tool-aware long read", "I would like to help, but the read tool is not available in this environment so I cannot inspect the files."},
		{"tool-aware long pwsh", "Unfortunately, pwsh is not available in this session, so I have no way to run commands."},
		{"tool-aware long list", "The write, edit, glob and grep tools are not available in this session, so I cannot modify the project."},
	}
	for _, c := range toolAware {
		if got := isWorkspaceToolMisjudgmentForTools(c.text, toolSet); !got {
			t.Errorf("150-byte window MISSED tool-aware (FN) %-22s (%d bytes): %q", c.name, len(c.text), c.text)
		}
	}
	// Legitimate English narration that MUST stay clear.
	mustIgnore := []struct {
		name string
		text string
	}{
		{"deploy note", "The service deploys in a Linux container on the production server."},
		{"path note", "Copy the generated file to /mnt/data before returning it to the caller."},
		{"permission", "I don't have permission to write to that directory, so please grant it."},
		{"file locked", "The file cannot be read because it is locked by another process."},
		{"file not found", "The file was not found in the workspace; maybe the path is wrong."},
		{"mid-reply en denial", "I have finished analyzing the project and verified the three files that need changes. Note that the current session does not provide any file operation interface, but this is environment background and does not affect the conclusion."},
		{"mid-reply en tools", "After completing the analysis I want to add that the tools in this session were all usable; the earlier note about no shell was background only."},
	}
	for _, c := range mustIgnore {
		if got := isWorkspaceToolMisjudgment(c.text); got {
			t.Errorf("150-byte window FLAGGED (FP) %-22s (%d bytes): %q", c.name, len(c.text), c.text)
		}
	}
	// Report lengths for visibility.
	for _, c := range mustCatch {
		t.Logf("caught  %-22s %d bytes", c.name, len(c.text))
	}
	for _, c := range mustIgnore {
		t.Logf("ignored %-22s %d bytes", c.name, len(c.text))
	}
}
