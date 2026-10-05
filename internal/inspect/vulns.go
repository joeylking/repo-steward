package inspect

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/joeylking/repo-steward/internal/deps"
	"github.com/joeylking/repo-steward/internal/lock"
	"github.com/joeylking/repo-steward/internal/repo"
	"github.com/joeylking/repo-steward/internal/snapshot"
	"github.com/joeylking/repo-steward/internal/vuln"
	"github.com/joeylking/repo-steward/internal/vulnscan"
)

// VulnOptions configure a vulnerability report.
type VulnOptions struct {
	RepoPath string
	DataDir  string
	// FixtureProxyDir, when set, replaces the network module proxy for the
	// repository's dependencies, as for inspect. The scanner build still
	// uses the public proxy.
	FixtureProxyDir string
	// VulnDBDir, when set, is an existing database directory used as it is
	// and never fetched. Otherwise the database is fetched from VulnDBURL
	// (vuln.DefaultBaseURL when empty) into the data directory.
	VulnDBDir   string
	VulnDBURL   string
	AllowPull   bool
	Socket      string
	Limits      snapshot.Limits
	ScanTimeout time.Duration
}

// VulnOutcome classifies a vulnerability report.
type VulnOutcome string

const (
	// VulnsClean: a conclusive scan with no third-party findings. There
	// may be standard-library findings.
	VulnsClean        VulnOutcome = "clean"
	VulnsFindings     VulnOutcome = "findings"
	VulnsInconclusive VulnOutcome = "inconclusive"
	VulnsUnsupported  VulnOutcome = "unsupported"
)

// VulnReport is the result of `repo-steward vulns`. It reports; nothing in
// it is acted on.
type VulnReport struct {
	Repo          string           `json:"repo"`
	HeadCommit    string           `json:"head_commit"`
	TreeHash      string           `json:"tree_hash"`
	WorktreeDirty bool             `json:"worktree_dirty"`
	Snapshot      *SnapshotSummary `json:"snapshot,omitempty"`
	Profile       *repo.Profile    `json:"profile,omitempty"`
	Outcome       VulnOutcome      `json:"outcome"`
	Refusals      []string         `json:"refusals,omitempty"`
	ImageRef      string           `json:"image_ref,omitempty"`
	// GoVersion is the toolchain image's Go version, which is the version
	// of the standard library the findings refer to.
	GoVersion string           `json:"go_version,omitempty"`
	Scanner   *ScannerSummary  `json:"scanner,omitempty"`
	Database  *DatabaseSummary `json:"database,omitempty"`
	// Conclusive and Reason are the parser's verdict, verbatim.
	Conclusive bool   `json:"conclusive"`
	Reason     string `json:"reason,omitempty"`
	// ThirdParty and Stdlib are null unless the scan is conclusive.
	ThirdParty []VulnFinding `json:"third_party"`
	Stdlib     []VulnFinding `json:"stdlib"`
	Notes      []string      `json:"notes,omitempty"`
	// OutputSHA256 is the digest of the scanner's raw JSON stream.
	OutputSHA256 string           `json:"scanner_output_sha256,omitempty"`
	Timings      map[string]int64 `json:"timings_ms"`
}

// ScannerSummary identifies the scanner that ran.
type ScannerSummary struct {
	Version   string `json:"version"`
	SHA256    string `json:"sha256"`
	ModuleSum string `json:"module_sum"`
	GoVersion string `json:"built_with"`
	Dir       string `json:"dir"`
	Built     bool   `json:"built_this_run"`
}

// DatabaseSummary identifies the database snapshot the scan read.
type DatabaseSummary struct {
	Source     string    `json:"source"`
	Dir        string    `json:"dir"`
	SnapshotID string    `json:"snapshot_id"`
	Modified   time.Time `json:"modified"`
	Files      int       `json:"files"`
	Bytes      int64     `json:"bytes"`
	Reused     bool      `json:"reused,omitempty"`
}

// VulnFinding is one advisory affecting one module.
type VulnFinding struct {
	ID           string     `json:"id"`
	Aliases      []string   `json:"aliases,omitempty"`
	Summary      string     `json:"summary,omitempty"`
	Module       string     `json:"module"`
	FoundVersion string     `json:"found_version"`
	FixedVersion string     `json:"fixed_version,omitempty"`
	FixAvailable bool       `json:"fix_available"`
	Level        vuln.Level `json:"level"`
	// CallPath is one representative call path, entry point first; only
	// symbol-level findings have one.
	CallPath string   `json:"call_path,omitempty"`
	Symbols  []string `json:"symbols,omitempty"`
	Packages []string `json:"packages,omitempty"`
}

// ExitCode is the command's exit status for the report: 0 conclusive with
// no third-party findings, 2 unsupported, 3 inconclusive, 4 third-party
// findings present.
func (r *VulnReport) ExitCode() int {
	switch r.Outcome {
	case VulnsClean:
		return 0
	case VulnsUnsupported:
		return 2
	case VulnsFindings:
		return 4
	}
	return 3
}

// setScan fills the scan-dependent fields from a parsed scan.
func (r *VulnReport) setScan(s *vuln.Scan, stdout []byte) {
	sum := sha256.Sum256(stdout)
	r.OutputSHA256 = hex.EncodeToString(sum[:])
	r.Conclusive, r.Reason, r.Notes = s.Conclusive, s.Reason, s.Notes
	if !s.Conclusive {
		r.Outcome = VulnsInconclusive
		r.ThirdParty, r.Stdlib = nil, nil
		return
	}
	r.ThirdParty, r.Stdlib = []VulnFinding{}, []VulnFinding{}
	for _, f := range s.Findings {
		vf := VulnFinding{ID: f.OSV, Aliases: unique(f.Aliases), Summary: f.Summary, Module: f.Module, FoundVersion: f.FoundVersion, FixedVersion: f.FixedVersion, FixAvailable: f.FixedVersion != "", Level: f.Level, CallPath: f.CallPath(), Symbols: f.Symbols, Packages: f.Packages}
		if f.Stdlib {
			r.Stdlib = append(r.Stdlib, vf)
		} else {
			r.ThirdParty = append(r.ThirdParty, vf)
		}
	}
	for _, list := range [][]VulnFinding{r.ThirdParty, r.Stdlib} {
		sort.Slice(list, func(i, j int) bool {
			if list[i].Module != list[j].Module {
				return list[i].Module < list[j].Module
			}
			return list[i].ID < list[j].ID
		})
	}
	r.Outcome = VulnsClean
	if len(r.ThirdParty) > 0 {
		r.Outcome = VulnsFindings
	}
}

// unique drops repeated entries, keeping the first of each; the public
// database lists some aliases twice.
func unique(list []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, s := range list {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// Summary is the short human account printed to stderr.
func (r *VulnReport) Summary() string {
	var b strings.Builder
	short := func(s string, n int) string {
		s = strings.TrimPrefix(s, "sha256:")
		if len(s) > n {
			return s[:n]
		}
		return s
	}
	fmt.Fprintf(&b, "vulns: %s at %s (tree %s)\n", r.Repo, short(r.HeadCommit, 12), short(r.TreeHash, 12))
	if r.Outcome == VulnsUnsupported {
		fmt.Fprintf(&b, "unsupported: %s\n", strings.Join(r.Refusals, "; "))
		return b.String()
	}
	if r.Scanner != nil && r.Database != nil {
		fmt.Fprintf(&b, "scanned with govulncheck %s (sha256 %s) in %s, database %s modified %s\n",
			r.Scanner.Version, short(r.Scanner.SHA256, 12), r.GoVersion, short(r.Database.SnapshotID, 12), r.Database.Modified.UTC().Format(time.RFC3339))
	}
	if !r.Conclusive {
		fmt.Fprintf(&b, "inconclusive: %s\nAn inconclusive scan says nothing about vulnerabilities, including that there are none.\n", r.Reason)
		return b.String()
	}
	line := func(f VulnFinding, withSummary bool) {
		id := f.ID
		if len(f.Aliases) > 0 {
			id += " (" + strings.Join(f.Aliases, ", ") + ")"
		}
		fix := "no fix"
		if f.FixAvailable {
			fix = "fixed in " + f.FixedVersion
		}
		fmt.Fprintf(&b, "  %s %s %s, %s, %s level", id, f.Module, f.FoundVersion, fix, f.Level)
		if f.CallPath != "" {
			fmt.Fprintf(&b, ": %s", f.CallPath)
		}
		b.WriteString("\n")
		if withSummary && f.Summary != "" {
			fmt.Fprintf(&b, "    %s\n", f.Summary)
		}
	}
	fmt.Fprintf(&b, "third-party dependencies: %d finding(s)\n", len(r.ThirdParty))
	for _, f := range r.ThirdParty {
		line(f, true)
	}
	fmt.Fprintf(&b, "standard library: %d finding(s)\n", len(r.Stdlib))
	if len(r.Stdlib) > 0 {
		fmt.Fprintf(&b, "  These follow the toolchain image's Go version, %s, not the repository's dependencies. repo-steward never changes the go directive, so it cannot fix them today; they are listed for information.\n", r.GoVersion)
	}
	for _, f := range r.Stdlib {
		line(f, false)
	}
	b.WriteString("This command only reports. To act on a third-party finding, run maintain -mode baseline -select vulnerable.\n")
	return b.String()
}

// Vulns scans the repository at HEAD for known vulnerabilities and reports
// them. It changes nothing in the repository: it profiles and snapshots
// HEAD as inspect does, populates the module cache in the acquire profile,
// provisions the pinned scanner and the database, verifies both, and runs
// the scanner in the execute profile with no network.
func Vulns(ctx context.Context, opts VulnOptions) (*VulnReport, error) {
	start := time.Now()
	rep := &VulnReport{Timings: map[string]int64{}}
	mark := func(name string, since time.Time) { rep.Timings[name] = time.Since(since).Milliseconds() }

	repoPath, err := filepath.Abs(opts.RepoPath)
	if err != nil {
		return nil, err
	}
	rep.Repo = repoPath
	if opts.DataDir == "" {
		if opts.DataDir, err = DefaultDataDir(); err != nil {
			return nil, err
		}
	}
	if opts.Limits == (snapshot.Limits{}) {
		opts.Limits = snapshot.DefaultLimits()
	}
	if err := lock.CheckLocal(opts.DataDir); err != nil {
		return nil, err
	}
	exec, err := lock.Acquire(filepath.Join(opts.DataDir, "executor.lock"))
	if err != nil {
		return nil, err
	}
	defer exec.Release()

	h, err := readHead(ctx, repoPath, opts.DataDir, opts.Limits, mark)
	if err != nil {
		return nil, err
	}
	rep.HeadCommit, rep.TreeHash, rep.WorktreeDirty, rep.Snapshot, rep.Profile = h.headCommit, h.treeHash, h.dirty, h.snapshot, h.profile
	if h.refusals != nil {
		rep.Outcome = VulnsUnsupported
		rep.Refusals = h.refusals
		mark("total", start)
		return rep, nil
	}
	img := *h.profile.Toolchain
	if _, err := vuln.ScannerFor(img.GoMinor); err != nil {
		return nil, err
	}

	// Database first: a bad -vulndb or an unreachable database fails before
	// any container starts.
	t := time.Now()
	var db *vulnscan.Database
	if opts.VulnDBDir != "" {
		db, err = vulnscan.LocalDatabase(opts.VulnDBDir)
	} else {
		db, err = vulnscan.FetchDatabase(ctx, filepath.Join(opts.DataDir, "vulndb"), opts.VulnDBURL)
	}
	if err != nil {
		return nil, err
	}
	rep.Database = &DatabaseSummary{Source: db.Source, Dir: db.Dir, SnapshotID: db.ID.Hash, Modified: db.ID.Modified, Files: db.ID.Files, Bytes: db.ID.Bytes, Reused: db.Reused}
	mark("database", t)

	t = time.Now()
	sb, cleanup, err := startSandbox(ctx, "vulns", opts.DataDir, h, opts.FixtureProxyDir, opts.Socket, opts.AllowPull)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	rep.ImageRef = img.Ref()
	mark("sandbox", t)

	t = time.Now()
	if err := deps.Download(ctx, sb); err != nil {
		return nil, err
	}
	mark("download", t)

	t = time.Now()
	goVersion, err := vulnscan.GoVersion(ctx, sb)
	if err != nil {
		return nil, err
	}
	scanner, err := vulnscan.EnsureScanner(ctx, sb, filepath.Join(opts.DataDir, "tools"), img, goVersion)
	if err != nil {
		return nil, err
	}
	rep.Scanner = &ScannerSummary{Version: scanner.ID.Version, SHA256: scanner.ID.SHA256, ModuleSum: scanner.ID.ModuleSum, GoVersion: scanner.ID.GoVersion, Dir: scanner.Dir, Built: scanner.Built}
	mark("scanner", t)

	t = time.Now()
	res, err := vulnscan.Scan(ctx, sb, scanner, db, vulnscan.ScanOptions{Timeout: opts.ScanTimeout})
	if err != nil {
		return nil, err
	}
	rep.GoVersion = res.GoVersion
	rep.setScan(res.Scan, res.Stdout)
	mark("scan", t)
	mark("total", start)
	return rep, nil
}
