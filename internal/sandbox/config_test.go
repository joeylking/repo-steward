package sandbox

import (
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
	hc, _ = d.hostConfig(Execute)
	if hc.NetworkMode != "none" || !hc.ReadonlyRootfs || strings.Join(hc.Binds, " ") != "/s:/work:ro /c:/cache:ro /b:/gocache:rw" {
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
	if err != nil || strings.Join(hc.Binds, " ") != "/snap:/work:ro /c:/cache:rw /b:/gocache:rw /st:/staging:rw /p:/proxy:ro" || hc.NetworkMode != "none" {
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
