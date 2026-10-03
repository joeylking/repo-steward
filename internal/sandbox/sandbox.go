// Package sandbox runs toolchain commands in containers under fixed
// profiles. Tools choose a profile and arguments; the sandbox alone decides
// mounts, network, environment, user, and resource limits.
//
// Profiles:
//
//	acquire  go mod download and module metadata. Source read-only, module
//	         cache writable, network on (or off with a file proxy). Executes
//	         no repository code.
//	mutate   go get and go mod tidy against a staging copy of the manifests
//	         via -modfile. Source read-only, staging and module cache
//	         writable, network as for acquire. Executes no repository code.
//	execute  build, vet, test, and offline go list. Source and module cache
//	         read-only, no network, tmpfs /tmp, and a writable build cache
//	         that is new for each validation and never mounted by acquire
//	         or mutate.
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
	Mutate  Profile = "mutate"
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
	// BuildCacheDir is the root of the run's Go build caches, discarded by
	// the caller at the end of a run. The acquire and mutate profiles mount
	// its "acquire" subdirectory. The execute profile mounts only a fresh
	// subdirectory made by WithFreshBuildCache, so code executed in one
	// validation cannot leave anything in the cache a later one reads.
	BuildCacheDir string
	// StagingDir, when set, is mounted read-write at /staging in the mutate
	// profile. It holds the go.mod and go.sum copies that -modfile targets.
	StagingDir string
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
	// cleanup overrides dockerapi.WaitTimeout as the budget of each
	// post-run engine call; zero means the default. Tests shorten it.
	cleanup time.Duration
	// execCache is the execute profile's build cache, set only by
	// WithFreshBuildCache. Without it the execute profile refuses to run.
	execCache string
}

func (d *Docker) cleanupTimeout() time.Duration {
	if d.cleanup > 0 {
		return d.cleanup
	}
	return dockerapi.WaitTimeout
}

// NewDocker validates cfg, prepares host directories, and connects to the
// engine socket.
func NewDocker(cfg Config, socket string) (*Docker, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	// The module and build caches are written by an unprivileged container
	// user, so they must be world-writable on the host. The root holding
	// the build caches is not mounted and stays owner-only; the data
	// directory above all of them is owner-only too (lock.CheckLocal).
	if err := os.MkdirAll(cfg.BuildCacheDir, 0o700); err != nil {
		return nil, err
	}
	for _, dir := range []string{cfg.CacheDir, acquireCache(cfg)} {
		if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
			return nil, err
		}
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

// WithSource returns a sandbox bound to a different source directory and
// sharing everything else. The pipeline uses it to validate each candidate
// snapshot; tools never call it.
func (d *Docker) WithSource(dir string) *Docker {
	n := *d
	n.cfg.SourceDir = dir
	return &n
}

// WithStaging returns a sandbox whose mutate profile mounts dir at /staging.
func (d *Docker) WithStaging(dir string) *Docker {
	n := *d
	n.cfg.StagingDir = dir
	return &n
}

// WithFreshBuildCache returns a sandbox whose execute profile mounts a new,
// empty build cache under BuildCacheDir that no container has written.
// Every validation calls it once, so build, vet, and test within one
// validation share a cache and nothing executed in an earlier validation
// can reach a later one. The name is random: a path is never reused within
// a run, because VM-backed engines cache path lookups.
func (d *Docker) WithFreshBuildCache() (*Docker, error) {
	dir, err := os.MkdirTemp(d.cfg.BuildCacheDir, "execute-")
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		return nil, err
	}
	n := *d
	n.execCache = dir
	return &n, nil
}

// ExecuteCacheDir is the host directory the execute profile mounts at
// /gocache, or empty when the sandbox has none. Tests use it.
func (d *Docker) ExecuteCacheDir() string { return d.execCache }

// RemoveBuildCaches deletes BuildCacheDir and everything under it. With a
// native Linux engine the files in a build cache belong to the container
// user and the operator cannot delete them, so what the host cannot remove
// is emptied by a container running as that user first. VM-backed engines
// map ownership to the operator and never need the container.
func (d *Docker) RemoveBuildCaches(ctx context.Context) error {
	root := d.cfg.BuildCacheDir
	if removeTree(root) {
		return nil
	}
	if err := d.emptyBuildCaches(ctx); err != nil {
		return err
	}
	if !removeTree(root) {
		return fmt.Errorf("sandbox: build cache %s could not be removed", root)
	}
	return nil
}

// emptyBuildCaches deletes the contents of every directory under
// BuildCacheDir from inside one container. The directories themselves
// stay: they are mount points, and the operator owns them.
func (d *Docker) emptyBuildCaches(ctx context.Context) error {
	entries, err := os.ReadDir(d.cfg.BuildCacheDir)
	if err != nil {
		return err
	}
	hc, err := d.hostConfig(Acquire)
	if err != nil {
		return err
	}
	hc.NetworkMode = "none"
	hc.Binds = []string{d.cfg.SourceDir + ":/work:ro"}
	for _, e := range entries {
		if e.IsDir() {
			hc.Binds = append(hc.Binds, filepath.Join(d.cfg.BuildCacheDir, e.Name())+":/clean/"+strconv.Itoa(len(hc.Binds))+":rw")
		}
	}
	if len(hc.Binds) == 1 {
		return nil
	}
	script := "chmod -R u+rwx /clean/* 2>/dev/null; find /clean -mindepth 2 -delete"
	res, err := d.run(ctx, ExecSpec{Profile: Acquire, Argv: []string{"sh", "-c", script}, Timeout: 2 * time.Minute, OutputCap: DefaultOutputCap, StepID: "cleanup"}, hc)
	if err != nil {
		return err
	}
	if res.TimedOut || res.ExitCode != 0 {
		return fmt.Errorf("sandbox: emptying build caches: exit %d: %s", res.ExitCode, strings.TrimSpace(string(res.Stderr)))
	}
	return nil
}

// removeTree deletes a tree whose entries may have been made read-only and
// reports whether it is gone.
func removeTree(dir string) bool {
	filepath.WalkDir(dir, func(p string, e os.DirEntry, err error) error {
		if err == nil {
			if e.IsDir() {
				os.Chmod(p, 0o755)
			} else {
				os.Chmod(p, 0o644)
			}
		}
		return nil
	})
	os.RemoveAll(dir)
	_, err := os.Lstat(dir)
	return os.IsNotExist(err)
}

func acquireCache(c Config) string { return filepath.Join(c.BuildCacheDir, "acquire") }

// Client exposes the engine client for tests and reaping.
func (d *Docker) Client() *dockerapi.Client { return d.client }

// ErrMountUnavailable means the engine cannot see the host directories. On
// VM-backed engines a bind mount of an unshared path silently becomes an
// empty directory inside the VM, which would make validation run against
// nothing. Probe turns that into a hard failure.
var ErrMountUnavailable = errors.New("sandbox: host directories are not visible to the container engine")

// Probe verifies that the cache and source directories are really shared
// with the engine and that the container user can write to the caches. It
// runs one short execute-profile container, against a fresh build cache it
// then removes, and one acquire-profile container.
func (d *Docker) Probe(ctx context.Context) error {
	// Marker names carry a nonce: VM-backed engines cache directory
	// lookups, and a name that was recently absent can stay invisible for
	// a while even after the host creates it.
	nonce := strconv.FormatInt(time.Now().UnixNano(), 36)
	markerName := ".repo-steward-probe-" + nonce
	marker := filepath.Join(d.cfg.CacheDir, markerName)
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
	gocacheMarker := ".repo-steward-probe-" + nonce
	script := "cat /cache/" + markerName + " && echo && test -e /work/" + shellQuote(first) + " && echo SRC_OK && echo ok > /gocache/" + gocacheMarker + " && echo GOCACHE_OK"
	exec, err := d.WithFreshBuildCache()
	if err != nil {
		return err
	}
	res, err := exec.Run(ctx, probeSpec(Execute, script))
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
		return fmt.Errorf("sandbox: build cache %s is not writable by the container user: %s", exec.execCache, strings.TrimSpace(string(res.Stderr)))
	}
	os.Remove(filepath.Join(exec.execCache, gocacheMarker))
	os.Remove(exec.execCache)
	cacheMarker := ".repo-steward-probe-w-" + nonce
	res, err = d.Run(ctx, probeSpec(Acquire, "echo ok > /cache/"+cacheMarker+" && echo CACHE_OK && echo ok > /gocache/"+cacheMarker+" && echo GOCACHE_OK"))
	if err != nil {
		return err
	}
	os.Remove(filepath.Join(d.cfg.CacheDir, cacheMarker))
	os.Remove(filepath.Join(acquireCache(d.cfg), cacheMarker))
	if !strings.Contains(string(res.Stdout), "CACHE_OK") {
		return fmt.Errorf("sandbox: module cache %s is not writable by the container user: %s", d.cfg.CacheDir, strings.TrimSpace(string(res.Stderr)))
	}
	if !strings.Contains(string(res.Stdout), "GOCACHE_OK") {
		return fmt.Errorf("sandbox: build cache %s is not writable by the container user: %s", acquireCache(d.cfg), strings.TrimSpace(string(res.Stderr)))
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
	case Acquire, Mutate:
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
	case Acquire, Mutate:
		hc.Binds = []string{
			d.cfg.SourceDir + ":/work:ro",
			d.cfg.CacheDir + ":/cache:rw",
			acquireCache(d.cfg) + ":/gocache:rw",
		}
		if p == Mutate {
			if d.cfg.StagingDir == "" {
				return hc, errors.New("sandbox: mutate profile requires a staging directory")
			}
			hc.Binds = append(hc.Binds, d.cfg.StagingDir+":/staging:rw")
		}
		if d.cfg.ProxyDir != "" {
			hc.Binds = append(hc.Binds, d.cfg.ProxyDir+":/proxy:ro")
			hc.NetworkMode = "none"
		} else {
			hc.NetworkMode = "bridge"
		}
	case Execute:
		if d.execCache == "" {
			return hc, errors.New("sandbox: execute profile requires a fresh build cache (WithFreshBuildCache)")
		}
		hc.Binds = []string{
			d.cfg.SourceDir + ":/work:ro",
			d.cfg.CacheDir + ":/cache:ro",
			d.execCache + ":/gocache:rw",
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
	return d.run(ctx, spec, hc)
}

// run executes spec in a container with the given host configuration.
func (d *Docker) run(ctx context.Context, spec ExecSpec, hc dockerapi.HostConfig) (ExecResult, error) {
	var res ExecResult
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
	// Kill, log collection, and removal must not depend on the caller's
	// context still being alive. Each gets its own budget starting when it
	// begins: a budget started at creation would already be spent by any
	// command that runs longer than it.
	afterRun := func() (context.Context, context.CancelFunc) {
		return context.WithTimeout(context.Background(), d.cleanupTimeout())
	}
	defer func() {
		rctx, cancel := afterRun()
		defer cancel()
		d.client.ContainerRemove(rctx, id)
	}()

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
			kctx, cancelKill := afterRun()
			defer cancelKill()
			if kerr := d.client.ContainerKill(kctx, id); kerr != nil {
				return res, kerr
			}
			code, err = d.client.ContainerWait(kctx, id)
			if err != nil {
				return res, err
			}
		} else {
			return res, err
		}
	}
	res.Duration = time.Since(start)
	res.ExitCode = code
	lctx, cancelLogs := afterRun()
	defer cancelLogs()
	logs, err := d.client.ContainerLogs(lctx, id, spec.OutputCap)
	if err != nil {
		return res, err
	}
	res.Stdout, res.Stderr = logs.Stdout, logs.Stderr
	res.StdoutTruncated, res.StderrTruncated = logs.StdoutTruncated, logs.StderrTruncated
	return res, nil
}
