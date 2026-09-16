// Package dockerapi is a minimal client for the Docker Engine API over a
// unix socket. It covers only the calls the sandbox needs, so the dependency
// surface stays small and every request is visible.
package dockerapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Client talks to one engine.
type Client struct {
	socket string
	http   *http.Client
}

const apiVersion = "v1.43"

// ErrNotFound is returned for 404 responses.
var ErrNotFound = errors.New("dockerapi: not found")

// DefaultSocket returns the engine socket from DOCKER_HOST or the first of
// the well-known local paths that exists.
func DefaultSocket() (string, error) {
	if h := os.Getenv("DOCKER_HOST"); h != "" {
		if !strings.HasPrefix(h, "unix://") {
			return "", fmt.Errorf("dockerapi: DOCKER_HOST %q is not a unix socket", h)
		}
		return strings.TrimPrefix(h, "unix://"), nil
	}
	home, _ := os.UserHomeDir()
	candidates := []string{
		"/var/run/docker.sock",
		filepath.Join(home, ".docker", "run", "docker.sock"),
		filepath.Join(home, ".colima", "default", "docker.sock"),
		filepath.Join(home, ".orbstack", "run", "docker.sock"),
		filepath.Join(home, ".rd", "docker.sock"),
	}
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && st.Mode()&os.ModeSocket != 0 {
			return c, nil
		}
	}
	return "", errors.New("dockerapi: no engine socket found; set DOCKER_HOST=unix:///path/to/docker.sock")
}

// New returns a client for the socket.
func New(socket string) *Client {
	return &Client{
		socket: socket,
		http: &http.Client{Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socket)
			},
		}},
	}
}

// Socket returns the socket path.
func (c *Client) Socket() string { return c.socket }

func (c *Client) do(ctx context.Context, method, path string, query url.Values, body any) (*http.Response, error) {
	u := "http://docker/" + apiVersion + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("dockerapi: %s %s: %w", method, path, err)
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		var e struct {
			Message string `json:"message"`
		}
		json.Unmarshal(msg, &e)
		if e.Message == "" {
			e.Message = strings.TrimSpace(string(msg))
		}
		if resp.StatusCode == http.StatusNotFound {
			return nil, fmt.Errorf("%w: %s %s: %s", ErrNotFound, method, path, e.Message)
		}
		return nil, fmt.Errorf("dockerapi: %s %s: %d %s", method, path, resp.StatusCode, e.Message)
	}
	return resp, nil
}

func (c *Client) doJSON(ctx context.Context, method, path string, query url.Values, body, out any) error {
	resp, err := c.do(ctx, method, path, query, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Version is the engine version report.
type Version struct {
	Version    string `json:"Version"`
	APIVersion string `json:"ApiVersion"`
	Os         string `json:"Os"`
	Arch       string `json:"Arch"`
}

// Version reports the engine version.
func (c *Client) Version(ctx context.Context) (Version, error) {
	var v Version
	err := c.doJSON(ctx, http.MethodGet, "/version", nil, nil, &v)
	return v, err
}

// Image is the subset of image inspection the sandbox uses.
type Image struct {
	ID          string   `json:"Id"`
	RepoDigests []string `json:"RepoDigests"`
	RepoTags    []string `json:"RepoTags"`
}

// ImageInspect returns ErrNotFound when the image is absent.
func (c *Client) ImageInspect(ctx context.Context, ref string) (Image, error) {
	var img Image
	err := c.doJSON(ctx, http.MethodGet, "/images/"+url.PathEscape(ref)+"/json", nil, nil, &img)
	return img, err
}

// ImagePull pulls ref (name:tag or name@digest) and waits for completion.
func (c *Client) ImagePull(ctx context.Context, ref string) error {
	q := url.Values{}
	if i := strings.Index(ref, "@"); i > 0 {
		q.Set("fromImage", ref[:i])
		q.Set("tag", ref[i+1:])
	} else if i := strings.LastIndex(ref, ":"); i > 0 && !strings.Contains(ref[i:], "/") {
		q.Set("fromImage", ref[:i])
		q.Set("tag", ref[i+1:])
	} else {
		q.Set("fromImage", ref)
	}
	resp, err := c.do(ctx, http.MethodPost, "/images/create", q, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	dec := json.NewDecoder(resp.Body)
	for {
		var msg struct {
			Error string `json:"error"`
		}
		if err := dec.Decode(&msg); err == io.EOF {
			return nil
		} else if err != nil {
			return fmt.Errorf("dockerapi: pull stream: %w", err)
		}
		if msg.Error != "" {
			return fmt.Errorf("dockerapi: pull %s: %s", ref, msg.Error)
		}
	}
}

// HostConfig is the subset of container host configuration used.
type HostConfig struct {
	Binds          []string          `json:"Binds,omitempty"`
	Tmpfs          map[string]string `json:"Tmpfs,omitempty"`
	ReadonlyRootfs bool              `json:"ReadonlyRootfs"`
	NetworkMode    string            `json:"NetworkMode"`
	CapDrop        []string          `json:"CapDrop,omitempty"`
	SecurityOpt    []string          `json:"SecurityOpt,omitempty"`
	Memory         int64             `json:"Memory,omitempty"`
	MemorySwap     int64             `json:"MemorySwap,omitempty"`
	NanoCPUs       int64             `json:"NanoCpus,omitempty"`
	PidsLimit      *int64            `json:"PidsLimit,omitempty"`
	Init           *bool             `json:"Init,omitempty"`
}

// ContainerConfig is the container creation request.
type ContainerConfig struct {
	Image      string            `json:"Image"`
	Cmd        []string          `json:"Cmd"`
	Env        []string          `json:"Env"`
	WorkingDir string            `json:"WorkingDir,omitempty"`
	User       string            `json:"User,omitempty"`
	Labels     map[string]string `json:"Labels,omitempty"`
	HostConfig HostConfig        `json:"HostConfig"`
}

// ContainerCreate creates a container and returns its id.
func (c *Client) ContainerCreate(ctx context.Context, cfg ContainerConfig) (string, error) {
	var out struct {
		ID       string   `json:"Id"`
		Warnings []string `json:"Warnings"`
	}
	if err := c.doJSON(ctx, http.MethodPost, "/containers/create", nil, cfg, &out); err != nil {
		return "", err
	}
	return out.ID, nil
}

// ContainerStart starts a created container.
func (c *Client) ContainerStart(ctx context.Context, id string) error {
	return c.doJSON(ctx, http.MethodPost, "/containers/"+id+"/start", nil, nil, nil)
}

// ContainerWait blocks until the container stops and returns its exit code.
func (c *Client) ContainerWait(ctx context.Context, id string) (int, error) {
	var out struct {
		StatusCode int `json:"StatusCode"`
		Error      *struct {
			Message string `json:"Message"`
		} `json:"Error"`
	}
	q := url.Values{"condition": {"not-running"}}
	if err := c.doJSON(ctx, http.MethodPost, "/containers/"+id+"/wait", q, nil, &out); err != nil {
		return -1, err
	}
	if out.Error != nil && out.Error.Message != "" {
		return -1, fmt.Errorf("dockerapi: wait %s: %s", id, out.Error.Message)
	}
	return out.StatusCode, nil
}

// ContainerKill sends SIGKILL.
func (c *Client) ContainerKill(ctx context.Context, id string) error {
	err := c.doJSON(ctx, http.MethodPost, "/containers/"+id+"/kill", url.Values{"signal": {"SIGKILL"}}, nil, nil)
	if err != nil && strings.Contains(err.Error(), "is not running") {
		return nil
	}
	return err
}

// ContainerRemove force-removes a container and its anonymous volumes.
func (c *Client) ContainerRemove(ctx context.Context, id string) error {
	err := c.doJSON(ctx, http.MethodDelete, "/containers/"+id, url.Values{"force": {"1"}, "v": {"1"}}, nil, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// Logs holds demultiplexed container output, each stream capped at cap bytes.
type Logs struct {
	Stdout, Stderr                   []byte
	StdoutTruncated, StderrTruncated bool
}

// ContainerLogs reads stdout and stderr of a stopped container. Each stream
// is capped at cap bytes; the flags report whether anything was dropped.
func (c *Client) ContainerLogs(ctx context.Context, id string, cap int) (Logs, error) {
	var logs Logs
	q := url.Values{"stdout": {"1"}, "stderr": {"1"}}
	resp, err := c.do(ctx, http.MethodGet, "/containers/"+id+"/logs", q, nil)
	if err != nil {
		return logs, err
	}
	defer resp.Body.Close()
	var hdr [8]byte
	for {
		if _, err := io.ReadFull(resp.Body, hdr[:]); err != nil {
			if err == io.EOF {
				return logs, nil
			}
			return logs, fmt.Errorf("dockerapi: log stream: %w", err)
		}
		n := int(binary.BigEndian.Uint32(hdr[4:]))
		chunk := make([]byte, n)
		if _, err := io.ReadFull(resp.Body, chunk); err != nil {
			return logs, fmt.Errorf("dockerapi: log stream: %w", err)
		}
		switch hdr[0] {
		case 1:
			logs.Stdout, logs.StdoutTruncated = appendCapped(logs.Stdout, chunk, cap, logs.StdoutTruncated)
		case 2:
			logs.Stderr, logs.StderrTruncated = appendCapped(logs.Stderr, chunk, cap, logs.StderrTruncated)
		}
	}
}

func appendCapped(dst, chunk []byte, cap int, truncated bool) ([]byte, bool) {
	if cap <= 0 {
		return append(dst, chunk...), truncated
	}
	room := cap - len(dst)
	if room <= 0 {
		return dst, true
	}
	if len(chunk) > room {
		return append(dst, chunk[:room]...), true
	}
	return append(dst, chunk...), truncated
}

// ContainerSummary is one row of a container listing.
type ContainerSummary struct {
	ID     string            `json:"Id"`
	Names  []string          `json:"Names"`
	State  string            `json:"State"`
	Labels map[string]string `json:"Labels"`
}

// ContainerListByLabel lists all containers carrying the label.
func (c *Client) ContainerListByLabel(ctx context.Context, label string) ([]ContainerSummary, error) {
	filters, _ := json.Marshal(map[string][]string{"label": {label}})
	q := url.Values{"all": {"1"}, "filters": {string(filters)}}
	var out []ContainerSummary
	err := c.doJSON(ctx, http.MethodGet, "/containers/json", q, nil, &out)
	return out, err
}

// WaitTimeout is how long a stop is given before the sandbox gives up.
const WaitTimeout = 30 * time.Second
