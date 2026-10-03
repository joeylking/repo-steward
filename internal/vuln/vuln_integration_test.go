//go:build integration

package vuln_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/joeylking/repo-steward/internal/fixture"
	"github.com/joeylking/repo-steward/internal/modproxy"
	"github.com/joeylking/repo-steward/internal/sandbox"
	"github.com/joeylking/repo-steward/internal/sandbox/dockerapi"
	"github.com/joeylking/repo-steward/internal/testtmp"
	"github.com/joeylking/repo-steward/internal/toolchain"
	"github.com/joeylking/repo-steward/internal/vuln"
)

// TestScan_FixtureAdvisoryThroughSandbox builds the pinned scanner in the
// acquire profile through the public module proxy, scans patch-safe in the
// execute profile against the fixture database, and checks that the
// planted advisory is reported as called and that it is gone at the fixed
// version.
//
// The sandbox has no tools or database mount yet, so this test places both
// under the module cache directory, which the acquire profile mounts
// writable and the execute profile read-only. The wiring batch should give
// them their own mounts.
//
// Building the scanner reaches proxy.golang.org and sum.golang.org, which
// no other integration test does, so the test runs only when
// REPO_STEWARD_VULN_NETWORK=1.
func TestScan_FixtureAdvisoryThroughSandbox(t *testing.T) {
	if os.Getenv("REPO_STEWARD_VULN_NETWORK") != "1" {
		t.Skip("set REPO_STEWARD_VULN_NETWORK=1: this test builds govulncheck through the public module proxy")
	}
	ctx := context.Background()
	img := toolchain.Table[0]
	pin, err := vuln.ScannerFor(img.GoMinor)
	if err != nil {
		t.Fatal(err)
	}
	socket, err := dockerapi.DefaultSocket()
	if err != nil {
		t.Fatalf("integration tests require a Docker-compatible engine: %v", err)
	}
	root := testtmp.Dir(t)
	repo, err := fixture.Setup(ctx, "patch-safe", filepath.Join(root, "repo"))
	if err != nil {
		t.Fatal(err)
	}
	ix, err := modproxy.Build(filepath.Join(root, "proxy"))
	if err != nil {
		t.Fatal(err)
	}
	// The fixed tree: patch-safe with lib at v1.2.4, written on the host.
	fixed := filepath.Join(root, "fixed")
	if err := os.MkdirAll(fixed, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"main.go", "main_test.go"} {
		b, err := os.ReadFile(filepath.Join(repo.Path, name))
		if err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(fixed, name), b, 0o644)
	}
	os.WriteFile(filepath.Join(fixed, "go.mod"), []byte("module example.com/app\n\ngo 1.22\n\nrequire example.com/lib v1.2.4\n"), 0o644)
	sum, err := ix.GoSum(map[string]string{"example.com/lib": "v1.2.4"})
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(fixed, "go.sum"), []byte(sum), 0o644)

	cfg := sandbox.Config{Image: img.Ref(), SourceDir: repo.Path, CacheDir: filepath.Join(root, "cache"), BuildCacheDir: filepath.Join(root, "gocache")}
	online, err := sandbox.NewDocker(cfg, socket)
	if err != nil {
		t.Fatal(err)
	}
	if err := online.EnsureImage(ctx, false); err != nil {
		t.Fatalf("%v (pull it once: docker pull %s)", err, img.Ref())
	}
	if err := online.Probe(ctx); err != nil {
		t.Fatal(err)
	}
	cfg.ProxyDir = filepath.Join(root, "proxy")
	offline, err := sandbox.NewDocker(cfg, socket)
	if err != nil {
		t.Fatal(err)
	}

	// 1. Build the scanner (acquire, network through the proxy only).
	toolDir := "tools/go" + img.GoMinor + "-" + pin.Version
	res, err := online.Run(ctx, sandbox.ExecSpec{Profile: sandbox.Acquire, Argv: pin.InstallArgv("/cache/" + toolDir), Timeout: 5 * time.Minute, StepID: "vuln-build"})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("build exit %d: %s", res.ExitCode, res.Stderr)
	}
	id, err := vuln.InspectBinary(filepath.Join(cfg.CacheDir, toolDir, "govulncheck"))
	if err != nil {
		t.Fatal(err)
	}

	// 2. Populate the module cache for both trees (acquire, fixture proxy).
	for _, src := range []string{repo.Path, fixed} {
		res, err := offline.WithSource(src).Run(ctx, sandbox.ExecSpec{Profile: sandbox.Acquire, Argv: []string{"go", "mod", "download"}, Timeout: 2 * time.Minute, StepID: "vuln-download"})
		if err != nil || res.ExitCode != 0 {
			t.Fatalf("download %s: %v %s", src, err, res.Stderr)
		}
	}

	// 3. The database, written by the host into the read-only mount.
	dbName := "vulndb-fixture"
	dbDir := filepath.Join(cfg.CacheDir, dbName)
	if err := vuln.WriteFixtureDB(dbDir); err != nil {
		t.Fatal(err)
	}
	snap, err := vuln.Identify(dbDir, vuln.Limits{})
	if err != nil {
		t.Fatal(err)
	}

	scan := func(src string) *vuln.Scan {
		t.Helper()
		sb, err := offline.WithSource(src).WithFreshBuildCache()
		if err != nil {
			t.Fatal(err)
		}
		ver, err := sb.Run(ctx, sandbox.ExecSpec{Profile: sandbox.Execute, Argv: []string{"go", "env", "GOVERSION"}, Timeout: time.Minute, StepID: "vuln-goversion"})
		if err != nil || ver.ExitCode != 0 {
			t.Fatalf("go env: %v %s", err, ver.Stderr)
		}
		goVersion := strings.TrimSpace(string(ver.Stdout))
		if err := pin.Check(id, goVersion); err != nil {
			t.Fatal(err)
		}
		if err := vuln.Verify(dbDir, snap, vuln.Limits{}); err != nil {
			t.Fatal(err)
		}
		res, err := sb.Run(ctx, sandbox.ExecSpec{Profile: sandbox.Execute, Argv: vuln.ScanArgv("/cache/"+toolDir+"/govulncheck", "file:///cache/"+dbName), Timeout: 5 * time.Minute, StepID: "vuln-scan"})
		if err != nil {
			t.Fatal(err)
		}
		s := vuln.Parse(vuln.Output{Stdout: res.Stdout, Stderr: res.Stderr, ExitCode: res.ExitCode, StdoutTruncated: res.StdoutTruncated, StderrTruncated: res.StderrTruncated, TimedOut: res.TimedOut},
			vuln.Expect{ScannerVersion: pin.Version, DB: "file:///cache/" + dbName, DBModified: snap.Modified, GoVersion: goVersion})
		if !s.Conclusive {
			t.Fatalf("scan of %s inconclusive: %s\nstderr: %s", src, s.Reason, res.Stderr)
		}
		t.Logf("scan of %s took %s", filepath.Base(src), res.Duration)
		return s
	}

	base := scan(repo.Path)
	f, ok := base.Finding("GO-TEST-0001", "example.com/lib")
	if !ok || f.Level != vuln.LevelSymbol || f.FoundVersion != "v1.2.1" || f.FixedVersion != "v1.2.4" || f.CallPath() != "app.main (main.go:10) -> lib.Greet" {
		t.Fatalf("GO-TEST-0001 = %+v (found %v)", f, ok)
	}
	after := scan(fixed)
	d, err := vuln.Compare(base, after)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Resolved) != 1 || d.Resolved[0].OSV != "GO-TEST-0001" || len(d.Introduced) != 0 || len(d.Escalated) != 0 {
		t.Fatalf("delta = %+v", d)
	}
}
