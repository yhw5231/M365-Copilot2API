package storage

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExplicitPathOverridesDataDir(t *testing.T) {
	dataDir := t.TempDir()
	explicit := filepath.Join(t.TempDir(), "accounts.json")
	t.Setenv("M365_DATA_DIR", dataDir)
	t.Setenv("M365_CONFIG", explicit)

	if got := Path("M365_CONFIG", "accounts.json"); got != explicit {
		t.Fatalf("Path()=%q, want explicit path %q", got, explicit)
	}
}

func TestWriteFileAtomicCreatesParentAndReplaces(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "settings.json")
	if err := WriteFileAtomic(path, []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(path, []byte("second"), 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "second" {
		t.Fatalf("file contents=%q", b)
	}
	leftovers, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".m365-copilot2api-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(leftovers) != 0 {
		t.Fatalf("temporary files left behind: %v", leftovers)
	}
}
