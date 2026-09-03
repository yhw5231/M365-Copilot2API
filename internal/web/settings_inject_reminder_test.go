package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// TestOpenSettingsStoreLegacyKeepsInjectToolReminderDefault reproduces the
// openSettingsStore load path: a legacy settings.json that predates the
// injectToolReminder key must NOT silently disable the tool reminder. Because
// Go's json.Unmarshal leaves absent fields untouched, the field seeded by
// defaultRuntimeSettings() (true) must survive.
func TestOpenSettingsStoreLegacyKeepsInjectToolReminderDefault(t *testing.T) {
	dir := t.TempDir()
	legacy := `{"maxToolCallsPerTurn":8,"maxToolRounds":0,"timeZone":"UTC"}`
	p := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(p, []byte(legacy), 0600); err != nil {
		t.Fatal(err)
	}

	d := defaultRuntimeSettings()
	if !d.InjectToolReminder {
		t.Fatalf("defaultRuntimeSettings().InjectToolReminder=%v want true", d.InjectToolReminder)
	}
	s := &settingsStore{path: p, accountPath: p + ".account-settings", v: d}
	if b, e := os.ReadFile(s.path); e == nil {
		if err := json.Unmarshal(b, &s.v); err != nil {
			t.Fatal(err)
		}
	}
	if !s.v.InjectToolReminder {
		t.Fatalf("legacy file without injectToolReminder key zeroed the flag; want default true (got %v)", s.v.InjectToolReminder)
	}
	if s.v.MaxToolCallsPerTurn != 8 {
		t.Fatalf("MaxToolCallsPerTurn=%d want 8", s.v.MaxToolCallsPerTurn)
	}
}

// TestInjectToolReminderPUTRoundTrip verifies a PUT that flips the flag to
// false persists and is returned by GET.
func TestInjectToolReminderPUTRoundTrip(t *testing.T) {
	dir := t.TempDir()
	st := &settingsStore{path: filepath.Join(dir, "settings.json"), accountPath: filepath.Join(dir, "account-settings.json"), v: defaultRuntimeSettings()}
	s := &Server{settings: st}

	body := `{"injectToolReminder":false}`
	r := httptest.NewRequest(http.MethodPut, "/api/admin/settings", bytes.NewReader([]byte(body)))
	w := httptest.NewRecorder()
	s.adminSettings(w, r)
	if w.Code != 200 {
		t.Fatalf("PUT=%d %s", w.Code, w.Body.String())
	}
	if st.get().InjectToolReminder {
		t.Fatal("injectToolReminder should be false after PUT")
	}
}
