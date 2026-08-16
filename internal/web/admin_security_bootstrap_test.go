package web

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnvPasswordOverridesLeftoverDefaultPersistedFile(t *testing.T) {
	dir := t.TempDir()
	persisted := filepath.Join(dir, "data", "admin-password")
	if err := os.MkdirAll(filepath.Dir(persisted), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(persisted, []byte("admin123\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("M365_DATA_DIR", "")
	t.Setenv("M365_ADMIN_PASSWORD_FILE", persisted)
	t.Setenv("M365_ADMIN_PASSWORD_BOOTSTRAP_FILE", "")
	t.Setenv("M365_ADMIN_PASSWORD", "custom-password")

	got, mustChange := loadAdminPassword()
	if got != "custom-password" || mustChange {
		t.Fatalf("loadAdminPassword()=(%q,%v)", got, mustChange)
	}
	b, err := os.ReadFile(persisted)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "custom-password\n" {
		t.Fatalf("env password not persisted: %q", b)
	}
}

func TestBootstrapPasswordUsesWritablePersistentPath(t *testing.T) {
	dir := t.TempDir()
	persisted := filepath.Join(dir, "data", "admin-password")
	bootstrap := filepath.Join(dir, "secret")
	if err := os.WriteFile(bootstrap, []byte("bootstrap-password\n"), 0400); err != nil {
		t.Fatal(err)
	}
	t.Setenv("M365_ADMIN_PASSWORD_FILE", persisted)
	t.Setenv("M365_ADMIN_PASSWORD_BOOTSTRAP_FILE", bootstrap)
	t.Setenv("M365_ADMIN_PASSWORD", "")

	got, mustChange := loadAdminPassword()
	if got != "bootstrap-password" || mustChange {
		t.Fatalf("loadAdminPassword()=(%q,%v)", got, mustChange)
	}
	if err := saveAdminPassword("a-new-password-123"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(persisted)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "a-new-password-123\n" {
		t.Fatalf("persisted password=%q", b)
	}
}

func TestExplicitPasswordFileOverridesDataDir(t *testing.T) {
	dataDir := t.TempDir()
	explicit := filepath.Join(t.TempDir(), "admin-password")
	if err := os.WriteFile(filepath.Join(dataDir, "admin-password"), []byte("data-password\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(explicit, []byte("explicit-password\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("M365_DATA_DIR", dataDir)
	t.Setenv("M365_ADMIN_PASSWORD_FILE", explicit)
	t.Setenv("M365_ADMIN_PASSWORD", "")
	t.Setenv("M365_ADMIN_PASSWORD_BOOTSTRAP_FILE", "")

	got, mustChange := loadAdminPassword()
	if got != "explicit-password" || mustChange {
		t.Fatalf("loadAdminPassword()=(%q,%v)", got, mustChange)
	}
}

func TestPasswordSaveReportsUnwritableParent(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(parent, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("M365_ADMIN_PASSWORD_FILE", filepath.Join(parent, "admin-password"))
	if err := saveAdminPassword("a-new-password-123"); err == nil {
		t.Fatal("expected password persistence error")
	}
}
