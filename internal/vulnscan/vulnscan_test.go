package vulnscan

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/joeylking/repo-steward/internal/toolchain"
	"github.com/joeylking/repo-steward/internal/vuln"
)

// fakeScanners replaces InspectBinary for the test: a file's identity is
// its digest plus the build information of the pin, as long as its
// contents start with "scanner". Anything else is not a scanner.
func fakeScanners(t *testing.T, pin vuln.ScannerPin, goVersion string) {
	t.Helper()
	orig := inspectBinary
	t.Cleanup(func() { inspectBinary = orig })
	inspectBinary = func(p string) (vuln.ScannerIdentity, error) {
		b, err := os.ReadFile(p)
		if err != nil {
			return vuln.ScannerIdentity{}, err
		}
		sum := sha256.Sum256(b)
		id := vuln.ScannerIdentity{SHA256: hex.EncodeToString(sum[:])}
		if !strings.HasPrefix(string(b), "scanner") {
			return id, errors.New("not a scanner")
		}
		id.Version, id.ModuleSum, id.GoVersion, id.GOOS, id.GOARCH = pin.Version, pin.ModuleSum, goVersion, "linux", "arm64"
		return id, nil
	}
}

type fakeBuild struct {
	t     *testing.T
	calls int
	body  string
}

func (f *fakeBuild) build() (string, error) {
	f.calls++
	p := filepath.Join(f.t.TempDir(), "govulncheck")
	return p, os.WriteFile(p, []byte(f.body), 0o755)
}

func setup(t *testing.T) (string, toolchain.Image, vuln.ScannerPin, string) {
	img := toolchain.Table[0]
	pin, err := vuln.ScannerFor(img.GoMinor)
	if err != nil {
		t.Fatal(err)
	}
	const goVersion = "go1.22.12"
	fakeScanners(t, pin, goVersion)
	return filepath.Join(t.TempDir(), "tools"), img, pin, goVersion
}

// The first call builds and persists, later calls reuse without building,
// and the persisted copy is the operator's: read-only and outside any
// build directory.
func TestEnsureScanner_BuildsOnceThenReuses(t *testing.T) {
	root, img, pin, gv := setup(t)
	fb := &fakeBuild{t: t, body: "scanner v1"}
	s, err := ensureScanner(root, img, pin, gv, fb.build)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Built || fb.calls != 1 || s.Dir != filepath.Join(root, ScannerDirName(img, pin)) || s.Path != filepath.Join(s.Dir, "govulncheck") {
		t.Fatalf("first call: %+v, %d builds", s, fb.calls)
	}
	if !strings.Contains(s.Dir, strings.TrimPrefix(img.Digest, "sha256:")) || !strings.Contains(s.Dir, pin.Version) {
		t.Fatalf("directory %s is not keyed by image digest and version", s.Dir)
	}
	if fi, err := os.Stat(s.Path); err != nil || fi.Mode().Perm() != 0o555 {
		t.Fatalf("binary mode %v %v, want 0555", fi, err)
	}
	if fi, err := os.Stat(root); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("root mode %v %v, want 0700", fi, err)
	}
	if entries, _ := os.ReadDir(root); len(entries) != 1 {
		t.Fatalf("root holds %v, want only the scanner directory", entries)
	}
	again, err := ensureScanner(root, img, pin, gv, fb.build)
	if err != nil {
		t.Fatal(err)
	}
	if again.Built || fb.calls != 1 || again.ID != s.ID {
		t.Fatalf("second call: %+v, %d builds", again, fb.calls)
	}
}

// A persisted scanner that no longer matches is an error, never a rebuild.
func TestEnsureScanner_MismatchIsRefusedNotRebuilt(t *testing.T) {
	for name, tc := range map[string]struct {
		tamper    func(t *testing.T, s *Scanner)
		goVersion string
	}{
		"binary changed": {tamper: func(t *testing.T, s *Scanner) {
			os.Chmod(s.Path, 0o755)
			if err := os.WriteFile(s.Path, []byte("scanner v2"), 0o755); err != nil {
				t.Fatal(err)
			}
		}},
		"binary not a scanner": {tamper: func(t *testing.T, s *Scanner) {
			os.Chmod(s.Path, 0o755)
			os.WriteFile(s.Path, []byte("#!/bin/sh\n"), 0o755)
		}},
		"binary removed": {tamper: func(t *testing.T, s *Scanner) { os.Remove(s.Path) }},
		"record removed": {tamper: func(t *testing.T, s *Scanner) { os.Remove(filepath.Join(s.Dir, "scanner.json")) }},
		"record for another image": {tamper: func(t *testing.T, s *Scanner) {
			p := filepath.Join(s.Dir, "scanner.json")
			b, _ := os.ReadFile(p)
			os.Chmod(p, 0o644)
			os.WriteFile(p, []byte(strings.Replace(string(b), toolchain.Table[0].Digest, toolchain.Table[1].Digest, 1)), 0o644)
		}},
		"image go version changed": {goVersion: "go1.22.13"},
	} {
		t.Run(name, func(t *testing.T) {
			root, img, pin, gv := setup(t)
			fb := &fakeBuild{t: t, body: "scanner v1"}
			s, err := ensureScanner(root, img, pin, gv, fb.build)
			if err != nil {
				t.Fatal(err)
			}
			if tc.tamper != nil {
				tc.tamper(t, s)
			}
			if tc.goVersion != "" {
				gv = tc.goVersion
			}
			_, err = ensureScanner(root, img, pin, gv, fb.build)
			if err == nil || fb.calls != 1 {
				t.Fatalf("err = %v after %d builds, want a refusal and no rebuild", err, fb.calls)
			}
			if name != "record removed" && name != "binary removed" && !errors.Is(err, vuln.ErrScannerMismatch) {
				t.Fatalf("err = %v, want ErrScannerMismatch", err)
			}
			if !strings.Contains(err.Error(), s.Dir) {
				t.Fatalf("err = %v does not name the directory to remove", err)
			}
		})
	}
}

// A build that produces something other than the pinned scanner leaves
// nothing behind to be reused.
func TestEnsureScanner_BadBuildPersistsNothing(t *testing.T) {
	root, img, pin, gv := setup(t)
	fb := &fakeBuild{t: t, body: "not it"}
	if _, err := ensureScanner(root, img, pin, gv, fb.build); err == nil {
		t.Fatal("accepted a build that is not a scanner")
	}
	if _, err := ensureScanner(root, img, pin, "go1.23.1", (&fakeBuild{t: t, body: "scanner"}).build); !errors.Is(err, vuln.ErrScannerMismatch) {
		t.Fatalf("err = %v, want a pin mismatch for another Go version", err)
	}
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Fatalf("root holds %v after failed builds", entries)
	}
	if _, err := ensureScanner("tools", img, pin, gv, fb.build); err == nil {
		t.Fatal("accepted a relative root")
	}
}

// A supplied database is identified and checked for consistency; anything
// outside the layout is refused before it could be mounted.
func TestLocalDatabase(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "db")
	if err := vuln.WriteFixtureDB(dir); err != nil {
		t.Fatal(err)
	}
	db, err := LocalDatabase(dir)
	if err != nil {
		t.Fatal(err)
	}
	if db.Source != "local" || !db.ID.Modified.Equal(vuln.FixtureTime) || !strings.HasPrefix(db.ID.Hash, "sha256:") {
		t.Fatalf("db = %+v", db)
	}
	if err := vuln.Verify(db.Dir, db.ID, vuln.Limits{}); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "ID", "GO-TEST-0001.json"), []byte(`{"id":"GO-TEST-0001","tampered":true}`), 0o644)
	if err := vuln.Verify(db.Dir, db.ID, vuln.Limits{}); !errors.Is(err, vuln.ErrSnapshotMismatch) {
		t.Fatalf("tampered database verified: %v", err)
	}
	os.WriteFile(filepath.Join(dir, "stray"), []byte("x"), 0o644)
	if _, err := LocalDatabase(dir); err == nil {
		t.Fatal("accepted a directory with a stray file")
	}
	missing := filepath.Join(t.TempDir(), "db")
	os.MkdirAll(filepath.Join(missing, "ID"), 0o755)
	os.WriteFile(filepath.Join(missing, "ID", "GO-X-1.json"), []byte(`{"id":"GO-X-1"}`), 0o644)
	if _, err := LocalDatabase(missing); err == nil {
		t.Fatal("accepted a directory without indexes")
	}
}
