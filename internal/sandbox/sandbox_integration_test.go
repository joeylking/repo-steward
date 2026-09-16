//go:build integration

package sandbox_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/joeylking/repo-steward/internal/sandbox"
	"github.com/joeylking/repo-steward/internal/sandbox/dockerapi"
	"github.com/joeylking/repo-steward/internal/testtmp"
	"github.com/joeylking/repo-steward/internal/toolchain"
)

// testImage is the pinned toolchain image the integration tests use. It must
// be present; the tests never pull.
var testImage = toolchain.Table[0].Ref()

var ctx = context.Background()

func newSandbox(t *testing.T, proxy bool) (*sandbox.Docker, sandbox.Config) {
	t.Helper()
	socket, err := dockerapi.DefaultSocket()
	if err != nil {
		t.Fatalf("integration tests require a Docker-compatible engine: %v", err)
	}
	root := testtmp.Dir(t)
	cfg := sandbox.Config{
		Image:         testImage,
		SourceDir:     filepath.Join(root, "src"),
		CacheDir:      filepath.Join(root, "cache"),
		BuildCacheDir: filepath.Join(root, "gocache"),
	}
	if proxy {
		cfg.ProxyDir = filepath.Join(root, "proxy")
		os.MkdirAll(cfg.ProxyDir, 0o755)
	}
	os.MkdirAll(cfg.SourceDir, 0o755)
	os.WriteFile(filepath.Join(cfg.SourceDir, "hello.txt"), []byte("hello\n"), 0o644)
	sb, err := sandbox.NewDocker(cfg, socket)
	if err != nil {
		t.Fatal(err)
	}
	if err := sb.EnsureImage(ctx, false); err != nil {
		t.Fatalf("%v (pull it once: docker pull %s)", err, testImage)
	}
	if err := sb.Probe(ctx); err != nil {
		t.Fatal(err)
	}
	return sb, cfg
}

// A directory the engine cannot see must be detected, never validated
// against silently. On macOS the default temp directory is such a path for
// VM-backed engines; on engines that share everything this test only checks
// that the probe passes.
func TestProbe_DetectsUnsharedDirectory(t *testing.T) {
	socket, err := dockerapi.DefaultSocket()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir() // deliberately the default temp location
	cfg := sandbox.Config{Image: testImage, SourceDir: filepath.Join(root, "src"), CacheDir: filepath.Join(root, "cache"), BuildCacheDir: filepath.Join(root, "gocache")}
	os.MkdirAll(cfg.SourceDir, 0o755)
	os.WriteFile(filepath.Join(cfg.SourceDir, "hello.txt"), []byte("hello\n"), 0o644)
	sb, err := sandbox.NewDocker(cfg, socket)
	if err != nil {
		t.Fatal(err)
	}
	err = sb.Probe(ctx)
	if err == nil {
		t.Log("engine shares the default temp directory; probe passed")
		return
	}
	if !errors.Is(err, sandbox.ErrMountUnavailable) {
		t.Fatalf("probe error = %v, want ErrMountUnavailable", err)
	}
}

func run(t *testing.T, sb *sandbox.Docker, p sandbox.Profile, script string) sandbox.ExecResult {
	t.Helper()
	res, err := sb.Run(ctx, sandbox.ExecSpec{Profile: p, Argv: []string{"bash", "-c", script}, Timeout: 60 * time.Second, RunID: "test", StepID: t.Name()})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestExecute_NoNetwork(t *testing.T) {
	sb, _ := newSandbox(t, false)
	res := run(t, sb, sandbox.Execute, `ip -o link 2>/dev/null | grep -v ' lo:' ; (exec 3<>/dev/tcp/1.1.1.1/80) 2>/dev/null && echo CONNECTED || echo NO_NET; echo GOPROXY=$GOPROXY`)
	out := string(res.Stdout)
	if !strings.Contains(out, "NO_NET") || strings.Contains(out, "CONNECTED") {
		t.Fatalf("execute profile has network:\n%s", out)
	}
	if !strings.Contains(out, "GOPROXY=off") {
		t.Fatalf("GOPROXY not off:\n%s", out)
	}
	if strings.Contains(out, "eth0") {
		t.Fatalf("execute profile has a non-loopback interface:\n%s", out)
	}
}

func TestExecute_SourceAndCacheReadOnly(t *testing.T) {
	sb, cfg := newSandbox(t, false)
	res := run(t, sb, sandbox.Execute, `touch /work/x 2>&1; touch /cache/x 2>&1; touch /usr/x 2>&1; touch /gocache/ok && echo GOCACHE_WRITABLE; touch /tmp/ok && echo TMP_WRITABLE`)
	out := string(res.Stdout) + string(res.Stderr)
	if strings.Count(out, "Read-only file system") < 3 {
		t.Fatalf("expected three read-only failures:\n%s", out)
	}
	if !strings.Contains(out, "GOCACHE_WRITABLE") || !strings.Contains(out, "TMP_WRITABLE") {
		t.Fatalf("writable locations missing:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(cfg.SourceDir, "x")); err == nil {
		t.Fatal("source directory was modified")
	}
	if _, err := os.Stat(filepath.Join(cfg.BuildCacheDir, "ok")); err != nil {
		t.Fatal("build cache write did not reach the host:", err)
	}
}

func TestAcquire_CacheWritableAndProxyMounted(t *testing.T) {
	sb, cfg := newSandbox(t, true)
	os.WriteFile(filepath.Join(cfg.ProxyDir, "marker"), []byte("p"), 0o644)
	res := run(t, sb, sandbox.Acquire, `mkdir -p /cache/mod && echo x > /cache/mod/ok && cat /proxy/marker && echo GOPROXY=$GOPROXY GOSUMDB=$GOSUMDB; touch /work/x 2>&1; (exec 3<>/dev/tcp/1.1.1.1/80) 2>/dev/null && echo CONNECTED || echo NO_NET`)
	out := string(res.Stdout) + string(res.Stderr)
	if res.ExitCode != 0 && !strings.Contains(out, "Read-only") {
		t.Fatalf("exit %d:\n%s", res.ExitCode, out)
	}
	if _, err := os.Stat(filepath.Join(cfg.CacheDir, "mod", "ok")); err != nil {
		t.Fatalf("cache write did not reach host: %v\n%s", err, out)
	}
	for _, want := range []string{"p", "GOPROXY=file:///proxy GOSUMDB=off", "Read-only file system", "NO_NET"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}

func TestEnvironmentIsBuiltFromScratch(t *testing.T) {
	t.Setenv("REPO_STEWARD_SENTINEL", "leak")
	t.Setenv("GITHUB_TOKEN", "leak")
	sb, _ := newSandbox(t, false)
	res := run(t, sb, sandbox.Execute, `env | sort; id -u`)
	out := string(res.Stdout)
	if strings.Contains(out, "leak") || strings.Contains(out, "SENTINEL") {
		t.Fatalf("host environment leaked:\n%s", out)
	}
	for _, want := range []string{"GOTOOLCHAIN=local", "CGO_ENABLED=0", "GOFLAGS=-mod=readonly", "GOMODCACHE=/cache/mod", "\n65534\n"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	for _, line := range strings.Split(out, "\n") {
		if i := strings.Index(line, "="); i > 0 {
			k := line[:i]
			switch {
			case strings.HasPrefix(k, "GO"), k == "PATH", k == "HOME", k == "TMPDIR", k == "CGO_ENABLED", k == "GIT_TERMINAL_PROMPT", k == "HOSTNAME", k == "PWD", k == "SHLVL", k == "_":
			default:
				t.Fatalf("unexpected environment variable %q", k)
			}
		}
	}
}

func TestTimeoutKillsAndRemoves(t *testing.T) {
	sb, _ := newSandbox(t, false)
	start := time.Now()
	res, err := sb.Run(ctx, sandbox.ExecSpec{Profile: sandbox.Execute, Argv: []string{"sleep", "60"}, Timeout: 2 * time.Second, RunID: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.TimedOut || time.Since(start) > 20*time.Second {
		t.Fatalf("timeout not enforced: %+v after %s", res, time.Since(start))
	}
	list, err := sb.Client().ContainerListByLabel(ctx, sandbox.Label+"=1")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range list {
		if c.ID == res.ContainerID {
			t.Fatal("container not removed after timeout")
		}
	}
}

func TestOutputCapMarksTruncation(t *testing.T) {
	sb, _ := newSandbox(t, false)
	res, err := sb.Run(ctx, sandbox.ExecSpec{Profile: sandbox.Execute, Argv: []string{"bash", "-c", "head -c 200000 /dev/zero | tr '\\0' a; echo err >&2"}, Timeout: 30 * time.Second, OutputCap: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if !res.StdoutTruncated || len(res.Stdout) != 1000 || res.StderrTruncated || !bytes.Equal(res.Stderr, []byte("err\n")) {
		t.Fatalf("cap not applied: len=%d truncated=%v stderr=%q", len(res.Stdout), res.StdoutTruncated, res.Stderr)
	}
}

func TestGoToolchainRunsOffline(t *testing.T) {
	sb, _ := newSandbox(t, false)
	res := run(t, sb, sandbox.Execute, `go version && go env GOTOOLCHAIN GOFLAGS GOPROXY`)
	if res.ExitCode != 0 || !strings.Contains(string(res.Stdout), "go1.22") {
		t.Fatalf("exit %d:\n%s%s", res.ExitCode, res.Stdout, res.Stderr)
	}
}

func TestReapOrphans(t *testing.T) {
	sb, _ := newSandbox(t, false)
	id, err := sb.Client().ContainerCreate(ctx, dockerapi.ContainerConfig{
		Image: testImage, Cmd: []string{"sleep", "300"},
		Labels:     map[string]string{sandbox.Label: "1", sandbox.Label + ".run": "orphan"},
		HostConfig: dockerapi.HostConfig{NetworkMode: "none"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := sb.Client().ContainerStart(ctx, id); err != nil {
		t.Fatal(err)
	}
	removed, err := sb.ReapOrphans(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range removed {
		if r == id {
			found = true
		}
	}
	if !found {
		t.Fatalf("orphan %s not reaped; removed %v", id, removed)
	}
	list, _ := sb.Client().ContainerListByLabel(ctx, sandbox.Label+"=1")
	for _, c := range list {
		if c.ID == id {
			t.Fatal("orphan still listed")
		}
	}
}
