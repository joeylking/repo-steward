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
