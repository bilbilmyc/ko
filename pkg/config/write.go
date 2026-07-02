package config

import (
	"fmt"
	"os"
	"path/filepath"
)

// WriteAtomic writes data to path by first writing to a temp file in the
// same directory and then renaming it into place. Guarantees the target
// path is either fully written or untouched (no half-written file on
// crash / disk-full).
//
// Refuses to overwrite unless overwrite=true. Returns the bytes written
// on success.
func WriteAtomic(path string, data []byte, overwrite bool) (int, error) {
	if !overwrite {
		if _, err := os.Stat(path); err == nil {
			return 0, fmt.Errorf("refusing to overwrite existing file %q (use --force)", path)
		} else if !os.IsNotExist(err) {
			return 0, fmt.Errorf("stat %q: %w", path, err)
		}
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, fmt.Errorf("mkdir %q: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".ko-clusterfile-*.tmp")
	if err != nil {
		return 0, fmt.Errorf("create temp: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() {
		_ = os.Remove(tmpName) // best-effort
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return 0, fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return 0, fmt.Errorf("fsync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return 0, fmt.Errorf("close temp: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return 0, fmt.Errorf("rename temp: %w", err)
	}
	// set mode 0644 so non-root users can read
	_ = os.Chmod(path, 0o644)
	return len(data), nil
}
