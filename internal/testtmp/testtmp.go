// Package testtmp provides temporary directories that container engines can
// bind-mount. Go's default temp directory on macOS lives under /var/folders,
// which VM-backed engines such as colima do not share, so bind mounts of it
// silently become empty directories inside the VM.
package testtmp

import (
	"os"
	"path/filepath"
	"testing"
)

// Root returns the directory under which shareable temp dirs are created:
// REPO_STEWARD_TEST_ROOT if set, otherwise $HOME/.cache/repo-steward-test.
func Root() string {
	if r := os.Getenv("REPO_STEWARD_TEST_ROOT"); r != "" {
		return r
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return os.TempDir()
	}
	return filepath.Join(home, ".cache", "repo-steward-test")
}

// Dir creates a fresh directory under Root and removes it when the test ends.
func Dir(t *testing.T) string {
	t.Helper()
	root := Root()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	d, err := os.MkdirTemp(root, "t-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { RemoveAll(d) })
	return d
}

// RemoveAll deletes a directory tree that may contain a Go module cache,
// whose files and directories the toolchain creates read-only.
func RemoveAll(dir string) error {
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil {
			if d.IsDir() {
				os.Chmod(p, 0o755)
			} else {
				os.Chmod(p, 0o644)
			}
		}
		return nil
	})
	return os.RemoveAll(dir)
}
