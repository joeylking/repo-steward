//go:build integration

package vulnscan_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/joeylking/repo-steward/internal/deps"
	"github.com/joeylking/repo-steward/internal/fixture"
	"github.com/joeylking/repo-steward/internal/modproxy"
	"github.com/joeylking/repo-steward/internal/sandbox"
	"github.com/joeylking/repo-steward/internal/sandbox/dockerapi"
	"github.com/joeylking/repo-steward/internal/testtmp"
	"github.com/joeylking/repo-steward/internal/toolchain"
	"github.com/joeylking/repo-steward/internal/vuln"
	"github.com/joeylking/repo-steward/internal/vulnscan"
)

// These tests share one data directory, so the scanner is built once per
// test binary, through the public module proxy and checksum database: the
// first test to need it builds it and every later one reuses it. The
// fixture proxy keeps the repository's own dependencies offline.

var ctx = context.Background()

var shared struct {
	root, data, proxy, vulndb, bin string
}

var img = toolchain.Table[0]

func TestMain(m *testing.M) {
	code := func() int {
		if err := os.MkdirAll(testtmp.Root(), 0o755); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		root, err := os.MkdirTemp(testtmp.Root(), "vulnscan-")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		defer testtmp.RemoveAll(root)
		shared.root, shared.data, shared.proxy, shared.vulndb, shared.bin = root, filepath.Join(root, "data"), filepath.Join(root, "proxy"), filepath.Join(root, "vulndb"), filepath.Join(root, "repo-steward")
		if _, err := modproxy.Build(shared.proxy); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		if err := vuln.WriteFixtureDB(shared.vulndb); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		if out, err := exec.Command("go", "build", "-o", shared.bin, "../../cmd/repo-steward").CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "build: %v\n%s", err, out)
			return 1
		}
		return m.Run()
	}()
	os.Exit(code)
}

// cliResult is one run of the built command line.
type cliResult struct {
	code   int
	report map[string]any
	stdout []byte
	stderr string
}

func cli(t *testing.T, args ...string) cliResult {
	t.Helper()
	cmd := exec.Command(shared.bin, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	r := cliResult{stdout: out, stderr: stderr.String()}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		r.code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("%v: %v\n%s", args, err, r.stderr)
	}
	if len(out) > 0 {
		if err := json.Unmarshal(out, &r.report); err != nil {
			t.Fatalf("%v: stdout is not JSON: %v\n%s", args, err, out)
		}
	}
	return r
}

func setupFixture(t *testing.T, name string) string {
	t.Helper()
	r, err := fixture.Setup(ctx, name, filepath.Join(testtmp.Dir(t), "repo"))
	if err != nil {
		t.Fatal(err)
	}
	return r.Path
}

// vulns runs the command on repo with the shared data directory, fixture
// proxy, and fixture database, plus any extra flags.
func vulns(t *testing.T, repo string, extra ...string) cliResult {
	t.Helper()
	args := append([]string{"vulns", repo, "-data-dir", shared.data, "-fixture-proxy", shared.proxy, "-vulndb", shared.vulndb}, extra...)
	r := cli(t, args...)
	if r.code == 1 && strings.Contains(r.stderr, "building golang.org/x/vuln") {
		t.Fatalf("building the scanner needs network access to proxy.golang.org and sum.golang.org, once per test binary; it failed:\n%s", r.stderr)
	}
	return r
}

func findings(t *testing.T, r cliResult, key string) []map[string]any {
	t.Helper()
	raw, ok := r.report[key].([]any)
	if !ok {
		t.Fatalf("report %s = %v", key, r.report[key])
	}
	var out []map[string]any
	for _, f := range raw {
		out = append(out, f.(map[string]any))
	}
	return out
}

// vulns on patch-safe reports GO-TEST-0001 at symbol level with its call
// path, and the standard-library advisory separately; after the baseline
// pipeline upgrades lib to v1.2.4, the advisory is gone.
func TestVulns_FixtureAdvisoryThenUpgradeRemovesIt(t *testing.T) {
	repo := setupFixture(t, "patch-safe")
	r := vulns(t, repo)
	if r.code != 4 || r.report["outcome"] != "findings" || r.report["conclusive"] != true {
		t.Fatalf("exit %d, report %v\n%s", r.code, r.report["outcome"], r.stderr)
	}
	tp, std := findings(t, r, "third_party"), findings(t, r, "stdlib")
	if len(tp) != 1 || len(std) != 1 {
		t.Fatalf("third party %v, stdlib %v", tp, std)
	}
	f := tp[0]
	if f["id"] != "GO-TEST-0001" || f["module"] != "example.com/lib" || f["found_version"] != "v1.2.1" || f["fixed_version"] != "v1.2.4" || f["level"] != "symbol" || f["call_path"] != "app.main (main.go:10) -> lib.Greet" {
		t.Fatalf("GO-TEST-0001 = %v", f)
	}
	if std[0]["id"] != "GO-TEST-0004" || std[0]["module"] != "stdlib" || std[0]["level"] != "package" {
		t.Fatalf("stdlib = %v", std)
	}
	sc := r.report["scanner"].(map[string]any)
	pin, _ := vuln.ScannerFor(img.GoMinor)
	if sc["version"] != pin.Version || len(sc["sha256"].(string)) != 64 || r.report["go_version"] == "" || r.report["image_ref"] != img.Ref() {
		t.Fatalf("scanner %v, go %v, image %v", sc, r.report["go_version"], r.report["image_ref"])
	}
	db := r.report["database"].(map[string]any)
	if db["source"] != "local" || db["modified"] != "2026-01-01T00:00:00Z" || !strings.HasPrefix(db["snapshot_id"].(string), "sha256:") {
		t.Fatalf("database = %v", db)
	}
	for _, want := range []string{"third-party dependencies: 1", "standard library: 1", "cannot fix them today", "only reports"} {
		if !strings.Contains(r.stderr, want) {
			t.Fatalf("summary lacks %q:\n%s", want, r.stderr)
		}
	}
	// The repository is unchanged.
	if out, err := exec.Command("git", "-C", repo, "status", "--porcelain").Output(); err != nil || len(out) != 0 {
		t.Fatalf("vulns changed the repository: %q %v", out, err)
	}

	// Upgrade through the baseline pipeline, then scan the proposal commit.
	m := cli(t, "maintain", repo, "-mode", "baseline", "-author", "Fixture Operator <op@example.invalid>", "-data-dir", filepath.Join(testtmp.Dir(t), "data"), "-fixture-proxy", shared.proxy)
	if m.code != 0 || m.report["outcome"] != "proposal_prepared" {
		t.Fatalf("maintain exit %d, outcome %v\n%s", m.code, m.report["outcome"], m.stderr)
	}
	prop := m.report["proposal"].(map[string]any)
	gitDir := m.report["workspace"].(map[string]any)["git_dir"].(string)
	fixed := filepath.Join(testtmp.Dir(t), "fixed")
	for _, args := range [][]string{{"init", "-q", fixed}, {"-C", fixed, "fetch", "-q", gitDir, prop["proposal_ref"].(string)}, {"-C", fixed, "checkout", "-q", "FETCH_HEAD"}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	after := vulns(t, fixed)
	if after.code != 0 || after.report["outcome"] != "clean" || after.report["head_commit"] != prop["head_commit"] {
		t.Fatalf("after upgrade: exit %d, outcome %v, head %v\n%s", after.code, after.report["outcome"], after.report["head_commit"], after.stderr)
	}
	if tp := findings(t, after, "third_party"); len(tp) != 0 {
		t.Fatalf("third party after upgrade = %v", tp)
	}
	if std := findings(t, after, "stdlib"); len(std) != 1 || std[0]["id"] != "GO-TEST-0004" {
		t.Fatalf("stdlib after upgrade = %v", std)
	}
	if after.report["scanner"].(map[string]any)["built_this_run"] != false {
		t.Fatal("the scanner was rebuilt instead of reused")
	}
}

// A scan that does not complete is inconclusive and exits 3, with the
// parser's reason verbatim and no findings.
func TestVulns_InconclusiveExits3(t *testing.T) {
	repo := setupFixture(t, "patch-safe")
	r := vulns(t, repo, "-scan-timeout", "1ms")
	if r.code != 3 || r.report["outcome"] != "inconclusive" || r.report["conclusive"] != false || r.report["reason"] != "scanner timed out" {
		t.Fatalf("exit %d, report %v %v %v\n%s", r.code, r.report["outcome"], r.report["conclusive"], r.report["reason"], r.stderr)
	}
	if r.report["third_party"] != nil || r.report["stdlib"] != nil || !strings.Contains(r.stderr, "inconclusive: scanner timed out") {
		t.Fatalf("inconclusive report lists findings or misses the reason: %v %v\n%s", r.report["third_party"], r.report["stdlib"], r.stderr)
	}
}

// A provisioned scanner that no longer matches its record is refused with
// exit 1 before any scan, and is not rebuilt.
func TestVulns_TamperedScannerRefused(t *testing.T) {
	repo := setupFixture(t, "patch-safe")
	if r := vulns(t, repo); r.code != 4 {
		t.Fatalf("provisioning run exit %d\n%s", r.code, r.stderr)
	}
	pin, _ := vuln.ScannerFor(img.GoMinor)
	name := vulnscan.ScannerDirName(img, pin)
	data := filepath.Join(testtmp.Dir(t), "data")
	if err := os.MkdirAll(filepath.Join(data, "tools", name), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"govulncheck", "scanner.json"} {
		copyFile(t, filepath.Join(shared.data, "tools", name, f), filepath.Join(data, "tools", name, f))
	}
	bin := filepath.Join(data, "tools", name, "govulncheck")
	f, err := os.OpenFile(bin, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.Write([]byte{0})
	f.Close()
	before, _ := os.ReadFile(bin)
	r := cli(t, "vulns", repo, "-data-dir", data, "-fixture-proxy", shared.proxy, "-vulndb", shared.vulndb)
	if r.code != 1 || !strings.Contains(r.stderr, "does not match its pin") || !strings.Contains(r.stderr, "recorded") || len(r.stdout) != 0 {
		t.Fatalf("exit %d, stdout %s\n%s", r.code, r.stdout, r.stderr)
	}
	if after, _ := os.ReadFile(bin); string(after) != string(before) {
		t.Fatal("the tampered scanner was replaced: a mismatch must not rebuild")
	}
}

// A database that is not a database is refused before any container runs.
func TestVulns_MalformedDatabaseRefused(t *testing.T) {
	repo := setupFixture(t, "patch-safe")
	db := filepath.Join(testtmp.Dir(t), "vulndb")
	if err := vuln.WriteFixtureDB(db); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(db, "ID", "notes.txt"), []byte("x"), 0o644)
	r := cli(t, "vulns", repo, "-data-dir", shared.data, "-fixture-proxy", shared.proxy, "-vulndb", db)
	if r.code != 1 || !strings.Contains(r.stderr, "unexpected file ID/notes.txt") {
		t.Fatalf("exit %d\n%s", r.code, r.stderr)
	}
}

// The default path fetches the public database into the data directory and
// reuses the snapshot on the next run. It needs vuln.go.dev.
func TestVulns_PublicDatabase(t *testing.T) {
	repo := setupFixture(t, "patch-safe")
	run := func() cliResult {
		r := cli(t, "vulns", repo, "-data-dir", shared.data, "-fixture-proxy", shared.proxy)
		if r.code == 1 && strings.Contains(r.stderr, "vuln.go.dev") {
			t.Fatalf("fetching the public database needs network access to https://vuln.go.dev:\n%s", r.stderr)
		}
		return r
	}
	first := run()
	// The fixture modules are in no public advisory; the image's standard
	// library may be.
	if first.code != 0 || first.report["outcome"] != "clean" || len(findings(t, first, "third_party")) != 0 {
		t.Fatalf("exit %d, outcome %v\n%s", first.code, first.report["outcome"], first.stderr)
	}
	db := first.report["database"].(map[string]any)
	if db["source"] != vuln.DefaultBaseURL || !strings.HasPrefix(db["dir"].(string), filepath.Join(shared.data, "vulndb")+string(filepath.Separator)) {
		t.Fatalf("database = %v", db)
	}
	second := run()
	db2 := second.report["database"].(map[string]any)
	if second.code != 0 {
		t.Fatalf("second run: exit %d\n%s", second.code, second.stderr)
	}
	// The database may have been republished between the two fetches; an
	// identical snapshot must be the same directory, reused.
	if db2["snapshot_id"] == db["snapshot_id"] && (db2["dir"] != db["dir"] || db2["reused"] != true) {
		t.Fatalf("an identical snapshot was not reused: %v then %v", db, db2)
	}
}

// --- API-level tests: the scan function against a prepared sandbox. ---

var scannerOnce struct {
	sync.Once
	s   *vulnscan.Scanner
	err error
}

// scanSandbox returns a sandbox for the fixture repo with its module cache
// populated through the fixture proxy, and the shared scanner.
func scanSandbox(t *testing.T, repo string) (*sandbox.Docker, *vulnscan.Scanner) {
	t.Helper()
	socket, err := dockerapi.DefaultSocket()
	if err != nil {
		t.Fatalf("integration tests require a Docker-compatible engine: %v", err)
	}
	root := testtmp.Dir(t)
	sb, err := sandbox.NewDocker(sandbox.Config{Image: img.Ref(), SourceDir: repo, CacheDir: filepath.Join(root, "cache"), BuildCacheDir: filepath.Join(root, "gocache"), ProxyDir: shared.proxy}, socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sb.RemoveBuildCaches(context.Background()) })
	if err := sb.EnsureImage(ctx, false); err != nil {
		t.Fatalf("%v (pull it once: docker pull %s)", err, img.Ref())
	}
	if err := sb.Probe(ctx); err != nil {
		t.Fatal(err)
	}
	if err := deps.Download(ctx, sb); err != nil {
		t.Fatal(err)
	}
	scannerOnce.Do(func() {
		gv, err := vulnscan.GoVersion(ctx, sb)
		if err != nil {
			scannerOnce.err = err
			return
		}
		scannerOnce.s, scannerOnce.err = vulnscan.EnsureScanner(ctx, sb, filepath.Join(shared.data, "tools"), img, gv)
	})
	if scannerOnce.err != nil {
		t.Fatalf("scanner (building it needs network access to proxy.golang.org and sum.golang.org): %v", scannerOnce.err)
	}
	return sb, scannerOnce.s
}

// The scan function reports the planted advisory through the real mounts
// and Compare sees it resolved in a tree at the fixed version, a pinned
// fixture already at lib v1.2.4 that calls lib.Greet the same way.
func TestScan_FixtureAdvisoryThroughMounts(t *testing.T) {
	db, err := vulnscan.LocalDatabase(shared.vulndb)
	if err != nil {
		t.Fatal(err)
	}
	scan := func(name string) *vulnscan.Result {
		sb, s := scanSandbox(t, setupFixture(t, name))
		res, err := vulnscan.Scan(ctx, sb, s, db, vulnscan.ScanOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if !res.Scan.Conclusive {
			t.Fatalf("%s: inconclusive: %s\n%s", name, res.Scan.Reason, res.Stderr)
		}
		if !strings.HasPrefix(res.GoVersion, "go1.22.") || len(res.Stdout) == 0 {
			t.Fatalf("%s: go %q, %d bytes of output", name, res.GoVersion, len(res.Stdout))
		}
		return res
	}
	base := scan("patch-safe")
	f, ok := base.Scan.Finding("GO-TEST-0001", "example.com/lib")
	if !ok || f.Level != vuln.LevelSymbol || f.FoundVersion != "v1.2.1" || f.FixedVersion != "v1.2.4" || f.CallPath() != "app.main (main.go:10) -> lib.Greet" {
		t.Fatalf("GO-TEST-0001 = %+v (found %v)", f, ok)
	}
	after := scan("breaking-minor")
	d, err := vuln.Compare(base.Scan, after.Scan)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Resolved) != 1 || d.Resolved[0].OSV != "GO-TEST-0001" || len(d.Introduced) != 0 || len(d.Escalated) != 0 {
		t.Fatalf("delta = %+v", d)
	}
}

// Code in an execute container cannot change the scanner or the database,
// and both still verify afterwards.
func TestScan_ExecuteCannotWriteScannerOrDatabase(t *testing.T) {
	sb, s := scanSandbox(t, setupFixture(t, "patch-safe"))
	db, err := vulnscan.LocalDatabase(shared.vulndb)
	if err != nil {
		t.Fatal(err)
	}
	msb, err := sb.WithScanMounts(s.Dir, db.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if msb, err = msb.WithFreshBuildCache(); err != nil {
		t.Fatal(err)
	}
	res, err := msb.Run(ctx, sandbox.ExecSpec{Profile: sandbox.Execute, Argv: []string{"sh", "-c", `
		echo x >> /tools/govulncheck 2>&1; rm -f /tools/scanner.json 2>&1; touch /tools/new 2>&1
		echo x > /vulndb/index/db.json 2>&1; rm -f /vulndb/ID/GO-TEST-0001.json 2>&1; touch /vulndb/new 2>&1
		echo DONE`}, Timeout: time.Minute, StepID: t.Name()})
	if err != nil {
		t.Fatal(err)
	}
	out := string(res.Stdout) + string(res.Stderr)
	// The binary is also mode 0555, so the shell may refuse it with
	// "Permission denied" before the read-only mount is reached.
	if strings.Count(out, "Read-only file system")+strings.Count(out, "Permission denied") != 6 || !strings.Contains(out, "DONE") {
		t.Fatalf("writes were not all refused:\n%s", out)
	}
	gv, err := vulnscan.GoVersion(ctx, sb)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Verify(gv); err != nil {
		t.Fatal(err)
	}
	if err := vuln.Verify(db.Dir, db.ID, vuln.Limits{}); err != nil {
		t.Fatal(err)
	}
}

// A scanner or database changed after it was identified is refused before
// the scan runs: Scan returns an error and no result.
func TestScan_TamperedInputsRefusedBeforeScan(t *testing.T) {
	sb, s := scanSandbox(t, setupFixture(t, "patch-safe"))
	root := testtmp.Dir(t)

	// A private copy of the scanner, then changed.
	dir := filepath.Join(root, "tools", filepath.Base(s.Dir))
	os.MkdirAll(dir, 0o755)
	for _, f := range []string{"govulncheck", "scanner.json"} {
		copyFile(t, filepath.Join(s.Dir, f), filepath.Join(dir, f))
	}
	copied, err := vulnscan.LoadScanner(dir, img)
	if err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(root, "vulndb")
	if err := vuln.WriteFixtureDB(db); err != nil {
		t.Fatal(err)
	}
	good, err := vulnscan.LocalDatabase(db)
	if err != nil {
		t.Fatal(err)
	}
	f, _ := os.OpenFile(copied.Path, os.O_APPEND|os.O_WRONLY, 0)
	f.Write([]byte("tampered"))
	f.Close()
	res, err := vulnscan.Scan(ctx, sb, copied, good, vulnscan.ScanOptions{})
	if !errors.Is(err, vuln.ErrScannerMismatch) || res != nil {
		t.Fatalf("tampered scanner: %v, %+v", err, res)
	}

	// The shared scanner with a database changed after it was identified.
	os.WriteFile(filepath.Join(db, "ID", "GO-TEST-0001.json"), []byte(`{"id":"GO-TEST-0001","withdrawn":"2026-01-01T00:00:00Z"}`), 0o644)
	res, err = vulnscan.Scan(ctx, sb, s, good, vulnscan.ScanOptions{})
	if !errors.Is(err, vuln.ErrSnapshotMismatch) || res != nil {
		t.Fatalf("tampered database: %v, %+v", err, res)
	}
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}
