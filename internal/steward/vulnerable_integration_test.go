//go:build integration

package steward_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/joeylking/repo-steward/internal/deps"
	"github.com/joeylking/repo-steward/internal/fixture"
	"github.com/joeylking/repo-steward/internal/gitx"
	"github.com/joeylking/repo-steward/internal/manifest"
	"github.com/joeylking/repo-steward/internal/modproxy"
	"github.com/joeylking/repo-steward/internal/proposal"
	"github.com/joeylking/repo-steward/internal/repo"
	"github.com/joeylking/repo-steward/internal/sandbox"
	"github.com/joeylking/repo-steward/internal/snapshot"
	"github.com/joeylking/repo-steward/internal/steward"
	"github.com/joeylking/repo-steward/internal/task"
	"github.com/joeylking/repo-steward/internal/testtmp"
	"github.com/joeylking/repo-steward/internal/vuln"
	"github.com/joeylking/repo-steward/internal/workspace"
)

// The scanner is built once per test binary, through the public module
// proxy and checksum database, into a tools directory every test shares;
// the fixture proxy keeps the repositories' own dependencies offline.
var shared struct {
	root, tools, proxy, fixtureDB string
}

func TestMain(m *testing.M) {
	code := func() int {
		if err := os.MkdirAll(testtmp.Root(), 0o755); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		root, err := os.MkdirTemp(testtmp.Root(), "steward-")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		defer testtmp.RemoveAll(root)
		shared.root, shared.tools, shared.proxy, shared.fixtureDB = root, filepath.Join(root, "tools"), filepath.Join(root, "proxy"), filepath.Join(root, "vulndb")
		if _, err := modproxy.Build(shared.proxy); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		if err := vuln.WriteFixtureDB(shared.fixtureDB); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return m.Run()
	}()
	os.Exit(code)
}

// innerAdvisory is declared vulnerable before v1.0.1 of example.com/inner,
// which the indirect fixtures reach only through another module.
var innerAdvisory = vuln.Advisory{
	ID: "GO-TEST-0007", Aliases: []string{"CVE-0000-0007"}, Summary: "Normalization flaw in example.com/inner", Details: "Do is declared vulnerable before v1.0.1.",
	Module: "example.com/inner", Ranges: []vuln.Range{{Fixed: "v1.0.1"}}, Packages: []vuln.AffectedPackage{{Path: "example.com/inner", Symbols: []string{"Do"}}},
}

// customDB writes a database of advisories for one test.
func customDB(t *testing.T, advisories ...vuln.Advisory) string {
	t.Helper()
	dir := filepath.Join(testtmp.Dir(t), "vulndb")
	if err := vuln.WriteDB(dir, advisories); err != nil {
		t.Fatal(err)
	}
	return dir
}

func fixtureAdvisory(id string) vuln.Advisory {
	for _, a := range vuln.FixtureAdvisories() {
		if a.ID == id {
			return a
		}
	}
	panic(id)
}

type vulnRun struct {
	res  *steward.Result
	data string
	repo string
}

// runVulnerable runs baseline mode with vulnerable selection on a fixture.
func runVulnerable(t *testing.T, name, db string, edit func(*steward.Options)) vulnRun {
	t.Helper()
	root := testtmp.Dir(t)
	r, err := fixture.Setup(ctx, name, filepath.Join(root, "repo"))
	if err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(root, "data")
	opts := steward.Options{SourcePath: r.Path, DataDir: data, FixtureProxyDir: shared.proxy, Policy: deps.DefaultPolicy(), Author: author,
		Select: steward.SelectVulnerable, VulnDBDir: db, ToolsDir: shared.tools}
	if edit != nil {
		edit(&opts)
	}
	res, err := steward.RunBaseline(ctx, opts)
	if err != nil {
		if strings.Contains(err.Error(), "building golang.org/x/vuln") {
			t.Fatalf("building the scanner needs network access to proxy.golang.org and sum.golang.org, once per test binary: %v", err)
		}
		t.Fatalf("%v (result %+v)", err, res)
	}
	status, _ := gitx.New(r.Path).Run(ctx, "status", "--porcelain")
	if len(status) != 0 {
		t.Fatalf("source checkout modified:\n%s", status)
	}
	if head, _ := gitx.New(r.Path).RevParse(ctx, "HEAD"); head != r.BaseCommit {
		t.Fatal("source HEAD moved")
	}
	return vulnRun{res: res, data: data, repo: r.Path}
}

func (v vulnRun) store(t *testing.T) *task.Store {
	t.Helper()
	s, err := task.Open(filepath.Join(v.data, "steward.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func keys(fs []vuln.Finding) string {
	var out []string
	for _, f := range fs {
		out = append(out, f.Key)
	}
	return strings.Join(out, ",")
}

// patch-safe with the fixture database: GO-TEST-0001 is reachable in lib
// v1.2.1 and fixed in v1.2.4, so the run proposes v1.2.4, the lowest fixing
// version, not v1.3.0, with the advisory in the body and both scans
// recorded against the trees they scanned.
func TestVulnerable_PatchSafeFixesAdvisory(t *testing.T) {
	v := runVulnerable(t, "patch-safe", shared.fixtureDB, nil)
	res := v.res
	if res.Outcome != steward.OutcomeProposalPrepared {
		t.Fatalf("outcome %s detail %v readiness %+v vulnerabilities %+v", res.Outcome, res.Detail, res.Readiness, res.Vulnerabilities)
	}
	if res.Selected == nil || res.Selected.Module != "example.com/lib" || res.Selected.Version != "v1.2.4" {
		t.Fatalf("selected %+v", res.Selected)
	}
	vr := res.Vulnerabilities
	if vr == nil || vr.Selected == nil || !vr.Selected.Direct || strings.Join(vr.Selected.Fixes, ",") != "GO-TEST-0001 example.com/lib" || len(vr.Remaining) != 0 {
		t.Fatalf("vulnerabilities %+v", vr)
	}
	if keys(vr.Base.ThirdParty) != "GO-TEST-0001 example.com/lib" || keys(vr.Base.Stdlib) != "GO-TEST-0004 stdlib" || keys(vr.Post.ThirdParty) != "" || keys(vr.Post.Stdlib) != "GO-TEST-0004 stdlib" {
		t.Fatalf("base %+v post %+v", vr.Base, vr.Post)
	}
	p := res.Proposal
	ev := p.Readiness.Vulnerabilities
	if ev == nil || strings.Join(ev.Resolved, ",") != "GO-TEST-0001 example.com/lib" || ev.BaseScanID != vr.Base.ID || ev.PostScanID != vr.Post.ID || len(ev.Introduced)+len(ev.Escalated) != 0 {
		t.Fatalf("readiness evidence %+v", ev)
	}
	for _, want := range []string{
		"Vulnerabilities fixed by this upgrade of example.com/lib, a direct dependency:",
		"- `GO-TEST-0001` (aliases: `CVE-0000-0001`): `Greeting injection in example.com/lib`",
		"example.com/lib v1.2.1 -> v1.2.4 (the advisory's fix: v1.2.4); reachability at base: symbol",
		"Call path at base: `app.main (main.go:10) -> lib.Greet`",
		"Third-party findings that remain: none",
		"Standard-library findings: 1 (",
		"Scanned with govulncheck " + vr.Scanner.Version + " against database snapshot " + vr.Database.SnapshotID,
	} {
		if !strings.Contains(p.Body, want) {
			t.Errorf("body lacks %q:\n%s", want, p.Body)
		}
	}
	raw, _ := json.MarshalIndent(res, "", "  ")
	t.Logf("result:\n%s", raw)

	// Both scans are evidence bound to the trees they scanned, and the
	// frozen proposal still verifies.
	store := v.store(t)
	scans, err := store.ListScans(ctx, res.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if len(scans) != 2 {
		t.Fatalf("scans = %d", len(scans))
	}
	byKind := map[string]task.ScanRecord{}
	for _, s := range scans {
		byKind[s.Kind] = s
	}
	b, post := byKind["base"], byKind["post"]
	if b.TreeHash != res.Workspace.BaseTree || post.TreeHash != p.TreeHash || b.TreeHash == post.TreeHash {
		t.Fatalf("scan trees base %s post %s, want %s and %s", b.TreeHash, post.TreeHash, res.Workspace.BaseTree, p.TreeHash)
	}
	for _, s := range scans {
		if !s.Conclusive || s.ConfigHash != p.ConfigHash || s.ToolchainDigest != p.ToolchainDigest || s.ScannerSHA256 != vr.Scanner.SHA256 || s.DBSnapshotID != vr.Database.SnapshotID || len(s.Output) == 0 || s.GoVersion != vr.GoVersion {
			t.Fatalf("scan record %+v", s)
		}
	}
	ws, err := workspace.Open(ctx, res.Workspace.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := proposal.Verify(ctx, store, ws, res.RunID, p.ID); err != nil {
		t.Fatal(err)
	}
	mod, _ := ws.Git().Run(ctx, "show", p.TreeHash+":go.mod")
	if !strings.Contains(string(mod), "example.com/lib v1.2.4") {
		t.Fatalf("go.mod in proposal tree:\n%s", mod)
	}
}

// indirect-fix: inner is only an indirect requirement, reached through
// wrap.Run. The run upgrades it to v1.0.1, the lowest fixing version, and
// go.mod keeps it as an indirect requirement at that version.
func TestVulnerable_IndirectTarget(t *testing.T) {
	v := runVulnerable(t, "indirect-fix", customDB(t, innerAdvisory), nil)
	res := v.res
	if res.Outcome != steward.OutcomeProposalPrepared {
		t.Fatalf("outcome %s detail %v normalization %+v readiness %+v", res.Outcome, res.Detail, res.Normalization, res.Readiness)
	}
	vr := res.Vulnerabilities
	if res.Selected.Module != "example.com/inner" || res.Selected.Version != "v1.0.1" || vr.Selected.Direct || vr.Selected.Level != vuln.LevelSymbol {
		t.Fatalf("selected %+v %+v", res.Selected, vr.Selected)
	}
	if len(res.Candidates) != 0 {
		t.Fatalf("discovery lists indirect or unchanged modules: %+v", res.Candidates)
	}
	p := res.Proposal
	if !strings.Contains(p.Body, "an indirect dependency (the main module does not require it directly") || !strings.Contains(p.Body, "Call path at base: `app.main (main.go:10) -> wrap.Run (wrap.go:8) -> inner.Do`") {
		t.Fatalf("body:\n%s", p.Body)
	}
	ws, _ := workspace.Open(ctx, res.Workspace.Dir)
	mod, _ := ws.Git().Run(ctx, "show", p.TreeHash+":go.mod")
	if !strings.Contains(string(mod), "example.com/inner v1.0.1 // indirect") || !strings.Contains(string(mod), "example.com/wrap v1.0.0\n") {
		t.Fatalf("go.mod in proposal tree:\n%s", mod)
	}
	t.Logf("body:\n%s", p.Body)
}

// closure-regression: GO-TEST-0005 is reached through core.Run and util.go
// both, and util is a direct requirement. The fix, util v0.2.0, renames
// Trim, which core v1.0.0 and util.go still call, so the post validation
// fails and nothing is proposed: baseline mode does not repair.
func TestVulnerable_ClosureRegressionIsRegressed(t *testing.T) {
	v := runVulnerable(t, "closure-regression", shared.fixtureDB, nil)
	res := v.res
	if res.Outcome != steward.OutcomeRegressed || res.Proposal != nil {
		t.Fatalf("outcome %s detail %v normalization %+v", res.Outcome, res.Detail, res.Normalization)
	}
	if res.Selected.Module != "example.com/util" || res.Selected.Version != "v0.2.0" || !res.Vulnerabilities.Selected.Direct {
		t.Fatalf("selected %+v %+v", res.Selected, res.Vulnerabilities.Selected)
	}
	if res.Vulnerabilities.Post != nil {
		t.Fatal("a regressed candidate was scanned")
	}
	var intro []string
	for _, f := range res.Introduced {
		intro = append(intro, f.Key)
	}
	if !strings.Contains(strings.Join(intro, " "), "build:/cache/mod/example.com/core@v1.0.0/core.go:undefined: util.Trim") {
		t.Fatalf("introduced: %v", intro)
	}
}

// moved-package: GO-TEST-0003 has no fix, so there is nothing to apply.
func TestVulnerable_NoFixAvailable(t *testing.T) {
	v := runVulnerable(t, "moved-package", shared.fixtureDB, nil)
	res := v.res
	if res.Outcome != steward.OutcomeNoFixAvailable || res.Selected != nil {
		t.Fatalf("outcome %s selected %+v detail %v", res.Outcome, res.Selected, res.Detail)
	}
	notes, ok := res.Detail["not_fixable"].([]deps.FindingNote)
	if !ok || len(notes) != 1 || notes[0].ID != "GO-TEST-0003" || notes[0].Reason != deps.ReasonNoFix {
		t.Fatalf("detail %#v", res.Detail)
	}
	if proms, _ := v.store(t).ListPromotions(ctx, res.RunID); len(proms) != 0 {
		t.Fatal("manifests promoted with nothing to fix")
	}
}

// indirect-dropped: inner v1.0.0 is in the build list through wrapx's
// go.mod, but no package of it is loaded, and the scanner reports only
// modules whose packages it loads. With only that advisory and a
// standard-library one, there is nothing third-party to fix.
func TestVulnerable_NoVulnerabilities(t *testing.T) {
	v := runVulnerable(t, "indirect-dropped", customDB(t, innerAdvisory, fixtureAdvisory("GO-TEST-0004")), nil)
	res := v.res
	if res.Outcome != steward.OutcomeNoVulnerabilities || res.Selected != nil {
		t.Fatalf("outcome %s selected %+v", res.Outcome, res.Selected)
	}
	vr := res.Vulnerabilities
	if keys(vr.Base.ThirdParty) != "" || keys(vr.Base.Stdlib) != "GO-TEST-0004 stdlib" || vr.StdlibNote == "" {
		t.Fatalf("base scan %+v", vr.Base)
	}
}

// A scan that cannot finish ends the run; it is never read as "no
// vulnerabilities".
func TestVulnerable_BaseScanInconclusive(t *testing.T) {
	v := runVulnerable(t, "patch-safe", shared.fixtureDB, func(o *steward.Options) { o.ScanTimeout = 1 })
	res := v.res
	if res.Outcome != steward.OutcomeScanInconclusive || res.Detail["reason"] != "scanner timed out" || res.Selected != nil {
		t.Fatalf("outcome %s detail %v", res.Outcome, res.Detail)
	}
	if res.Vulnerabilities.Base.Conclusive || res.Vulnerabilities.Base.ThirdParty != nil {
		t.Fatalf("base %+v", res.Vulnerabilities.Base)
	}
	scans, _ := v.store(t).ListScans(ctx, res.RunID)
	if len(scans) != 1 || scans[0].Conclusive || scans[0].Reason != "scanner timed out" {
		t.Fatalf("scans %+v", scans)
	}
}

// The upgrade clears GO-TEST-0001 but v1.2.4 is affected by another
// advisory, so readiness fails and nothing is frozen.
func TestVulnerable_IntroducedAdvisoryIsNotReady(t *testing.T) {
	introduced := vuln.Advisory{ID: "GO-TEST-0098", Summary: "Flaw introduced in example.com/lib v1.2.4", Details: "Introduced in v1.2.4, fixed in v1.3.0.",
		Module: "example.com/lib", Ranges: []vuln.Range{{Introduced: "v1.2.4", Fixed: "v1.3.0"}}, Packages: []vuln.AffectedPackage{{Path: "example.com/lib", Symbols: []string{"Greet"}}}}
	v := runVulnerable(t, "patch-safe", customDB(t, fixtureAdvisory("GO-TEST-0001"), introduced), nil)
	res := v.res
	if res.Outcome != steward.OutcomeNotReady || res.Proposal != nil {
		t.Fatalf("outcome %s readiness %+v", res.Outcome, res.Readiness)
	}
	var codes []string
	for _, f := range res.Readiness.Failures {
		codes = append(codes, f.Code+": "+f.Detail)
	}
	if len(codes) != 1 || !strings.HasPrefix(codes[0], proposal.CodeAdvisoryIntroduced+": GO-TEST-0098 example.com/lib: introduced") {
		t.Fatalf("failures %v", codes)
	}
	if ev := res.Readiness.Vulnerabilities; strings.Join(ev.Resolved, ",") != "GO-TEST-0001 example.com/lib" || strings.Join(ev.Introduced, ",") != "GO-TEST-0098 example.com/lib" {
		t.Fatalf("evidence %+v", ev)
	}
	if rows, _ := v.store(t).ListProposals(ctx, res.RunID); len(rows) != 0 {
		t.Fatal("proposal rows exist")
	}
}

// indirect-dropped, at the gates themselves: go get adds an indirect
// requirement on inner v1.0.1, which Gate A admits; go mod tidy removes it
// again, because no package of inner is loaded, and the build list selects
// v1.0.0 once more. Gate B refuses that on the manifests (the target is not
// set) and the build-list rules refuse it on what the toolchain selects.
// Vulnerable selection never reaches this through a scan today: the scanner
// does not report a module none of whose packages it loads (see
// TestVulnerable_NoVulnerabilities). The gates do not rely on that.
func TestBuildListGate_TidyDroppingAnIndirectTargetIsRefused(t *testing.T) {
	root := testtmp.Dir(t)
	r, err := fixture.Setup(ctx, "indirect-dropped", filepath.Join(root, "repo"))
	if err != nil {
		t.Fatal(err)
	}
	runDir := filepath.Join(root, "run")
	ws, err := workspace.Create(ctx, r.Path, runDir)
	if err != nil {
		t.Fatal(err)
	}
	limits := snapshot.DefaultLimits()
	entries, err := snapshot.List(ctx, ws.Git(), ws.BaseTree, limits)
	if err != nil {
		t.Fatal(err)
	}
	snap, _, err := ws.Materialize(ctx, ws.BaseTree, filepath.Join(runDir, "snapshots", ws.BaseTree), limits)
	if err != nil {
		t.Fatal(err)
	}
	prof, err := repo.Inspect(snap, entries)
	if err != nil || !prof.Supported() {
		t.Fatalf("profile %+v %v", prof, err)
	}
	sb, err := sandbox.NewDocker(sandbox.Config{Image: prof.Toolchain.Ref(), SourceDir: snap, CacheDir: filepath.Join(root, "modcache"), BuildCacheDir: filepath.Join(runDir, "gocache"), ProxyDir: shared.proxy}, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sb.RemoveBuildCaches(ctx) })
	if err := sb.EnsureImage(ctx, false); err != nil {
		t.Fatal(err)
	}
	if err := deps.Download(ctx, sb); err != nil {
		t.Fatal(err)
	}
	store, err := task.Open(filepath.Join(root, "steward.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.CreateTask(ctx, task.Task{RunID: "gate", Mode: "baseline", SourcePath: r.Path, BaseCommit: ws.BaseCommit, BaseTree: ws.BaseTree, WorkspaceDir: ws.Dir}); err != nil {
		t.Fatal(err)
	}
	staging := filepath.Join(runDir, "staging")
	target := manifest.Target{Module: "example.com/inner", Version: "v1.0.1"}

	baseBL, err := manifest.WorkspaceBuildList(ctx, sb, ws, staging)
	if err != nil {
		t.Fatal(err)
	}
	if baseBL["example.com/inner"] != "v1.0.0" {
		t.Fatalf("base build list %v", baseBL)
	}
	st, err := manifest.Stage(ctx, sb, ws, staging, manifest.Op{Kind: "upgrade", Target: target})
	if err != nil {
		t.Fatal(err)
	}
	closure, err := manifest.Closure(ctx, sb, st, target)
	if err != nil {
		t.Fatal(err)
	}
	baseFacts, _ := manifest.Parse(st.Before.Mod)
	upFacts, _ := manifest.Parse(st.After.Mod)
	if got := upFacts.Require["example.com/inner"]; got.Version != "v1.0.1" || !got.Indirect {
		t.Fatalf("go get wrote %+v:\n%s", got, st.After.Mod)
	}
	if adm := manifest.VerifyAdmission(baseFacts, upFacts, target, closure); !adm.OK() {
		t.Fatalf("Gate A %+v", adm)
	}
	if _, err := manifest.Promote(ctx, store, ws, "gate", "", st); err != nil {
		t.Fatal(err)
	}
	tree1, err := ws.CandidateTree(ctx)
	if err != nil {
		t.Fatal(err)
	}
	snap1, _, err := ws.Materialize(ctx, tree1, filepath.Join(runDir, "snapshots", tree1), limits)
	if err != nil {
		t.Fatal(err)
	}
	sb1 := sb.WithSource(snap1)
	if err := deps.Download(ctx, sb1); err != nil {
		t.Fatal(err)
	}
	nst, err := manifest.Stage(ctx, sb1, ws, staging, manifest.Op{Kind: "normalize"})
	if err != nil {
		t.Fatal(err)
	}
	nFacts, _ := manifest.Parse(nst.After.Mod)
	if _, ok := nFacts.Require["example.com/inner"]; ok {
		t.Fatalf("tidy kept inner:\n%s", nst.After.Mod)
	}
	norm := manifest.VerifyNormalized(baseFacts, nFacts, target, closure)
	candBL, err := manifest.BuildList(ctx, sb1, nst)
	if err != nil {
		t.Fatal(err)
	}
	vs := manifest.VerifyBuildList(baseBL, candBL, target, closure)
	if norm.OK() || norm.Violations[0].Code != manifest.CodeTargetNotSet {
		t.Fatalf("Gate B manifests %+v", norm)
	}
	if len(vs) != 1 || vs[0].Code != manifest.CodeTargetNotInBuildList || !strings.Contains(vs[0].Detail, "selects example.com/inner v1.0.0, want v1.0.1; the version before the change is selected again") {
		t.Fatalf("build-list rules %+v (build list %v)", vs, candBL)
	}
}
