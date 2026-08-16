package storage

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const applicationDir = "m365-copilot2api"

// EnvPath returns a configured path after trimming accidental whitespace.
// Empty values are treated as unset so compose files can safely pass an empty
// optional variable.
func EnvPath(name string) string {
	p := strings.TrimSpace(os.Getenv(name))
	if p == "" {
		return ""
	}
	p = os.ExpandEnv(p)
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		if home, err := os.UserHomeDir(); err == nil && strings.TrimSpace(home) != "" {
			p = filepath.Join(home, strings.TrimLeft(p[1:], `/\`))
		}
	}
	return filepath.Clean(p)
}

// DataDir returns the default writable directory for runtime state. The
// process working directory is deliberately not used: service managers and
// containers commonly start a binary from a read-only installation directory.
func DataDir() string {
	if dir := EnvPath("M365_DATA_DIR"); dir != "" {
		return dir
	}
	if home, err := os.UserHomeDir(); err == nil && strings.TrimSpace(home) != "" {
		// Keep the pre-existing location when upgrading from versions that used
		// ~/.config on every platform.
		legacy := filepath.Join(home, ".config", applicationDir)
		if _, err := os.Stat(legacy); err == nil {
			return legacy
		}
	}
	if dir, err := os.UserConfigDir(); err == nil && strings.TrimSpace(dir) != "" {
		return filepath.Join(dir, applicationDir)
	}
	if home, err := os.UserHomeDir(); err == nil && strings.TrimSpace(home) != "" {
		return filepath.Join(home, ".config", applicationDir)
	}
	if dir, err := os.UserCacheDir(); err == nil && strings.TrimSpace(dir) != "" {
		return filepath.Join(dir, applicationDir)
	}
	return filepath.Join(os.TempDir(), applicationDir)
}

// Path returns an explicitly configured file path or a file below DataDir.
func Path(envName, defaultName string) string {
	if envName != "" {
		if p := EnvPath(envName); p != "" {
			return p
		}
	}
	return filepath.Join(DataDir(), defaultName)
}

// EnsureParent creates the parent directory for a runtime file.
func EnsureParent(path string) error {
	dir := filepath.Dir(path)
	if dir == "" {
		dir = "."
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create runtime data directory %q: %w", dir, err)
	}
	return nil
}

// WriteFileAtomic writes a private runtime file through a temporary file in
// the same directory, then replaces the destination. The temporary filename
// is unique so concurrent stores and stale files from a crashed process do
// not collide.
func WriteFileAtomic(path string, b []byte, perm os.FileMode) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("empty runtime file path")
	}
	if err := EnsureParent(path); err != nil {
		return err
	}
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".m365-copilot2api-*")
	if err != nil {
		return fmt.Errorf("create temporary runtime file: %w", err)
	}
	tmp := f.Name()
	removeTmp := true
	defer func() {
		_ = f.Close()
		if removeTmp {
			_ = os.Remove(tmp)
		}
	}()
	if err := f.Chmod(perm); err != nil {
		return fmt.Errorf("set runtime file permissions: %w", err)
	}
	if _, err := f.Write(b); err != nil {
		return fmt.Errorf("write temporary runtime file: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync temporary runtime file: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close temporary runtime file: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("replace runtime file %q: %w", path, err)
	}
	removeTmp = false
	return nil
}
