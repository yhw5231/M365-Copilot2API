package auth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	return s, dir
}

func upsertAccount(t *testing.T, s *Store, id, email string) AccountToken {
	t.Helper()
	acc, err := s.Upsert(TokenSet{
		AccessToken:  "access-" + id,
		RefreshToken: "refresh-" + id,
		Email:        email,
		HomeOID:      id,
		ExpiresAt:    time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("upsert %s: %v", id, err)
	}
	return acc
}

// TestPerAccountFileLayout proves the on-disk layout: exactly one
// <account>.json per account holding credentials and per-account settings.
func TestPerAccountFileLayout(t *testing.T) {
	s, dir := newTestStore(t)
	upsertAccount(t, s, "oid-1", "one@example.com")

	tokenPath := filepath.Join(dir, "one@example.com.json")
	tokenRaw, err := os.ReadFile(tokenPath)
	if err != nil {
		t.Fatalf("token file missing: %v", err)
	}
	var tokenDoc map[string]any
	if err := json.Unmarshal(tokenRaw, &tokenDoc); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"id", "email", "accessToken", "refreshToken", "expiresAt"} {
		if _, ok := tokenDoc[key]; !ok {
			t.Fatalf("token file missing %q: %s", key, tokenRaw)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "one@example.com"+settingsSuffix)); !os.IsNotExist(err) {
		t.Fatalf("per-account settings file must not exist: %v", err)
	}
}

// TestAccountSettingsPersistedInAccountFile: scheduling toggle and bound proxy
// are part of the account file and survive a restart.
func TestAccountSettingsPersistedInAccountFile(t *testing.T) {
	s, dir := newTestStore(t)
	upsertAccount(t, s, "oid-1", "one@example.com")
	if err := s.SetScheduleEnabled("oid-1", false); err != nil {
		t.Fatal(err)
	}
	if err := s.SetBoundProxy("oid-1", "http://127.0.0.1:8080"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "one@example.com.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"scheduleDisabled": true`) || !strings.Contains(string(raw), "http://127.0.0.1:8080") {
		t.Fatalf("account file missing per-account settings: %s", raw)
	}

	reopened, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.ScheduleEnabled("oid-1") {
		t.Fatal("scheduleDisabled was not persisted")
	}
	acc, ok := reopened.Get("oid-1")
	if !ok {
		t.Fatal("account missing after restart")
	}
	if acc.BoundProxy != "http://127.0.0.1:8080" {
		t.Fatalf("boundProxy was not persisted: %+v", acc)
	}
}

// TestInterimSettingsFileMergedAndRemoved: files written by the intermediate
// build (<name>.json + <name>.settings.json) are merged into the account file
// and the separate settings file is removed on load.
func TestInterimSettingsFileMergedAndRemoved(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	token := `{"id": "oid-1", "email": "one@example.com", "status": "online", "accessToken": "a1", "refreshToken": "r1", "expiresAt": "2026-01-01T00:00:00Z"}`
	settings := `{"scheduleDisabled": true, "boundProxy": "http://127.0.0.1:9090"}`
	if err := os.WriteFile(filepath.Join(dir, "one@example.com.json"), []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "one@example.com"+settingsSuffix), []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}

	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s.ScheduleEnabled("oid-1") {
		t.Fatal("scheduleDisabled from interim settings file lost")
	}
	if acc, _ := s.Get("oid-1"); acc.BoundProxy != "http://127.0.0.1:9090" {
		t.Fatalf("boundProxy from interim settings file lost: %+v", acc)
	}
	if _, err := os.Stat(filepath.Join(dir, "one@example.com"+settingsSuffix)); !os.IsNotExist(err) {
		t.Fatalf("interim settings file was not removed: %v", err)
	}
	// The merged values must now live in the account file itself.
	reopened, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.ScheduleEnabled("oid-1") {
		t.Fatal("merged settings did not survive a restart")
	}
}

// TestLegacyMigrationImportsAndSeals: an old single-file accounts.json is
// imported into per-account files and renamed out of the way, so removed
// accounts cannot resurrect from it.
func TestLegacyMigrationImportsAndSeals(t *testing.T) {
	dir := t.TempDir()
	body := `{"accounts": [
		{"id": "oid-1", "email": "one@example.com", "status": "online", "accessToken": "a1", "refreshToken": "r1", "expiresAt": "2026-01-01T00:00:00Z"},
		{"id": "oid-2", "email": "two@example.com", "status": "online", "accessToken": "a2", "refreshToken": "r2", "expiresAt": "2026-01-01T00:00:00Z"}
	]}`
	legacy := filepath.Join(dir, "accounts.json")
	if err := os.WriteFile(legacy, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	accountsDir := filepath.Join(dir, "accounts")
	t.Setenv("M365_DATA_DIR", dir)
	t.Setenv("M365_ACCOUNTS_DIR", accountsDir)

	s, err := OpenStore("")
	if err != nil {
		t.Fatal(err)
	}
	if got := len(s.List()); got != 2 {
		t.Fatalf("expected 2 imported accounts, got %d", got)
	}
	for _, name := range []string{"one@example.com.json", "two@example.com.json"} {
		if _, err := os.Stat(filepath.Join(accountsDir, name)); err != nil {
			t.Fatalf("per-account file missing: %v", err)
		}
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatalf("legacy file was not sealed: %v", err)
	}
	if _, err := os.Stat(legacy + ".migrated"); err != nil {
		t.Fatalf("sealed legacy file missing: %v", err)
	}

	// Reopening must not re-import (file is gone) and must keep state.
	if err := s.SetScheduleEnabled("oid-1", false); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenStore(accountsDir)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(reopened.List()); got != 2 {
		t.Fatalf("expected 2 accounts after restart, got %d", got)
	}
	if reopened.ScheduleEnabled("oid-1") {
		t.Fatal("settings lost after restart")
	}
}

// TestLegacyMigrationResumesAfterInterruption: if a previous migration only
// wrote some accounts before dying, the remaining ones are merged in on the
// next start instead of being lost.
func TestLegacyMigrationResumesAfterInterruption(t *testing.T) {
	dir := t.TempDir()
	body := `{"accounts": [
		{"id": "oid-1", "email": "one@example.com", "status": "online", "accessToken": "a1", "refreshToken": "r1", "expiresAt": "2026-01-01T00:00:00Z"},
		{"id": "oid-2", "email": "two@example.com", "status": "online", "accessToken": "a2", "refreshToken": "r2", "expiresAt": "2026-01-01T00:00:00Z"}
	]}`
	legacy := filepath.Join(dir, "accounts.json")
	if err := os.WriteFile(legacy, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	accountsDir := filepath.Join(dir, "accounts")
	t.Setenv("M365_DATA_DIR", dir)
	t.Setenv("M365_ACCOUNTS_DIR", accountsDir)

	// Simulate an interrupted migration: only oid-1 ever made it to disk.
	if err := os.MkdirAll(accountsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	partial := `{"id": "oid-1", "email": "one@example.com", "status": "online", "accessToken": "a1", "refreshToken": "r1", "expiresAt": "2026-01-01T00:00:00Z"}`
	if err := os.WriteFile(filepath.Join(accountsDir, "one@example.com.json"), []byte(partial), 0o600); err != nil {
		t.Fatal(err)
	}

	s, err := OpenStore("")
	if err != nil {
		t.Fatal(err)
	}
	if got := len(s.List()); got != 2 {
		t.Fatalf("expected migration resume to import oid-2, got %d accounts", got)
	}
	if _, err := os.Stat(filepath.Join(accountsDir, "two@example.com.json")); err != nil {
		t.Fatalf("missing account was not imported: %v", err)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatalf("legacy file was not sealed after resume: %v", err)
	}
}

// TestEmailChangeRenamesFiles: when an account's email changes, the file saved
// under the old name is removed so exactly one file remains.
func TestEmailChangeRenamesFiles(t *testing.T) {
	s, dir := newTestStore(t)
	upsertAccount(t, s, "oid-1", "old@example.com")

	if _, err := s.Upsert(TokenSet{
		AccessToken: "access-2", RefreshToken: "refresh-2",
		Email: "new@example.com", HomeOID: "oid-1", ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "old@example.com.json")); !os.IsNotExist(err) {
		t.Fatalf("old token file still present: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "new@example.com.json")); err != nil {
		t.Fatalf("new token file missing: %v", err)
	}
}

// TestDeleteRemovesFiles proves Delete also removes the on-disk file.
func TestDeleteRemovesFiles(t *testing.T) {
	s, dir := newTestStore(t)
	upsertAccount(t, s, "oid-1", "one@example.com")
	if err := s.Delete("oid-1"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Get("oid-1"); ok {
		t.Fatal("account still in memory")
	}
	if _, err := os.Stat(filepath.Join(dir, "one@example.com.json")); !os.IsNotExist(err) {
		t.Fatalf("token file still present: %v", err)
	}
}

// TestSanitizeFileName covers unsafe and reserved names.
func TestSanitizeFileName(t *testing.T) {
	cases := map[string]string{
		`a:b\c/d*e?f"g<h>i|j`: "a_b_c_d_e_f_g_h_i_j",
		"  spaced  ":          "spaced",
		"con":                 "_con",
		"COM1":                "_COM1",
		"nul.txt":             "_nul.txt",
		"u@example.com":       "u@example.com",
	}
	for in, want := range cases {
		if got := sanitizeFileName(in); got != want {
			t.Fatalf("sanitizeFileName(%q)=%q, want %q", in, got, want)
		}
	}
	if got := sanitizeFileName("\x00\x1f"); got != "account" {
		t.Fatalf("sanitizeFileName(control chars)=%q", got)
	}
}
