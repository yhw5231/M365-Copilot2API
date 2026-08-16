package web

import (
	"os"

	"m365-copilot2api/internal/storage"
)

// writeFileAtomic persists b to path via a temp file + rename so a crash
// mid-write can never leave a truncated store file that fails to load.
func writeFileAtomic(path string, b []byte, perm os.FileMode) error {
	return storage.WriteFileAtomic(path, b, perm)
}
