package vuln

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// zipOf builds an archive from a fixture database directory plus extra
// entries, in the layout of vulndb.zip.
func zipOf(t *testing.T, dir string, extra map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	if dir != "" {
		err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			rel, _ := filepath.Rel(dir, p)
			w, err := zw.Create(filepath.ToSlash(rel))
			if err != nil {
				return err
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			_, err = w.Write(b)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for name, content := range extra {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(content))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func fixtureDir(t *testing.T) string {
	t.Helper()
	d := filepath.Join(t.TempDir(), "fixture-db")
	if err := WriteFixtureDB(d); err != nil {
		t.Fatal(err)
	}
	return d
}

func serve(t *testing.T, body []byte) (*httptest.Server, *int32) {
	t.Helper()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		if r.URL.Path != "/vulndb.zip" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/zip")
		w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func TestFetch_ExtractsIdentifiesAndReuses(t *testing.T) {
	src := fixtureDir(t)
	srv, hits := serve(t, zipOf(t, src, nil))
	parent := t.TempDir()
	snap, err := Fetch(context.Background(), FetchOptions{Client: srv.Client(), BaseURL: srv.URL, Parent: parent})
	if err != nil {
		t.Fatal(err)
	}
	if snap.ID.Hash != fixtureDBHash || !snap.ID.Modified.Equal(FixtureTime) || snap.Reused {
		t.Fatalf("snapshot = %+v", snap)
	}
	if filepath.Dir(snap.Dir) != parent || filepath.Base(snap.Dir) != "vulndb-20260101T000000Z-"+strings.TrimPrefix(fixtureDBHash, "sha256:")[:16] {
		t.Fatalf("dir = %s", snap.Dir)
	}
	if err := Verify(snap.Dir, snap.ID, Limits{}); err != nil {
		t.Fatal(err)
	}
	// Nothing but the snapshot is left behind.
	entries, _ := os.ReadDir(parent)
	if len(entries) != 1 {
		t.Fatalf("parent holds %d entries", len(entries))
	}
	// A second fetch of identical content verifies and reuses the directory
	// rather than replacing a path an engine may have mounted.
	before, _ := os.Stat(snap.Dir)
	again, err := Fetch(context.Background(), FetchOptions{Client: srv.Client(), BaseURL: srv.URL, Parent: parent})
	if err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(again.Dir)
	if !again.Reused || again.Dir != snap.Dir || !os.SameFile(before, after) || atomic.LoadInt32(hits) != 2 {
		t.Fatalf("second fetch = %+v", again)
	}
	entries, _ = os.ReadDir(parent)
	if len(entries) != 1 {
		t.Fatalf("parent holds %d entries after reuse", len(entries))
	}
}

func TestFetch_RefusesTamperedExistingSnapshot(t *testing.T) {
	src := fixtureDir(t)
	srv, _ := serve(t, zipOf(t, src, nil))
	parent := t.TempDir()
	snap, err := Fetch(context.Background(), FetchOptions{Client: srv.Client(), BaseURL: srv.URL, Parent: parent})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(snap.Dir, "ID", "GO-TEST-0001.json"), []byte(`{"id":"GO-TEST-0001"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Fetch(context.Background(), FetchOptions{Client: srv.Client(), BaseURL: srv.URL, Parent: parent}); !errors.Is(err, ErrSnapshotMismatch) {
		t.Fatalf("err = %v, want ErrSnapshotMismatch", err)
	}
}

func TestFetch_Refusals(t *testing.T) {
	src := fixtureDir(t)
	good := zipOf(t, src, nil)
	cases := map[string]struct {
		body   []byte
		limits Limits
		want   string
	}{
		"zip slip":            {body: zipOf(t, src, map[string]string{"../escape.json": "{}"}), want: "not part of the database layout"},
		"absolute path":       {body: zipOf(t, src, map[string]string{"/etc/passwd": "x"}), want: "not part of the database layout"},
		"nested traversal":    {body: zipOf(t, src, map[string]string{"ID/../../x.json": "{}"}), want: "not part of the database layout"},
		"stray file":          {body: zipOf(t, src, map[string]string{"README": "hi"}), want: "not part of the database layout"},
		"stray directory":     {body: zipOf(t, src, map[string]string{"other/": ""}), want: "not part of the database layout"},
		"entry without index": {body: zipOf(t, src, map[string]string{"ID/GO-TEST-9999.json": `{"id":"GO-TEST-9999"}`}), want: "not in index/vulns.json"},
		"entry misnamed":      {body: zipOf(t, src, map[string]string{"ID/GO-TEST-9998.json": `{"id":"GO-TEST-0001"}`}), want: "declares id"},
		"missing index":       {body: zipOf(t, "", map[string]string{"index/db.json": `{"modified":"2026-01-01T00:00:00Z"}`, "index/vulns.json": "[]"}), want: "no index/modules.json"},
		"no modified time":    {body: zipOf(t, "", map[string]string{"index/db.json": `{}`, "index/vulns.json": "[]", "index/modules.json": "[]"}), want: "no modified time"},
		"not a zip":           {body: []byte("<html>maintenance</html>"), want: "open archive"},
		"archive too large":   {body: good, limits: Limits{MaxArchiveBytes: 100, MaxTotalBytes: 1 << 20, MaxEntryBytes: 1 << 20, MaxEntries: 100}, want: "100 bytes"},
		"too many entries":    {body: good, limits: Limits{MaxArchiveBytes: 1 << 20, MaxTotalBytes: 1 << 20, MaxEntryBytes: 1 << 20, MaxEntries: 3}, want: "entries"},
		"entry too large":     {body: good, limits: Limits{MaxArchiveBytes: 1 << 20, MaxTotalBytes: 1 << 20, MaxEntryBytes: 64, MaxEntries: 100}, want: "limit"},
		"total too large":     {body: good, limits: Limits{MaxArchiveBytes: 1 << 20, MaxTotalBytes: 500, MaxEntryBytes: 1 << 20, MaxEntries: 100}, want: "limit"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			srv, _ := serve(t, c.body)
			parent := t.TempDir()
			_, err := Fetch(context.Background(), FetchOptions{Client: srv.Client(), BaseURL: srv.URL, Parent: parent, Limits: c.limits})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want it to contain %q", err, c.want)
			}
			entries, _ := os.ReadDir(parent)
			if len(entries) != 0 {
				t.Fatalf("a failed fetch left %d entries", len(entries))
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(parent), "escape.json")); err == nil {
				t.Fatal("zip slip wrote outside the parent")
			}
		})
	}
}

func TestFetch_HTTPFailures(t *testing.T) {
	parent := t.TempDir()
	status := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "gone", http.StatusServiceUnavailable)
	}))
	defer status.Close()
	if _, err := Fetch(context.Background(), FetchOptions{Client: status.Client(), BaseURL: status.URL, Parent: parent}); err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("err = %v", err)
	}
	// A body shorter than its declared length is a truncated download.
	short := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000")
		w.Write([]byte("PK"))
	}))
	defer short.Close()
	if _, err := Fetch(context.Background(), FetchOptions{Client: short.Client(), BaseURL: short.URL, Parent: parent}); err == nil {
		t.Fatal("accepted a truncated body")
	}
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer slow.Close()
	defer close(release)
	start := time.Now()
	if _, err := Fetch(context.Background(), FetchOptions{Client: slow.Client(), BaseURL: slow.URL, Parent: parent, Timeout: 100 * time.Millisecond}); err == nil {
		t.Fatal("no timeout")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("timeout not enforced")
	}
	if _, err := Fetch(context.Background(), FetchOptions{BaseURL: "http://127.0.0.1:1", Parent: "relative"}); err == nil {
		t.Fatal("accepted a relative parent")
	}
	entries, _ := os.ReadDir(parent)
	if len(entries) != 0 {
		t.Fatalf("failed fetches left %d entries", len(entries))
	}
}

func TestIdentify_Refusals(t *testing.T) {
	cases := map[string]func(dir string) error{
		"stray file":      func(d string) error { return os.WriteFile(filepath.Join(d, "notes.txt"), nil, 0o644) },
		"stray directory": func(d string) error { return os.Mkdir(filepath.Join(d, "ID", "sub"), 0o755) },
		"symlink entry": func(d string) error {
			return os.Symlink(filepath.Join(d, "ID", "GO-TEST-0001.json"), filepath.Join(d, "ID", "GO-TEST-0100.json"))
		},
		"missing db.json": func(d string) error { return os.Remove(filepath.Join(d, "index", "db.json")) },
		// Without index/modules.json the scanner silently reads the
		// directory as a flat list of OSV files.
		"missing modules.json": func(d string) error { return os.Remove(filepath.Join(d, "index", "modules.json")) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			d := fixtureDir(t)
			if err := mutate(d); err != nil {
				t.Fatal(err)
			}
			if _, err := Identify(d, Limits{}); err == nil {
				t.Fatal("identified")
			}
		})
	}
}

func TestVerify_DetectsChanges(t *testing.T) {
	cases := map[string]func(dir string) error{
		"content": func(d string) error {
			return os.WriteFile(filepath.Join(d, "ID", "GO-TEST-0001.json"), []byte(`{"id":"GO-TEST-0001"}`), 0o644)
		},
		"removed entry": func(d string) error { return os.Remove(filepath.Join(d, "ID", "GO-TEST-0006.json")) },
		"added entry": func(d string) error {
			return os.WriteFile(filepath.Join(d, "ID", "GO-TEST-0100.json"), []byte(`{"id":"GO-TEST-0100"}`), 0o644)
		},
		"modified time": func(d string) error {
			return os.WriteFile(filepath.Join(d, "index", "db.json"), []byte(`{"modified":"2026-01-02T00:00:00Z"}`), 0o644)
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			d := fixtureDir(t)
			id, err := Identify(d, Limits{})
			if err != nil {
				t.Fatal(err)
			}
			if err := mutate(d); err != nil {
				t.Fatal(err)
			}
			if err := Verify(d, id, Limits{}); !errors.Is(err, ErrSnapshotMismatch) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

// The identity depends on paths and contents only, not on file times or
// directory order.
func TestIdentify_IgnoresMetadata(t *testing.T) {
	d := fixtureDir(t)
	a, err := Identify(d, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	old := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	filepath.WalkDir(d, func(p string, _ fs.DirEntry, _ error) error { return os.Chtimes(p, old, old) })
	b, err := Identify(d, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("%+v != %+v", a, b)
	}
}

// VULN_REAL_ZIP names a downloaded https://vuln.go.dev/vulndb.zip. The
// test serves it locally, so it never reaches the network, and reports the
// snapshot identity and the time taken to validate and extract it.
func TestFetch_RealArchive(t *testing.T) {
	p := os.Getenv("VULN_REAL_ZIP")
	if p == "" {
		t.Skip("VULN_REAL_ZIP not set")
	}
	body, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	srv, _ := serve(t, body)
	start := time.Now()
	snap, err := Fetch(context.Background(), FetchOptions{Client: srv.Client(), BaseURL: srv.URL, Parent: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%d archive bytes -> %s in %s: %+v", len(body), filepath.Base(snap.Dir), time.Since(start), snap.ID)
}
