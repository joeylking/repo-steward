package sandbox

import (
	"context"
	"encoding/binary"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/joeylking/repo-steward/internal/sandbox/dockerapi"
)

// fakeEngine serves the container calls Run makes over a unix socket. The
// wait call blocks for wait, standing in for a command that runs that long.
type fakeEngine struct {
	wait time.Duration

	mu      sync.Mutex
	removed bool
	errs    []string
}

func (f *fakeEngine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	switch {
	case r.Method == http.MethodPost && strings.HasSuffix(p, "/containers/create"):
		w.Write([]byte(`{"Id":"c1"}`))
	case r.Method == http.MethodPost && strings.HasSuffix(p, "/containers/c1/start"):
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPost && strings.HasSuffix(p, "/containers/c1/wait"):
		select {
		case <-time.After(f.wait):
		case <-r.Context().Done():
			return
		}
		w.Write([]byte(`{"StatusCode":0}`))
	case r.Method == http.MethodGet && strings.HasSuffix(p, "/containers/c1/logs"):
		msg := []byte("ok\n")
		hdr := make([]byte, 8)
		hdr[0] = 1
		binary.BigEndian.PutUint32(hdr[4:], uint32(len(msg)))
		w.Write(append(hdr, msg...))
	case r.Method == http.MethodDelete && strings.HasSuffix(p, "/containers/c1"):
		f.mu.Lock()
		f.removed = true
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	default:
		f.mu.Lock()
		f.errs = append(f.errs, r.Method+" "+p)
		f.mu.Unlock()
		http.NotFound(w, r)
	}
}

func serveFake(t *testing.T, f *fakeEngine) string {
	t.Helper()
	// A short directory: unix socket paths are limited to about 100 bytes.
	dir, err := os.MkdirTemp("", "sbx")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "e.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: f}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return sock
}

// A command that runs longer than the post-run budget still has its output
// collected and its container removed: each post-run call gets a budget
// that starts when the call does, not when the container was created.
func TestRun_LongCommandStillCollectsLogsAndRemoves(t *testing.T) {
	f := &fakeEngine{wait: 300 * time.Millisecond}
	sock := serveFake(t, f)
	d := &Docker{
		cfg:     Config{Image: "img", SourceDir: "/s", CacheDir: "/c", BuildCacheDir: "/b"},
		client:  dockerapi.New(sock),
		cleanup: 100 * time.Millisecond,
	}
	res, err := d.Run(context.Background(), ExecSpec{Profile: Execute, Argv: []string{"true"}, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.TimedOut || res.ExitCode != 0 || string(res.Stdout) != "ok\n" {
		t.Fatalf("result %+v stdout %q", res, res.Stdout)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.removed {
		t.Fatal("container not removed")
	}
	if len(f.errs) != 0 {
		t.Fatalf("unexpected engine calls: %v", f.errs)
	}
}
