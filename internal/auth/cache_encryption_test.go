package auth

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testBase64Key = "ab"

func testKey() string { return strings.Repeat(testBase64Key, 32) } // 64 hex chars = 32 bytes

// writePlainLegacyCache creates a legacy single-file accounts.json with a
// plaintext account, the store format used before per-account files.
func writePlainLegacyCache(t *testing.T, path string) {
	t.Helper()
	body := `{
  "accounts": [
    {
      "id": "acc-1",
      "email": "u@example.com",
      "status": "online",
      "accessToken": "PLAIN-ACCESS-TOKEN-123",
      "refreshToken": "PLAIN-REFRESH-TOKEN-456",
      "expiresAt": "2026-01-01T00:00:00Z",
      "updatedAt": "2026-01-01T00:00:00Z"
    }
  ]
}`
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}

func accountTokenPath(dir, email string) string {
	return filepath.Join(dir, sanitizeFileName(email)+".json")
}

// TestTokenEncryptionAtRest proves that with M365_TOKEN_ENC_KEY set, the
// on-disk per-account file never contains plaintext tokens while in-memory
// access keeps working.
func TestTokenEncryptionAtRest(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("M365_TOKEN_ENC_KEY", testKey())
	writePlainLegacyCache(t, filepath.Join(dir, "accounts.json"))
	t.Setenv("M365_DATA_DIR", dir)
	t.Setenv("M365_ACCOUNTS_DIR", filepath.Join(dir, "accounts"))

	s, err := OpenStore("")
	if err != nil {
		t.Fatal(err)
	}
	acc, ok := s.Get("acc-1")
	if !ok {
		t.Fatal("account not found after legacy import")
	}
	if acc.AccessToken != "PLAIN-ACCESS-TOKEN-123" || acc.RefreshToken != "PLAIN-REFRESH-TOKEN-456" {
		t.Fatalf("in-memory tokens wrong: %+v", acc)
	}
	// Force a save (e.g. a rotation).
	if err := s.UpdateRefreshToken("acc-1", "NEW-REFRESH-TOKEN-789"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(accountTokenPath(filepath.Join(dir, "accounts"), "u@example.com"))
	if err != nil {
		t.Fatal(err)
	}
	disk := string(b)
	if strings.Contains(disk, "PLAIN-ACCESS-TOKEN-123") || strings.Contains(disk, "NEW-REFRESH-TOKEN-789") || strings.Contains(disk, "PLAIN-REFRESH-TOKEN-456") {
		t.Fatalf("plaintext token on disk: %s", disk)
	}
	if !strings.Contains(disk, "enc:v1:") {
		t.Fatalf("no encrypted token marker on disk: %s", disk)
	}
}

// TestTokenPlaintextImportRewritesEncrypted: importing a legacy plaintext
// cache with a key configured writes the per-account files encrypted without
// losing account data.
func TestTokenPlaintextImportRewritesEncrypted(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("M365_TOKEN_ENC_KEY", testKey())
	writePlainLegacyCache(t, filepath.Join(dir, "accounts.json"))
	accountsDir := filepath.Join(dir, "accounts")
	t.Setenv("M365_DATA_DIR", dir)
	t.Setenv("M365_ACCOUNTS_DIR", accountsDir)

	if _, err := OpenStore(""); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(accountTokenPath(accountsDir, "u@example.com"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "enc:v1:") {
		t.Fatalf("legacy account was not migrated to encrypted storage: %s", b)
	}
}

// TestTokenEncryptionWrongKeyFailsClosed: opening encrypted account files with
// the wrong key must fail instead of silently returning unusable tokens.
func TestTokenEncryptionWrongKeyFailsClosed(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("M365_TOKEN_ENC_KEY", testKey())
	writePlainLegacyCache(t, filepath.Join(dir, "accounts.json"))
	accountsDir := filepath.Join(dir, "accounts")
	t.Setenv("M365_DATA_DIR", dir)
	t.Setenv("M365_ACCOUNTS_DIR", accountsDir)
	s, err := OpenStore("")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateRefreshToken("acc-1", "ROTATED-TOKEN"); err != nil {
		t.Fatal(err)
	}

	t.Setenv("M365_TOKEN_ENC_KEY", strings.Repeat("cd", 32))
	if _, err := OpenStore(accountsDir); err == nil {
		t.Fatal("OpenStore with wrong key must fail")
	}
}

// TestNoKeyKeepsLegacyPlaintextBehavior: without M365_TOKEN_ENC_KEY the store
// still reads and writes plaintext so existing deployments keep working.
func TestNoKeyKeepsLegacyPlaintextBehavior(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("M365_TOKEN_ENC_KEY", "")
	writePlainLegacyCache(t, filepath.Join(dir, "accounts.json"))
	accountsDir := filepath.Join(dir, "accounts")
	t.Setenv("M365_DATA_DIR", dir)
	t.Setenv("M365_ACCOUNTS_DIR", accountsDir)
	s, err := OpenStore("")
	if err != nil {
		t.Fatal(err)
	}
	acc, ok := s.Get("acc-1")
	if !ok || acc.AccessToken != "PLAIN-ACCESS-TOKEN-123" {
		t.Fatalf("legacy plaintext read failed: %+v", acc)
	}
	if err := s.UpdateRefreshToken("acc-1", "ANOTHER-PLAIN"); err != nil {
		t.Fatal(err)
	}
	disk, err := os.ReadFile(accountTokenPath(accountsDir, "u@example.com"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(disk), "ANOTHER-PLAIN") {
		t.Fatalf("plaintext mode should keep plaintext: %s", disk)
	}
}

// TestMalformedKeyRejected ensures a bad M365_TOKEN_ENC_KEY is a hard error.
func TestMalformedKeyRejected(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("M365_TOKEN_ENC_KEY", "not-hex")
	writePlainLegacyCache(t, filepath.Join(dir, "accounts.json"))
	t.Setenv("M365_DATA_DIR", dir)
	t.Setenv("M365_ACCOUNTS_DIR", filepath.Join(dir, "accounts"))
	if _, err := OpenStore(""); err == nil {
		t.Fatal("malformed encryption key must be rejected")
	}
}
