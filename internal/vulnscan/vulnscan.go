// Package vulnscan runs the pinned vulnerability scanner in the sandbox. It
// provisions the scanner and the database on the host, verifies both
// immediately before every scan, and hands the captured output to
// vuln.Parse. internal/vuln stays free of anything that runs containers.
//
// The scanner is built once per toolchain image digest and pinned version
// in the sandbox's tool profile, which reaches only the public module proxy
// and checksum database, and the host copies the binary out into
// <root>/govulncheck-<version>-<image digest>/ next to a record of its
// identity. That directory is never rebuilt: a binary that no longer
// matches its record or its pin is an error the operator resolves by
// removing the directory.
package vulnscan

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/joeylking/repo-steward/internal/sandbox"
	"github.com/joeylking/repo-steward/internal/toolchain"
	"github.com/joeylking/repo-steward/internal/vuln"
)

const (
	binaryName = "govulncheck"
	recordName = "scanner.json"
	// ToolsMount and DBMount are where the execute profile sees the
	// scanner directory and the database.
	ToolsMount = "/tools"
	DBMount    = "/vulndb"
	// DBURL is the database as the scanner is told to read it.
	DBURL = "file://" + DBMount
)

// BuildTimeout bounds one scanner build.
const BuildTimeout = 10 * time.Minute

// Scanner is a provisioned scanner on the host.
type Scanner struct {
	// Dir is the directory mounted at /tools; Path is the binary in it.
	Dir  string `json:"dir"`
	Path string `json:"path"`
	// Image is the toolchain image the scanner was built in and runs in.
	Image string          `json:"image"`
	Pin   vuln.ScannerPin `json:"pin"`
	// ID is the identity recorded when the binary was built. Every scan
	// checks the binary against it.
	ID vuln.ScannerIdentity `json:"identity"`
	// Built is true when this call built the scanner.
	Built bool `json:"built"`
}

// record is what scanner.json holds.
type record struct {
	Pin            vuln.ScannerPin      `json:"pin"`
	Image          string               `json:"image"`
	ImageGoVersion string               `json:"image_go_version"`
	Identity       vuln.ScannerIdentity `json:"identity"`
	BuiltAt        time.Time            `json:"built_at"`
}

// inspectBinary is vuln.InspectBinary; unit tests replace it to stand in
// fake binaries for real scanner builds.
var inspectBinary = vuln.InspectBinary

// ScannerDirName is the directory name of the scanner for an image.
func ScannerDirName(img toolchain.Image, pin vuln.ScannerPin) string {
	return "govulncheck-" + pin.Version + "-" + strings.TrimPrefix(img.Digest, "sha256:")
}

// GoVersion reports the image's Go version, such as go1.22.12, from go env
// in the execute profile.
func GoVersion(ctx context.Context, sb *sandbox.Docker) (string, error) {
	esb, err := sb.WithFreshBuildCache()
	if err != nil {
		return "", err
	}
	res, err := esb.Run(ctx, sandbox.ExecSpec{Profile: sandbox.Execute, Argv: []string{"go", "env", "GOVERSION"}, Timeout: 2 * time.Minute, StepID: "goversion"})
	if err != nil {
		return "", err
	}
	v := strings.TrimSpace(string(res.Stdout))
	if res.ExitCode != 0 || !strings.HasPrefix(v, "go1.") || strings.ContainsAny(v, " \n") {
		return "", fmt.Errorf("vulnscan: go env GOVERSION: exit %d: %q %s", res.ExitCode, v, strings.TrimSpace(string(res.Stderr)))
	}
	return v, nil
}

// EnsureScanner returns the scanner for img under root, building it in the
// sandbox's tool profile the first time. goVersion is the image's Go
// version as GoVersion reports it. An existing scanner is verified, never
// rebuilt: a mismatch is returned as an error wrapping
// vuln.ErrScannerMismatch.
func EnsureScanner(ctx context.Context, sb *sandbox.Docker, root string, img toolchain.Image, goVersion string) (*Scanner, error) {
	pin, err := vuln.ScannerFor(img.GoMinor)
	if err != nil {
		return nil, err
	}
	return ensureScanner(root, img, pin, goVersion, func() (string, error) { return buildScanner(ctx, sb, pin) })
}

func ensureScanner(root string, img toolchain.Image, pin vuln.ScannerPin, goVersion string, build func() (string, error)) (*Scanner, error) {
	if !filepath.IsAbs(root) {
		return nil, fmt.Errorf("vulnscan: scanner root must be absolute: %s", root)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	final := filepath.Join(root, ScannerDirName(img, pin))
	if _, err := os.Lstat(final); err == nil {
		s, err := LoadScanner(final, img)
		if err != nil {
			return nil, err
		}
		if err := s.Verify(goVersion); err != nil {
			return nil, err
		}
		return s, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}

	built, err := build()
	if err != nil {
		return nil, err
	}
	id, err := inspectBinary(built)
	if err != nil {
		return nil, fmt.Errorf("vulnscan: built scanner: %w", err)
	}
	if err := pin.Check(id, goVersion); err != nil {
		return nil, fmt.Errorf("vulnscan: built scanner: %w", err)
	}
	// The host copies the binary out of the build directory, so the
	// persistent copy belongs to the operator whatever user the engine
	// wrote it as, and is assembled under a unique name and renamed into
	// place: the final path never exists half-written.
	tmp, err := os.MkdirTemp(root, ".build-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	dst := filepath.Join(tmp, binaryName)
	if err := copyFile(built, dst); err != nil {
		return nil, fmt.Errorf("vulnscan: copying the built scanner (it must be readable by the operator): %w", err)
	}
	copied, err := inspectBinary(dst)
	if err != nil {
		return nil, err
	}
	if copied.SHA256 != id.SHA256 {
		return nil, fmt.Errorf("%w: copy has sha256 %s, build %s", vuln.ErrScannerMismatch, copied.SHA256, id.SHA256)
	}
	rec := record{Pin: pin, Image: img.Ref(), ImageGoVersion: goVersion, Identity: id, BuiltAt: time.Now().UTC()}
	raw, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(tmp, recordName), append(raw, '\n'), 0o444); err != nil {
		return nil, err
	}
	if err := os.Chmod(dst, 0o555); err != nil {
		return nil, err
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, final); err != nil {
		return nil, err
	}
	s, err := LoadScanner(final, img)
	if err != nil {
		return nil, err
	}
	if err := s.Verify(goVersion); err != nil {
		return nil, err
	}
	s.Built = true
	return s, nil
}

// buildScanner runs go install in a fresh tool sandbox and returns the
// host path of the binary it wrote.
func buildScanner(ctx context.Context, sb *sandbox.Docker, pin vuln.ScannerPin) (string, error) {
	tb, err := sb.WithToolBuild()
	if err != nil {
		return "", err
	}
	res, err := tb.Run(ctx, sandbox.ExecSpec{Profile: sandbox.Tool, Argv: pin.InstallArgv(ToolsMount), Timeout: BuildTimeout, StepID: "scanner-build"})
	if err != nil {
		return "", err
	}
	if res.TimedOut || res.ExitCode != 0 {
		return "", fmt.Errorf("vulnscan: building %s@%s failed (exit %d, timed out %v); the build fetches it from %s and checks it against %s, so it needs network access to both: %s",
			vuln.ScannerPackage, pin.Version, res.ExitCode, res.TimedOut, sandbox.ToolGoProxy, sandbox.ToolGoSumDB, lastLines(res.Stderr, 5))
	}
	return filepath.Join(tb.ToolOutputDir(), binaryName), nil
}

// LoadScanner reads a provisioned scanner directory for img. It checks the
// record against img and the pin table, not the binary; Verify does that.
func LoadScanner(dir string, img toolchain.Image) (*Scanner, error) {
	pin, err := vuln.ScannerFor(img.GoMinor)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(filepath.Join(dir, recordName))
	if err != nil {
		return nil, fmt.Errorf("vulnscan: scanner directory %s has no readable record (remove the directory to rebuild): %w", dir, err)
	}
	var rec record
	if err := json.Unmarshal(raw, &rec); err != nil {
		return nil, fmt.Errorf("vulnscan: %s: %w", filepath.Join(dir, recordName), err)
	}
	if rec.Image != img.Ref() || rec.Pin != pin || len(rec.Identity.SHA256) != 64 {
		return nil, fmt.Errorf("%w: %s records image %s and pin %+v, want %s and %+v (remove the directory to rebuild)", vuln.ErrScannerMismatch, dir, rec.Image, rec.Pin, img.Ref(), pin)
	}
	return &Scanner{Dir: dir, Path: filepath.Join(dir, binaryName), Image: rec.Image, Pin: pin, ID: rec.Identity}, nil
}

// Verify checks the binary on the host: its digest against the one
// recorded at build time, and its build information against the pin and
// the image's Go version.
func (s *Scanner) Verify(goVersion string) error {
	id, err := inspectBinary(s.Path)
	if err != nil {
		return fmt.Errorf("%w: %v (remove %s to rebuild)", vuln.ErrScannerMismatch, err, s.Dir)
	}
	if id.SHA256 != s.ID.SHA256 {
		return fmt.Errorf("%w: %s has sha256 %s, recorded %s at build time (remove %s to rebuild)", vuln.ErrScannerMismatch, s.Path, id.SHA256, s.ID.SHA256, s.Dir)
	}
	if err := s.Pin.Check(id, goVersion); err != nil {
		return fmt.Errorf("%w (remove %s to rebuild)", err, s.Dir)
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}

// Database is a verified database snapshot on the host.
type Database struct {
	Dir string          `json:"dir"`
	ID  vuln.SnapshotID `json:"id"`
	// Source is the base URL it was fetched from, or "local" for a
	// directory the operator supplied.
	Source string `json:"source"`
	// Reused is true when a fetched snapshot already existed.
	Reused bool `json:"reused,omitempty"`
}

// FetchDatabase downloads the database from baseURL (vuln.DefaultBaseURL
// when empty) into a content-addressed directory under root, reusing an
// identical snapshot already there.
func FetchDatabase(ctx context.Context, root, baseURL string) (*Database, error) {
	if baseURL == "" {
		baseURL = vuln.DefaultBaseURL
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	snap, err := vuln.Fetch(ctx, vuln.FetchOptions{Parent: root, BaseURL: baseURL})
	if err != nil {
		return nil, err
	}
	return &Database{Dir: snap.Dir, ID: snap.ID, Source: baseURL, Reused: snap.Reused}, nil
}

// LocalDatabase identifies a database directory the operator supplied. It
// is never fetched or written.
func LocalDatabase(dir string) (*Database, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	id, err := vuln.Identify(abs, vuln.Limits{})
	if err != nil {
		return nil, err
	}
	if err := vuln.CheckConsistency(abs); err != nil {
		return nil, err
	}
	return &Database{Dir: abs, ID: id, Source: "local"}, nil
}

// ScanOutputCap is the per-stream capture limit for a scan. The default
// sandbox cap can truncate the JSON stream of a large module graph, which
// would make the scan inconclusive.
const ScanOutputCap = 64 << 20

// DefaultScanTimeout bounds one scan when ScanOptions.Timeout is zero.
const DefaultScanTimeout = 10 * time.Minute

// ScanOptions configure Scan.
type ScanOptions struct {
	Timeout time.Duration
	RunID   string
}

// Result is one scan.
type Result struct {
	// Scan is the parsed run; inconclusive when anything about it is not
	// as expected.
	Scan *vuln.Scan
	// Stdout and Stderr are the raw captured streams.
	Stdout, Stderr []byte
	// GoVersion is the image's Go version, read in the scan's sandbox.
	GoVersion string
	ExitCode  int
	Duration  time.Duration
}

// ErrMountUnavailable means the scan container could not see the scanner
// or the database.
var ErrMountUnavailable = errors.New("vulnscan: scanner or database not visible inside the container")

// Scan runs the scanner over the module at the sandbox's source directory,
// in the execute profile with a fresh build cache and no network, with the
// scanner at /tools and the database at /vulndb, both read-only. The image's
// Go version is read in the same sandbox, and the scanner and database are
// verified against their recorded identities immediately before the scan;
// a mismatch is an error and no scan runs. The module cache must already
// hold the module's dependencies. An inconclusive scan is a result.
func Scan(ctx context.Context, sb *sandbox.Docker, s *Scanner, db *Database, opts ScanOptions) (*Result, error) {
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultScanTimeout
	}
	msb, err := sb.WithScanMounts(s.Dir, db.Dir)
	if err != nil {
		return nil, err
	}
	if msb, err = msb.WithFreshBuildCache(); err != nil {
		return nil, err
	}
	pre, err := msb.Run(ctx, sandbox.ExecSpec{Profile: sandbox.Execute, Argv: []string{"sh", "-c", "go env GOVERSION; test -x " + ToolsMount + "/" + binaryName + " && echo TOOLS_OK; test -f " + DBMount + "/index/db.json && echo VULNDB_OK"}, Timeout: 2 * time.Minute, RunID: opts.RunID, StepID: "vuln-precheck"})
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimSpace(string(pre.Stdout)), "\n")
	goVersion := strings.TrimSpace(lines[0])
	if !strings.HasPrefix(goVersion, "go1.") {
		return nil, fmt.Errorf("vulnscan: go env GOVERSION: %q %s", goVersion, strings.TrimSpace(string(pre.Stderr)))
	}
	out := string(pre.Stdout)
	if !strings.Contains(out, "TOOLS_OK") {
		return nil, fmt.Errorf("%w: %s", ErrMountUnavailable, s.Dir)
	}
	if !strings.Contains(out, "VULNDB_OK") {
		return nil, fmt.Errorf("%w: %s", ErrMountUnavailable, db.Dir)
	}
	if err := s.Verify(goVersion); err != nil {
		return nil, err
	}
	if err := vuln.Verify(db.Dir, db.ID, vuln.Limits{}); err != nil {
		return nil, err
	}
	res, err := msb.Run(ctx, sandbox.ExecSpec{Profile: sandbox.Execute, Argv: vuln.ScanArgv(ToolsMount+"/"+binaryName, DBURL), Timeout: opts.Timeout, OutputCap: ScanOutputCap, RunID: opts.RunID, StepID: "vuln-scan"})
	if err != nil {
		return nil, err
	}
	scan := vuln.Parse(vuln.Output{Stdout: res.Stdout, Stderr: res.Stderr, ExitCode: res.ExitCode, StdoutTruncated: res.StdoutTruncated, StderrTruncated: res.StderrTruncated, TimedOut: res.TimedOut},
		vuln.Expect{ScannerVersion: s.Pin.Version, DB: DBURL, DBModified: db.ID.Modified, GoVersion: goVersion})
	return &Result{Scan: scan, Stdout: res.Stdout, Stderr: res.Stderr, GoVersion: goVersion, ExitCode: res.ExitCode, Duration: res.Duration}, nil
}

func lastLines(b []byte, n int) string {
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	s := strings.Join(lines, " | ")
	if s == "" {
		return "no stderr"
	}
	return s
}
