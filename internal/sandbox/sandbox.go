// Package sandbox runs toolchain commands in containers under fixed
// profiles. Tools choose a profile and arguments; the sandbox alone decides
// mounts, network, environment, user, and resource limits.
//
// Profiles:
//
//	acquire  go mod download and module metadata. Source read-only, module
//	         cache writable, network on (or off with a file proxy). Executes
//	         no repository code.
//	execute  build, vet, test, and offline go list. Source and module cache
//	         read-only, no network, build cache writable, tmpfs /tmp.
package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/joeylking/repo-steward/internal/sandbox/dockerapi"
)

// Profile selects a fixed permission set.
type Profile string

const (
	Acquire Profile = "acquire"
	Execute Profile = "execute"
)

// Label marks every container the sandbox creates, for orphan reaping.
const Label = "repo-steward"

// ExecSpec is what a caller may choose.
type ExecSpec struct {
	Profile Profile
	Argv    []string
	// Timeout bounds the command. The host enforces it by killing the
	// container; an in-container timeout slightly longer than this also
	// terminates the command if the host process dies.
	Timeout time.Duration
	// OutputCap caps each captured stream in bytes. Zero means DefaultOutputCap.
	OutputCap int
	// RunID and StepID become container labels.
	RunID, StepID string
}

// DefaultOutputCap is the per-stream capture limit.
const DefaultOutputCap = 4 << 20

// ExecResult is the outcome of one command.
type ExecResult struct {
	ExitCode        int
	Stdout, Stderr  []byte
	StdoutTruncated bool
	StderrTruncated bool
	TimedOut        bool
	Duration        time.Duration
	ContainerID     string
}

// Sandbox executes commands.
type Sandbox interface {
	Run(ctx context.Context, spec ExecSpec) (ExecResult, error)
}

// Config fixes everything a tool may not choose.
type Config struct {
	// Image is the toolchain image reference, ideally by digest.
	Image string
	// SourceDir is mounted read-only at /work in every profile.
	SourceDir string
	// CacheDir holds the module cache; writable in acquire, read-only in execute.
	CacheDir string
	// BuildCacheDir holds the Go build cache; writable in every profile and
	// discarded by the caller at the end of a run.
	BuildCacheDir string
	// ProxyDir, when set, is a file-based module proxy mounted read-only at
	// /proxy. The acquire profile then runs with no network and no checksum
	// database. Fixture use only.
	ProxyDir string
	// GoProxy is used when ProxyDir is empty. It must not include "direct".
	GoProxy string
	// GoSumDB is used when ProxyDir is empty.
	GoSumDB string
	// User is the uid:gid inside containers.
	User string
	// Resource limits.
	MemoryBytes int64
	NanoCPUs    int64
	PidsLimit   int64
	TmpfsSize   string
}

// Validate checks the configuration and applies defaults.
func (c *Config) Validate() error {
	if c.Image == "" {
		return errors.New("sandbox: image is required")
	}
	for name, dir := range map[string]string{"source": c.SourceDir, "cache": c.CacheDir, "build cache": c.BuildCacheDir} {
		if dir == "" {
			return fmt.Errorf("sandbox: %s directory is required", name)
		}
		if !filepath.IsAbs(dir) {
			return fmt.Errorf("sandbox: %s directory must be absolute: %s", name, dir)
		}
	}
	if c.ProxyDir == "" {
		if c.GoProxy == "" {
			c.GoProxy = "https://proxy.golang.org"
		}
		for _, part := range strings.Split(c.GoProxy, ",") {
			if strings.TrimSpace(part) == "direct" || strings.TrimSpace(part) == "off" {
				return fmt.Errorf("sandbox: GOPROXY %q must name proxies only", c.GoProxy)
			}
		}
		if c.GoSumDB == "" {
			c.GoSumDB = "sum.golang.org"
		}
		if c.GoSumDB == "off" {
			return errors.New("sandbox: checksum database may be disabled only with a fixture proxy directory")
		}
	} else if !filepath.IsAbs(c.ProxyDir) {
		return errors.New("sandbox: proxy directory must be absolute")
	}
	if c.User == "" {
		c.User = "65534:65534"
	}
	if c.MemoryBytes == 0 {
		c.MemoryBytes = 2 << 30
	}
	if c.NanoCPUs == 0 {
		c.NanoCPUs = 2e9
	}
	if c.PidsLimit == 0 {
		c.PidsLimit = 512
	}
	if c.TmpfsSize == "" {
		c.TmpfsSize = "512m"
	}
	return nil
}

// Docker is the container-backed Sandbox.
type Docker struct {
	cfg    Config
	client *dockerapi.Client
}

// NewDocker validates cfg, prepares host directories, and connects to the
// engine socket.
func NewDocker(cfg Config, socket string) (*Docker, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	// The module and build caches are written by an unprivileged container
	// user, so they must be world-writable on the host.
	for _, dir := range []string{cfg.CacheDir, cfg.BuildCacheDir} {
		if err := os.MkdirAll(dir, 0o777); err != nil {
			return nil, err
		}
		if err := os.Chmod(dir, 0o777); err != nil {
			return nil, err
		}
	}
	if socket == "" {
		s, err := dockerapi.DefaultSocket()
		if err != nil {
			return nil, err
		}
		socket = s
	}
	return &Docker{cfg: cfg, client: dockerapi.New(socket)}, nil
}

// Client exposes the engine client for tests and reaping.
func (d *Docker) Client() *dockerapi.Client { return d.client }

// ErrMountUnavailable means the engine cannot see the host directories. On
// VM-backed engines a bind mount of an unshared path silently becomes an
// empty directory inside the VM, which would make validation run against
// nothing. Probe turns that into a hard failure.
var ErrMountUnavailable = errors.New("sandbox: host directories are not visible to the container engine")

// Probe verifies that the cache and source directories are really shared
// with the engine and that the container user can write to the caches. It
// runs one short execute-profile and one acquire-profile container.
func (d *Docker) Probe(ctx context.Context) error {
	nonce := strconv.FormatInt(time.Now().UnixNano(), 36)
	marker := filepath.Join(d.cfg.CacheDir, ".repo-steward-probe")
	if err := os.WriteFile(marker, []byte(nonce), 0o644); err != nil {
		return err
	}
	defer os.Remove(marker)
	srcEntries, err := os.ReadDir(d.cfg.SourceDir)
	if err != nil {
		return err
	}
	var first string
	for _, e := range srcEntries {
		first = e.Name()
		break
	}
	script := "cat /cache/.repo-steward-probe && echo && test -e /work/" + shellQuote(first) + " && echo SRC_OK && echo ok > /gocache/.repo-steward-probe && echo GOCACHE_OK"
	res, err := d.Run(ctx, probeSpec(Execute, script))
	if err != nil {
		return err
	}
	out := string(res.Stdout)
	if !strings.HasPrefix(out, nonce) {
		return fmt.Errorf("%w: cache directory %s (engine socket %s)", ErrMountUnavailable, d.cfg.CacheDir, d.client.Socket())
	}
	if first != "" && !strings.Contains(out, "SRC_OK") {
		return fmt.Errorf("%w: source directory %s", ErrMountUnavailable, d.cfg.SourceDir)
	}
	if !strings.Contains(out, "GOCACHE_OK") {
		return fmt.Errorf("sandbox: build cache %s is not writable by the container user: %s", d.cfg.BuildCacheDir, strings.TrimSpace(string(res.Stderr)))
	}
	os.Remove(filepath.Join(d.cfg.BuildCacheDir, ".repo-steward-probe"))
	res, err = d.Run(ctx, probeSpec(Acquire, "echo ok > /cache/.repo-steward-probe-w && echo CACHE_OK"))
	if err != nil {
		return err
	}
	os.Remove(filepath.Join(d.cfg.CacheDir, ".repo-steward-probe-w"))
	if !strings.Contains(string(res.Stdout), "CACHE_OK") {
		return fmt.Errorf("sandbox: module cache %s is not writable by the container user: %s", d.cfg.CacheDir, strings.TrimSpace(string(res.Stderr)))
	}
	return nil
}

func probeSpec(p Profile, script string) ExecSpec {
	return ExecSpec{Profile: p, Argv: []string{"bash", "-c", script}, Timeout: 30 * time.Second, StepID: "probe"}
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// EnsureImage checks the image is present, pulling it when allowPull is set.
// Pulling is the one network operation the sandbox performs on its own, and
// only as a bootstrap.
func (d *Docker) EnsureImage(ctx context.Context, allowPull bool) error {
	_, err := d.client.ImageInspect(ctx, d.cfg.Image)
	if err == nil {
		return nil
	}
	if !errors.Is(err, dockerapi.ErrNotFound) {
		return err
	}
	if !allowPull {
		return fmt.Errorf("sandbox: image %s is not present; pull it once with network access", d.cfg.Image)
	}
	if err := d.client.ImagePull(ctx, d.cfg.Image); err != nil {
		return err
	}
	_, err = d.client.ImageInspect(ctx, d.cfg.Image)
	return err
}

// ReapOrphans removes every container carrying the sandbox label. It is
// called by the executor after taking the executor lock, when no other
// process can legitimately own one.
func (d *Docker) ReapOrphans(ctx context.Context) ([]string, error) {
	list, err := d.client.ContainerListByLabel(ctx, Label+"=1")
	if err != nil {
		return nil, err
	}
	var removed []string
	for _, c := range list {
		if err := d.client.ContainerRemove(ctx, c.ID); err != nil {
			return removed, err
		}
		removed = append(removed, c.ID)
	}
	return removed, nil
}

func (d *Docker) env(p Profile) []string {
	env := []string{
		"PATH=/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin",
		"HOME=/tmp",
		"TMPDIR=/tmp",
		"GOTMPDIR=/tmp",
		"GOPATH=/tmp/gopath",
		"GOMODCACHE=/cache/mod",
		"GOCACHE=/gocache",
		"GOTOOLCHAIN=local",
		"CGO_ENABLED=0",
		"GOPRIVATE=",
		"GONOPROXY=",
		"GONOSUMDB=",
		"GOINSECURE=",
		"GIT_TERMINAL_PROMPT=0",
	}
	switch p {
	case Acquire:
		env = append(env, "GOFLAGS=-mod=mod")
		if d.cfg.ProxyDir != "" {
			env = append(env, "GOPROXY=file:///proxy", "GOSUMDB=off")
		} else {
			env = append(env, "GOPROXY="+d.cfg.GoProxy, "GOSUMDB="+d.cfg.GoSumDB)
		}
	case Execute:
		env = append(env, "GOFLAGS=-mod=readonly", "GOPROXY=off", "GOSUMDB=off")
	}
	return env
}

func (d *Docker) hostConfig(p Profile) (dockerapi.HostConfig, error) {
	pids := d.cfg.PidsLimit
	initTrue := true
	hc := dockerapi.HostConfig{
		// exec is required: the execute profile runs test binaries from /tmp.
		Tmpfs:          map[string]string{"/tmp": "rw,nosuid,nodev,exec,size=" + d.cfg.TmpfsSize},
		ReadonlyRootfs: true,
		CapDrop:        []string{"ALL"},
		SecurityOpt:    []string{"no-new-privileges"},
		Memory:         d.cfg.MemoryBytes,
		MemorySwap:     d.cfg.MemoryBytes,
		NanoCPUs:       d.cfg.NanoCPUs,
		PidsLimit:      &pids,
		Init:           &initTrue,
	}
	switch p {
	case Acquire:
		hc.Binds = []string{
			d.cfg.SourceDir + ":/work:ro",
			d.cfg.CacheDir + ":/cache:rw",
			d.cfg.BuildCacheDir + ":/gocache:rw",
		}
		if d.cfg.ProxyDir != "" {
			hc.Binds = append(hc.Binds, d.cfg.ProxyDir+":/proxy:ro")
			hc.NetworkMode = "none"
		} else {
			hc.NetworkMode = "bridge"
		}
	case Execute:
		hc.Binds = []string{
			d.cfg.SourceDir + ":/work:ro",
			d.cfg.CacheDir + ":/cache:ro",
			d.cfg.BuildCacheDir + ":/gocache:rw",
		}
		hc.NetworkMode = "none"
	default:
		return hc, fmt.Errorf("sandbox: unknown profile %q", p)
	}
	return hc, nil
}

// Run implements Sandbox.
func (d *Docker) Run(ctx context.Context, spec ExecSpec) (ExecResult, error) {
	var res ExecResult
	if len(spec.Argv) == 0 {
		return res, errors.New("sandbox: empty argv")
	}
	if spec.Timeout <= 0 {
		return res, errors.New("sandbox: timeout is required")
	}
	if spec.OutputCap <= 0 {
		spec.OutputCap = DefaultOutputCap
	}
	hc, err := d.hostConfig(spec.Profile)
	if err != nil {
		return res, err
	}
	// In-container fallback timeout: only matters if this process dies.
	inner := int(spec.Timeout.Seconds()) + 5
	cmd := append([]string{"timeout", "-s", "KILL", strconv.Itoa(inner)}, spec.Argv...)
	cfg := dockerapi.ContainerConfig{
		Image:      d.cfg.Image,
		Cmd:        cmd,
		Env:        d.env(spec.Profile),
		WorkingDir: "/work",
		User:       d.cfg.User,
		Labels: map[string]string{
			Label:              "1",
			Label + ".profile": string(spec.Profile),
			Label + ".run":     spec.RunID,
			Label + ".step":    spec.StepID,
			Label + ".owner":   strconv.Itoa(os.Getpid()),
		},
		HostConfig: hc,
	}
	id, err := d.client.ContainerCreate(ctx, cfg)
	if err != nil {
		return res, err
	}
	res.ContainerID = id
	// Removal must not depend on the caller's context still being alive.
	cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), dockerapi.WaitTimeout)
	defer cancelCleanup()
	defer d.client.ContainerRemove(cleanupCtx, id)

	start := time.Now()
	if err := d.client.ContainerStart(ctx, id); err != nil {
		return res, err
	}
	runCtx, cancel := context.WithTimeout(ctx, spec.Timeout)
	defer cancel()
	code, err := d.client.ContainerWait(runCtx, id)
	if err != nil {
		if runCtx.Err() != nil {
			res.TimedOut = true
			if kerr := d.client.ContainerKill(cleanupCtx, id); kerr != nil {
				return res, kerr
			}
			code, err = d.client.ContainerWait(cleanupCtx, id)
			if err != nil {
				return res, err
			}
		} else {
			return res, err
		}
	}
	res.Duration = time.Since(start)
	res.ExitCode = code
	logs, err := d.client.ContainerLogs(cleanupCtx, id, spec.OutputCap)
	if err != nil {
		return res, err
	}
	res.Stdout, res.Stderr = logs.Stdout, logs.Stderr
	res.StdoutTruncated, res.StderrTruncated = logs.StdoutTruncated, logs.StderrTruncated
	return res, nil
}
