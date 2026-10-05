package steward

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	rttrace "github.com/joeylking/agent-runtime/trace"

	"github.com/joeylking/repo-steward/internal/deps"
	"github.com/joeylking/repo-steward/internal/manifest"
	"github.com/joeylking/repo-steward/internal/proposal"
	"github.com/joeylking/repo-steward/internal/sandbox"
	"github.com/joeylking/repo-steward/internal/task"
	"github.com/joeylking/repo-steward/internal/vuln"
	"github.com/joeylking/repo-steward/internal/vulnscan"
)

// Upgrade selection in baseline mode.
const (
	// SelectSmallest picks the smallest eligible upgrade of a direct
	// dependency. It is the default.
	SelectSmallest = "smallest"
	// SelectVulnerable scans the base tree and picks the lowest eligible
	// upgrade that clears a module's known vulnerabilities, direct or
	// indirect.
	SelectVulnerable = "vulnerable"
)

// Outcomes of vulnerable selection, all explained non-results.
const (
	// OutcomeScanInconclusive: the base scan did not complete; it says
	// nothing about vulnerabilities, including that there are none.
	OutcomeScanInconclusive = "scan_inconclusive"
	// OutcomeNoVulnerabilities: a conclusive base scan with no third-party
	// findings. Standard-library findings may be listed.
	OutcomeNoVulnerabilities = "no_vulnerabilities"
	// OutcomeNoFixAvailable: third-party findings, none of which an
	// eligible upgrade clears.
	OutcomeNoFixAvailable = "no_fix_available"
)

// ErrSelectUnsupported is returned when an agent mode is asked to select
// by vulnerability.
var ErrSelectUnsupported = errors.New("-select vulnerable is not yet supported in this mode; use -mode baseline")

// checkSelect validates Options.Select for a mode.
func checkSelect(sel, mode string) error {
	switch sel {
	case "", SelectSmallest:
		return nil
	case SelectVulnerable:
		if mode == "baseline" {
			return nil
		}
		return ErrSelectUnsupported
	}
	return fmt.Errorf("steward: unknown selection %q (want %s or %s)", sel, SelectSmallest, SelectVulnerable)
}

// VulnResult is what vulnerable selection adds to a result.
type VulnResult struct {
	Scanner  *ScannerInfo  `json:"scanner,omitempty"`
	Database *DatabaseInfo `json:"database,omitempty"`
	// GoVersion is the toolchain image's Go version, which is the standard
	// library version the scans refer to.
	GoVersion string    `json:"go_version,omitempty"`
	Base      *ScanInfo `json:"base,omitempty"`
	Post      *ScanInfo `json:"post,omitempty"`
	// Plan is the target computation over the base scan; Selected is the
	// upgrade this run applies and Remaining the eligible upgrades left for
	// later runs.
	Plan      *deps.VulnPlan    `json:"plan,omitempty"`
	Selected  *deps.VulnTarget  `json:"selected,omitempty"`
	Remaining []deps.VulnTarget `json:"remaining,omitempty"`
	// StdlibNote says why standard-library findings are not acted on.
	StdlibNote string `json:"stdlib_note,omitempty"`
	// CooldownWaived is set when the selected version is within the
	// version cooldown, or its publish time is unknown, and was selected
	// anyway because it clears the advisories: it says why the cooldown
	// would have refused it.
	CooldownWaived string `json:"cooldown_waived,omitempty"`
}

// ScannerInfo identifies the scanner.
type ScannerInfo struct {
	Version   string `json:"version"`
	SHA256    string `json:"sha256"`
	ModuleSum string `json:"module_sum"`
	BuiltWith string `json:"built_with"`
	Built     bool   `json:"built_this_run"`
}

// DatabaseInfo identifies the database snapshot.
type DatabaseInfo struct {
	Source     string    `json:"source"`
	SnapshotID string    `json:"snapshot_id"`
	Modified   time.Time `json:"modified"`
}

// ScanInfo summarizes one recorded scan.
type ScanInfo struct {
	ID           string         `json:"id"`
	TreeHash     string         `json:"tree_hash"`
	Conclusive   bool           `json:"conclusive"`
	Reason       string         `json:"reason,omitempty"`
	OutputSHA256 string         `json:"output_sha256"`
	ThirdParty   []vuln.Finding `json:"third_party"`
	Stdlib       []vuln.Finding `json:"stdlib"`
}

const stdlibNote = "standard-library findings follow the toolchain image's Go version; repo-steward never changes the go directive, so they are reported but not fixed"

// openDatabase identifies or fetches the database before any container
// starts, so a bad -vulndb fails first.
func (r *run) openDatabase(ctx context.Context) (*vulnscan.Database, error) {
	if r.opts.VulnDBDir != "" {
		return vulnscan.LocalDatabase(r.opts.VulnDBDir)
	}
	return vulnscan.FetchDatabase(ctx, filepath.Join(r.dataDir, "vulndb"), r.opts.VulnDBURL)
}

// scan runs the scanner over sb's snapshot, which holds tree, and records
// the scan as evidence for it.
func (r *run) scan(ctx context.Context, sb *sandbox.Docker, tree, kind string) (*ScanInfo, *vuln.Scan, error) {
	res, err := vulnscan.Scan(ctx, sb, r.scanner, r.vdb, vulnscan.ScanOptions{Timeout: r.opts.ScanTimeout, RunID: r.id})
	if err != nil {
		return nil, nil, err
	}
	raw, err := json.Marshal(res.Scan)
	if err != nil {
		return nil, nil, err
	}
	stderr := string(res.Stderr)
	if len(stderr) > 64<<10 {
		stderr = stderr[:64<<10]
	}
	rec := task.ScanRecord{ID: newID(), RunID: r.id, StepID: kind, Kind: kind, TreeHash: tree, ConfigHash: r.configHash, ToolchainDigest: r.profile.Toolchain.Digest,
		ScannerVersion: r.scanner.ID.Version, ScannerSHA256: r.scanner.ID.SHA256, DBSnapshotID: r.vdb.ID.Hash, DBModified: r.vdb.ID.Modified, GoVersion: res.GoVersion,
		Conclusive: res.Scan.Conclusive, Reason: res.Scan.Reason, Scan: raw, Output: res.Stdout, Stderr: stderr}
	if err := r.store.InsertScan(ctx, rec); err != nil {
		return nil, nil, err
	}
	r.res.Vulnerabilities.GoVersion = res.GoVersion
	info := &ScanInfo{ID: rec.ID, TreeHash: tree, Conclusive: res.Scan.Conclusive, Reason: res.Scan.Reason}
	stored, err := r.store.ListScans(ctx, r.id)
	if err != nil {
		return nil, nil, err
	}
	for _, s := range stored {
		if s.ID == rec.ID {
			info.OutputSHA256 = s.OutputSHA256
		}
	}
	if res.Scan.Conclusive {
		info.ThirdParty, info.Stdlib = []vuln.Finding{}, []vuln.Finding{}
		for _, f := range res.Scan.Findings {
			if f.Stdlib {
				info.Stdlib = append(info.Stdlib, f)
			} else {
				info.ThirdParty = append(info.ThirdParty, f)
			}
		}
	}
	return info, res.Scan, nil
}

// selectVulnerable provisions the scanner, scans the base tree, and
// computes the upgrade that clears the most reachable advisories. It
// returns a non-empty outcome when there is nothing to apply.
func (r *run) selectVulnerable(ctx context.Context) (string, *deps.VulnTarget, error) {
	res, sb, img := r.res, r.sb, *r.profile.Toolchain
	t := time.Now()
	goVersion, err := vulnscan.GoVersion(ctx, sb)
	if err != nil {
		return "", nil, err
	}
	toolsDir := r.opts.ToolsDir
	if toolsDir == "" {
		toolsDir = filepath.Join(r.dataDir, "tools")
	}
	if r.scanner, err = vulnscan.EnsureScanner(ctx, sb, toolsDir, img, goVersion); err != nil {
		return "", nil, err
	}
	res.Vulnerabilities.Scanner = &ScannerInfo{Version: r.scanner.ID.Version, SHA256: r.scanner.ID.SHA256, ModuleSum: r.scanner.ID.ModuleSum, BuiltWith: r.scanner.ID.GoVersion, Built: r.scanner.Built}
	r.mark("scanner", t)

	t = time.Now()
	base, scan, err := r.scan(ctx, sb, r.ws.BaseTree, "base")
	if err != nil {
		return "", nil, err
	}
	res.Vulnerabilities.Base = base
	r.baseScan = scan
	r.mark("base_scan", t)
	if !scan.Conclusive {
		res.Detail = map[string]any{"reason": scan.Reason}
		return OutcomeScanInconclusive, nil, nil
	}
	if len(base.Stdlib) > 0 {
		res.Vulnerabilities.StdlibNote = stdlibNote
	}
	if len(base.ThirdParty) == 0 {
		return OutcomeNoVulnerabilities, nil, nil
	}

	t = time.Now()
	mods := []string{}
	seen := map[string]bool{}
	for _, f := range base.ThirdParty {
		if !seen[f.Module] {
			seen[f.Module] = true
			mods = append(mods, f.Module)
		}
	}
	sort.Strings(mods)
	versions, err := deps.PublishedVersions(ctx, sb, mods)
	if err != nil {
		return "", nil, err
	}
	modBytes, err := r.ws.ReadFile("go.mod")
	if err != nil {
		return "", nil, err
	}
	baseFacts, err := manifest.Parse(modBytes)
	if err != nil {
		return "", nil, err
	}
	facts := map[string]deps.ModuleFacts{}
	for _, f := range base.ThirdParty {
		if _, ok := facts[f.Module]; ok {
			continue
		}
		vs, ok := versions[f.Module]
		if !ok {
			continue
		}
		req, listed := baseFacts.Require[f.Module]
		facts[f.Module] = deps.ModuleFacts{Current: f.FoundVersion, Direct: listed && !req.Indirect, Versions: vs}
	}
	plan := deps.PlanVulnFixes(base.ThirdParty, facts, r.opts.Policy)
	res.Vulnerabilities.Plan = &plan
	r.mark("vuln_targets", t)
	sel, ok := plan.Selected()
	if !ok {
		res.Detail = map[string]any{"not_fixable": plan.NotFixable}
		return OutcomeNoFixAvailable, nil, nil
	}
	res.Vulnerabilities.Selected = &sel
	res.Vulnerabilities.Remaining = plan.Targets[1:]
	// The cooldown guards against adopting a hijacked release before
	// anyone notices; a version that clears a known advisory is taken
	// anyway, and the result and the proposal say so.
	if pol := r.opts.Policy; pol.MinAge > 0 {
		times, err := deps.PublishTimes(ctx, sb, sel.Module, []string{sel.Version})
		if err != nil {
			return "", nil, err
		}
		pub, known := times[sel.Version]
		res.Vulnerabilities.CooldownWaived = deps.CooldownReason(pub, known, r.opts.now(), pol.MinAge)
	}
	return "", &sel, nil
}

// Bounds on database text in a proposal body.
const (
	maxIDLen      = 64
	maxAliases    = 5
	maxSummaryLen = 200
	maxPathLen    = 300
	maxPlainLen   = 200
)

// untrusted renders text that came from the vulnerability database or the
// repository as one bounded code span: control and invisible characters are
// escaped and newlines become spaces (rttrace.Sanitize), whitespace runs
// collapse, backticks become quotes so the span cannot be closed early, and
// the result is cut to max runes. Inside a code span a pull request renders
// no mention, link, or HTML, and because nothing can start a new line the
// text cannot pose as one of the body's own lines or as a commit trailer.
func untrusted(s string, max int) string {
	s = plain(s, max)
	if s == "" {
		return "(none)"
	}
	return "`" + s + "`"
}

// plain sanitizes and bounds text without the code span, for values the
// toolchain already constrains, such as module paths and versions.
func plain(s string, max int) string {
	s = rttrace.Sanitize(strings.Join(strings.Fields(s), " "))
	s = strings.ReplaceAll(s, "`", "'")
	if utf8.RuneCountInString(s) > max {
		rs := []rune(s)
		s = string(rs[:max]) + "..."
	}
	return s
}

// advisorySection is the part of the proposal body that says which
// advisories the upgrade fixes and which findings remain. Every value from
// the database or the scanned code passes through untrusted or plain.
func advisorySection(v *VulnResult, sel *deps.VulnTarget, ready *proposal.Readiness, base *vuln.Scan) string {
	var b strings.Builder
	kind := "a direct dependency"
	if !sel.Direct {
		kind = "an indirect dependency (the main module does not require it directly; go.mod records the new version as an // indirect requirement)"
	}
	fmt.Fprintf(&b, "Vulnerabilities fixed by this upgrade of %s, %s:\n", plain(sel.Module, maxPathLen), kind)
	resolved := map[string]bool{}
	if ready != nil && ready.Vulnerabilities != nil {
		for _, k := range ready.Vulnerabilities.Resolved {
			resolved[k] = true
		}
	}
	for _, key := range sel.Fixes {
		f, ok := findingByKey(base, key)
		if !ok || !resolved[key] {
			continue
		}
		fmt.Fprintf(&b, "- %s", untrusted(f.OSV, maxIDLen))
		if len(f.Aliases) > 0 {
			aliases := dedupe(f.Aliases)
			shown := []string{}
			for i, a := range aliases {
				if i == maxAliases {
					shown = append(shown, fmt.Sprintf("and %d more", len(aliases)-maxAliases))
					break
				}
				shown = append(shown, untrusted(a, maxIDLen))
			}
			fmt.Fprintf(&b, " (aliases: %s)", strings.Join(shown, ", "))
		}
		fmt.Fprintf(&b, ": %s\n", untrusted(f.Summary, maxSummaryLen))
		fmt.Fprintf(&b, "  %s %s -> %s (the advisory's fix: %s); reachability at base: %s\n", plain(f.Module, maxPathLen), plain(f.FoundVersion, maxPlainLen), plain(sel.Version, maxPlainLen), plain(f.FixedVersion, maxPlainLen), plain(string(f.Level), maxPlainLen))
		if p := f.CallPath(); p != "" {
			fmt.Fprintf(&b, "  Call path at base: %s\n", untrusted(p, maxPathLen))
		}
	}
	var remaining []string
	for _, n := range sel.Unfixed {
		remaining = append(remaining, fmt.Sprintf("- %s in %s %s: %s", untrusted(n.ID, maxIDLen), plain(n.Module, maxPathLen), plain(n.Found, maxPlainLen), plain(n.Reason, maxPlainLen)))
	}
	for _, t := range v.Remaining {
		for _, key := range t.Fixes {
			if f, ok := findingByKey(base, key); ok {
				remaining = append(remaining, fmt.Sprintf("- %s in %s %s: fixable by upgrading to %s in a later run (one upgrade per run)", untrusted(f.OSV, maxIDLen), plain(f.Module, maxPathLen), plain(f.FoundVersion, maxPlainLen), plain(t.Version, maxPlainLen)))
			}
		}
		for _, n := range t.Unfixed {
			remaining = append(remaining, fmt.Sprintf("- %s in %s %s: %s", untrusted(n.ID, maxIDLen), plain(n.Module, maxPathLen), plain(n.Found, maxPlainLen), plain(n.Reason, maxPlainLen)))
		}
	}
	if v.Plan != nil {
		for _, n := range v.Plan.NotFixable {
			remaining = append(remaining, fmt.Sprintf("- %s in %s %s: %s", untrusted(n.ID, maxIDLen), plain(n.Module, maxPathLen), plain(n.Found, maxPlainLen), plain(n.Reason, maxPlainLen)))
		}
	}
	b.WriteString("\nThird-party findings that remain:")
	if len(remaining) == 0 {
		b.WriteString(" none\n")
	} else {
		b.WriteString("\n" + strings.Join(remaining, "\n") + "\n")
	}
	stdlib := 0
	if v.Base != nil {
		stdlib = len(v.Base.Stdlib)
	}
	fmt.Fprintf(&b, "Standard-library findings: %d", stdlib)
	if stdlib > 0 {
		fmt.Fprintf(&b, " (%s)", stdlibNote)
	}
	b.WriteString("\n")
	if v.CooldownWaived != "" {
		fmt.Fprintf(&b, "Version cooldown waived for %s %s, which clears the advisories above: %s.\n", plain(sel.Module, maxPathLen), plain(sel.Version, maxPlainLen), plain(v.CooldownWaived, maxPathLen))
	}
	if v.Scanner != nil && v.Database != nil {
		fmt.Fprintf(&b, "\nScanned with govulncheck %s against database snapshot %s, modified %s; summaries and identifiers above are quoted from the database as untrusted text.\n",
			plain(v.Scanner.Version, maxPlainLen), plain(v.Database.SnapshotID, maxPlainLen), v.Database.Modified.UTC().Format(time.RFC3339))
	}
	return b.String()
}

func findingByKey(s *vuln.Scan, key string) (vuln.Finding, bool) {
	if s == nil {
		return vuln.Finding{}, false
	}
	for _, f := range s.Findings {
		if f.Key == key {
			return f, true
		}
	}
	return vuln.Finding{}, false
}

func dedupe(list []string) []string {
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
