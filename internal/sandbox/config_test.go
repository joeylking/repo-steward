package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigValidate(t *testing.T) {
	base := func() Config {
		return Config{Image: "img", SourceDir: "/s", CacheDir: "/c", BuildCacheDir: "/b"}
	}
	c := base()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.GoProxy != "https://proxy.golang.org" || c.GoSumDB != "sum.golang.org" || c.User != "65534:65534" {
		t.Fatalf("defaults not applied: %+v", c)
	}
	bad := map[string]func(*Config){
		"direct fallback":      func(c *Config) { c.GoProxy = "https://proxy.golang.org,direct" },
		"proxy off":            func(c *Config) { c.GoProxy = "off" },
		"sumdb off no fixture": func(c *Config) { c.GoSumDB = "off" },
		"relative source":      func(c *Config) { c.SourceDir = "src" },
		"missing image":        func(c *Config) { c.Image = "" },
		"relative proxy dir":   func(c *Config) { c.ProxyDir = "proxy" },
	}
	for name, mutate := range bad {
		c := base()
		mutate(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	c = base()
	c.ProxyDir = "/p"
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	d := &Docker{cfg: c}
	env := strings.Join(d.env(Acquire), " ")
	if !strings.Contains(env, "GOPROXY=file:///proxy") || !strings.Contains(env, "GOSUMDB=off") {
		t.Fatalf("fixture acquire env = %s", env)
	}
	hc, _ := d.hostConfig(Acquire)
	if hc.NetworkMode != "none" {
		t.Fatalf("fixture acquire network = %s", hc.NetworkMode)
	}
	if _, err := d.hostConfig(Execute); err == nil {
		t.Fatal("execute without a fresh build cache must fail")
	}
	hc, _ = (&Docker{cfg: c, execCache: "/b/execute-1"}).hostConfig(Execute)
	if hc.NetworkMode != "none" || !hc.ReadonlyRootfs || strings.Join(hc.Binds, " ") != "/s:/work:ro /c:/cache:ro /b/execute-1:/gocache:rw" {
		t.Fatalf("execute host config = %+v", hc)
	}
	if !strings.Contains(hc.Tmpfs["/tmp"], "exec") || !strings.Contains(hc.Tmpfs["/tmp"], "nosuid") {
		t.Fatalf("tmpfs options = %q", hc.Tmpfs["/tmp"])
	}
	if _, err := d.hostConfig(Mutate); err == nil {
		t.Fatal("mutate without staging must fail")
	}
	dm := d.WithStaging("/st").WithSource("/snap")
	hc, err := dm.hostConfig(Mutate)
	if err != nil || strings.Join(hc.Binds, " ") != "/snap:/work:ro /c:/cache:rw /b/acquire:/gocache:rw /st:/staging:rw /p:/proxy:ro" || hc.NetworkMode != "none" {
		t.Fatalf("mutate host config = %+v, %v", hc, err)
	}
	if d.cfg.SourceDir != "/s" {
		t.Fatal("WithSource mutated the original")
	}
	env = strings.Join(d.env(Execute), " ")
	if !strings.Contains(env, "GOPROXY=off") || !strings.Contains(env, "GOFLAGS=-mod=readonly") {
		t.Fatalf("execute env = %s", env)
	}
}

// Each validation's execute containers mount a build cache that is new,
// empty, owned by no other validation, and never mounted by acquire or
// mutate; deriving a sandbox for another source or staging keeps it.
func TestFreshBuildCache_IsolatesValidations(t *testing.T) {
	root := t.TempDir()
	cfg := Config{Image: "img", SourceDir: "/s", CacheDir: filepath.Join(root, "mod"), BuildCacheDir: filepath.Join(root, "gocache")}
	d, err := NewDocker(cfg, filepath.Join(root, "no.sock"))
	if err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(cfg.BuildCacheDir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("build cache root: %v %v, want owner-only", fi.Mode(), err)
	}
	first, err := d.WithFreshBuildCache()
	if err != nil {
		t.Fatal(err)
	}
	// Code executed in the first validation leaves a marker behind.
	if err := os.WriteFile(filepath.Join(first.ExecuteCacheDir(), "marker"), []byte("x"), 0o666); err != nil {
		t.Fatal(err)
	}
	second, err := d.WithFreshBuildCache()
	if err != nil {
		t.Fatal(err)
	}
	gocache := func(sb *Docker, p Profile) string {
		t.Helper()
		if p == Mutate {
			sb = sb.WithStaging("/st")
		}
		hc, err := sb.hostConfig(p)
		if err != nil {
			t.Fatal(err)
		}
		for _, b := range hc.Binds {
			if strings.HasSuffix(b, ":/gocache:rw") {
				return strings.TrimSuffix(b, ":/gocache:rw")
			}
		}
		t.Fatalf("%s: no build cache bind in %v", p, hc.Binds)
		return ""
	}
	a, b := gocache(first, Execute), gocache(second, Execute)
	if a == b || filepath.Dir(a) != cfg.BuildCacheDir || filepath.Dir(b) != cfg.BuildCacheDir {
		t.Fatalf("execute caches %s and %s, want distinct directories under %s", a, b, cfg.BuildCacheDir)
	}
	entries, err := os.ReadDir(b)
	if err != nil || len(entries) != 0 {
		t.Fatalf("second validation's cache holds %v (%v), want empty", entries, err)
	}
	if fi, err := os.Stat(b); err != nil || fi.Mode().Perm() != 0o777 {
		t.Fatalf("execute cache mode %v %v, want 0777 for the container user", fi.Mode(), err)
	}
	for _, p := range []Profile{Acquire, Mutate} {
		if got := gocache(second, p); got == a || got == b || strings.HasPrefix(a, got+string(filepath.Separator)) || strings.HasPrefix(b, got+string(filepath.Separator)) {
			t.Fatalf("%s mounts %s, which reaches an execute cache", p, got)
		}
	}
	if got := gocache(second.WithSource("/snap"), Execute); got != b {
		t.Fatalf("WithSource dropped the execute cache: %s", got)
	}
}

// The tools and database mounts are optional: absent, every profile mounts
// exactly what it did before; present, only execute mounts them, and only
// read-only. Relative paths are refused by Validate and WithScanMounts.
func TestScanMounts_ExecuteOnlyAndReadOnly(t *testing.T) {
	for name, mutate := range map[string]func(*Config){
		"relative tools":  func(c *Config) { c.ToolsDir = "tools" },
		"relative vulndb": func(c *Config) { c.VulnDBDir = "db" },
	} {
		c := Config{Image: "img", SourceDir: "/s", CacheDir: "/c", BuildCacheDir: "/b"}
		mutate(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	c := Config{Image: "img", SourceDir: "/s", CacheDir: "/c", BuildCacheDir: "/b", ToolsDir: "/t", VulnDBDir: "/v"}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	d := &Docker{cfg: c, execCache: "/b/execute-1", tool: &toolDirs{bin: "/b/tool-bin-1", mod: "/b/tool-mod-1", cache: "/b/tool-cache-1"}}
	hc, err := d.hostConfig(Execute)
	if err != nil || strings.Join(hc.Binds, " ") != "/s:/work:ro /c:/cache:ro /b/execute-1:/gocache:rw /t:/tools:ro /v:/vulndb:ro" {
		t.Fatalf("execute binds = %v, %v", hc.Binds, err)
	}
	for _, p := range []Profile{Acquire, Mutate, Tool} {
		hc, err := d.WithStaging("/st").hostConfig(p)
		if err != nil {
			t.Fatal(err)
		}
		for _, b := range hc.Binds {
			if strings.HasPrefix(b, "/t:") || strings.HasPrefix(b, "/v:") {
				t.Fatalf("%s mounts %s", p, b)
			}
		}
	}
	if _, err := d.WithScanMounts("rel", ""); err == nil {
		t.Fatal("WithScanMounts accepted a relative path")
	}
	plain := &Docker{cfg: Config{Image: "img", SourceDir: "/s", CacheDir: "/c", BuildCacheDir: "/b"}, execCache: "/b/execute-1"}
	scan, err := plain.WithScanMounts("/t2", "/v2")
	if err != nil {
		t.Fatal(err)
	}
	if hc, _ := scan.hostConfig(Execute); strings.Join(hc.Binds[3:], " ") != "/t2:/tools:ro /v2:/vulndb:ro" {
		t.Fatalf("WithScanMounts binds = %v", hc.Binds)
	}
	if hc, _ := plain.hostConfig(Execute); len(hc.Binds) != 3 {
		t.Fatalf("WithScanMounts changed the original: %v", hc.Binds)
	}
}

// The tool profile builds a trusted tool from the public proxy: no source,
// none of the repository's caches, its own fresh directories, network on,
// and proxy.golang.org with sum.golang.org even in fixture mode, under the
// same hardening as every other profile.
func TestToolProfile_IsolatedFromTheRepository(t *testing.T) {
	root := t.TempDir()
	cfg := Config{Image: "img", SourceDir: "/s", CacheDir: filepath.Join(root, "mod"), BuildCacheDir: filepath.Join(root, "gocache"), ProxyDir: "/p", ToolsDir: "/t", VulnDBDir: "/v"}
	d, err := NewDocker(cfg, filepath.Join(root, "no.sock"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.hostConfig(Tool); err == nil {
		t.Fatal("tool profile without WithToolBuild must fail")
	}
	b1, err := d.WithToolBuild()
	if err != nil {
		t.Fatal(err)
	}
	b2, err := d.WithToolBuild()
	if err != nil {
		t.Fatal(err)
	}
	hc, err := b1.hostConfig(Tool)
	if err != nil {
		t.Fatal(err)
	}
	if len(hc.Binds) != 3 {
		t.Fatalf("tool binds = %v", hc.Binds)
	}
	targets := map[string]string{}
	for _, b := range hc.Binds {
		parts := strings.Split(b, ":")
		if len(parts) != 3 || parts[2] != "rw" {
			t.Fatalf("bind %q", b)
		}
		if filepath.Dir(parts[0]) != cfg.BuildCacheDir {
			t.Fatalf("tool bind %s is not a fresh directory under the build cache root", parts[0])
		}
		fi, err := os.Stat(parts[0])
		if err != nil || fi.Mode().Perm() != 0o777 {
			t.Fatalf("%s: %v %v, want 0777 for the container user", parts[0], fi, err)
		}
		if entries, _ := os.ReadDir(parts[0]); len(entries) != 0 {
			t.Fatalf("%s not empty", parts[0])
		}
		targets[parts[1]] = parts[0]
	}
	if targets["/tools"] != b1.ToolOutputDir() || targets["/cache"] == "" || targets["/gocache"] == "" {
		t.Fatalf("tool mounts = %v", targets)
	}
	for _, b := range hc.Binds {
		for _, forbidden := range []string{cfg.SourceDir + ":", cfg.CacheDir + ":", "/p:", "/t:", "/v:", acquireCache(cfg) + ":"} {
			if strings.HasPrefix(b, forbidden) {
				t.Fatalf("tool profile mounts %s", b)
			}
		}
	}
	if b2.ToolOutputDir() == b1.ToolOutputDir() {
		t.Fatal("two tool builds share an output directory")
	}
	if hc.NetworkMode != "bridge" || !hc.ReadonlyRootfs || len(hc.CapDrop) != 1 || hc.CapDrop[0] != "ALL" || hc.Memory == 0 || hc.PidsLimit == nil || *hc.PidsLimit == 0 || hc.NanoCPUs == 0 {
		t.Fatalf("tool host config = %+v", hc)
	}
	env := strings.Join(b1.env(Tool), " ")
	for _, want := range []string{"GOPROXY=https://proxy.golang.org", "GOSUMDB=sum.golang.org", "GOBIN=/tools", "GOTOOLCHAIN=local", "CGO_ENABLED=0", "GOPRIVATE= ", "GONOSUMDB= "} {
		if !strings.Contains(env+" ", want) {
			t.Fatalf("tool env lacks %q: %s", want, env)
		}
	}
	if strings.Contains(env, "file:///proxy") || strings.Contains(env, "GOSUMDB=off") {
		t.Fatalf("tool env follows the fixture proxy: %s", env)
	}
	// The other profiles are unchanged by a tool build.
	if hc, _ := b1.hostConfig(Acquire); strings.Contains(strings.Join(hc.Binds, " "), "tool-") {
		t.Fatalf("acquire mounts a tool directory: %v", hc.Binds)
	}
}
