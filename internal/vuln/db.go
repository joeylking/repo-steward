package vuln

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// The offline database is the layout govulncheck reads through
// -db file://<dir> (https://go.dev/security/vuln/database#api):
//
//	index/db.json       {"modified": RFC 3339 time}
//	index/modules.json  [{"path", "vulns": [{"id", "modified", "fixed"}]}]
//	index/vulns.json    [{"id", "modified", "aliases"}]
//	ID/<id>.json        one OSV entry
//
// vuln.go.dev publishes the same files as one archive, vulndb.zip. The
// scanner's local client decides the layout by the presence of
// index/modules.json; without it, it silently falls back to reading the
// directory as a flat list of OSV files. Identify and Verify therefore
// refuse any directory that is not exactly this layout.

const (
	dbMetaPath  = "index/db.json"
	modulesPath = "index/modules.json"
	vulnsPath   = "index/vulns.json"
)

// entryName is the only shape an ID file may have. It excludes path
// separators and dot segments, so no accepted name can leave the directory.
var entryName = regexp.MustCompile(`^ID/[A-Za-z0-9][A-Za-z0-9-]{0,63}\.json$`)

func allowedPath(p string) bool {
	switch p {
	case dbMetaPath, modulesPath, vulnsPath:
		return true
	}
	return entryName.MatchString(p)
}

// SnapshotID identifies a database snapshot by content. Scan evidence binds
// to Hash; Modified is the database's own last-modified time from
// index/db.json and is what govulncheck reports as db_last_modified.
type SnapshotID struct {
	// Hash is "sha256:" plus the hex digest of, for each file in byte order
	// of its slash-separated relative path, the line
	// "<path> <hex sha256 of contents>\n".
	Hash     string    `json:"hash"`
	Modified time.Time `json:"modified"`
	Files    int       `json:"files"`
	Bytes    int64     `json:"bytes"`
}

// Limits bound a fetch and an identification.
type Limits struct {
	MaxArchiveBytes int64 // compressed download
	MaxTotalBytes   int64 // sum of extracted file sizes
	MaxEntryBytes   int64 // any one file
	MaxEntries      int
}

// DefaultLimits are roughly ten times the October 2026 database (3.4 MB
// archive, 6.9 MB extracted, 4580 files).
var DefaultLimits = Limits{
	MaxArchiveBytes: 64 << 20,
	MaxTotalBytes:   256 << 20,
	MaxEntryBytes:   8 << 20,
	MaxEntries:      100000,
}

// DefaultBaseURL is the public Go vulnerability database.
const DefaultBaseURL = "https://vuln.go.dev"

// FetchOptions configure Fetch.
type FetchOptions struct {
	// Client performs the download. Nil means a client with Timeout.
	Client *http.Client
	// BaseURL is the database root; the archive is BaseURL + "/vulndb.zip".
	BaseURL string
	// Parent is the directory the snapshot is created in. It must exist.
	Parent string
	// Timeout bounds the whole fetch; zero means two minutes.
	Timeout time.Duration
	Limits  Limits
}

// Snapshot is a database directory and its identity.
type Snapshot struct {
	Dir string     `json:"dir"`
	ID  SnapshotID `json:"id"`
	// Reused is true when an identical snapshot already existed and was
	// verified instead of being replaced.
	Reused bool `json:"reused,omitempty"`
}

// Fetch downloads the database archive, validates and extracts it into a
// private temporary directory under Parent, identifies it, and renames it
// to a content-addressed name, vulndb-<modified>-<hash prefix>. An existing
// directory with that name is verified and reused, never deleted and
// recreated: VM-backed engines cache path lookups, so a mounted path must
// not disappear and come back.
func Fetch(ctx context.Context, opts FetchOptions) (*Snapshot, error) {
	if opts.BaseURL == "" {
		opts.BaseURL = DefaultBaseURL
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 2 * time.Minute
	}
	if opts.Limits == (Limits{}) {
		opts.Limits = DefaultLimits
	}
	if opts.Client == nil {
		opts.Client = &http.Client{Timeout: opts.Timeout}
	}
	if !filepath.IsAbs(opts.Parent) {
		return nil, fmt.Errorf("vuln: snapshot parent must be absolute: %q", opts.Parent)
	}
	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	tmp, err := os.MkdirTemp(opts.Parent, ".vulndb-tmp-")
	if err != nil {
		return nil, err
	}
	// Removed unless it is renamed into place.
	defer os.RemoveAll(tmp)

	archive := filepath.Join(tmp, "archive.zip")
	if err := download(ctx, opts.Client, strings.TrimRight(opts.BaseURL, "/")+"/vulndb.zip", archive, opts.Limits.MaxArchiveBytes); err != nil {
		return nil, err
	}
	dbDir := filepath.Join(tmp, "db")
	if err := extract(archive, dbDir, opts.Limits); err != nil {
		return nil, err
	}
	id, err := Identify(dbDir, opts.Limits)
	if err != nil {
		return nil, err
	}
	if err := CheckConsistency(dbDir); err != nil {
		return nil, err
	}
	name := SnapshotDirName(id)
	final := filepath.Join(opts.Parent, name)
	if _, err := os.Lstat(final); err == nil {
		if err := Verify(final, id, opts.Limits); err != nil {
			return nil, fmt.Errorf("vuln: existing snapshot %s does not match its name: %w", final, err)
		}
		return &Snapshot{Dir: final, ID: id, Reused: true}, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if err := os.Rename(dbDir, final); err != nil {
		return nil, err
	}
	return &Snapshot{Dir: final, ID: id}, nil
}

// SnapshotDirName is the content-addressed directory name of a snapshot.
func SnapshotDirName(id SnapshotID) string {
	h := strings.TrimPrefix(id.Hash, "sha256:")
	if len(h) > 16 {
		h = h[:16]
	}
	return "vulndb-" + id.Modified.UTC().Format("20060102T150405Z") + "-" + h
}

func download(ctx context.Context, client *http.Client, url, dest string, limit int64) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("vuln: fetch %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("vuln: fetch %s: %s", url, resp.Status)
	}
	if resp.ContentLength > limit {
		return fmt.Errorf("vuln: fetch %s: archive is %d bytes, limit %d", url, resp.ContentLength, limit)
	}
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, io.LimitReader(resp.Body, limit+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("vuln: fetch %s: %w", url, err)
	}
	if n > limit {
		return fmt.Errorf("vuln: fetch %s: archive exceeds %d bytes", url, limit)
	}
	if resp.ContentLength >= 0 && n != resp.ContentLength {
		return fmt.Errorf("vuln: fetch %s: got %d of %d bytes", url, n, resp.ContentLength)
	}
	return nil
}

// extract writes the archive's files into dir, which must not exist. Every
// entry name is checked against the fixed layout before anything is
// written, so traversal, absolute paths, links, and stray files are refused
// rather than skipped.
func extract(archive, dir string, lim Limits) error {
	zr, err := zip.OpenReader(archive)
	if err != nil {
		return fmt.Errorf("vuln: open archive: %w", err)
	}
	defer zr.Close()
	if len(zr.File) > lim.MaxEntries {
		return fmt.Errorf("vuln: archive has %d entries, limit %d", len(zr.File), lim.MaxEntries)
	}
	seen := map[string]bool{}
	var declared uint64
	for _, f := range zr.File {
		if strings.HasSuffix(f.Name, "/") && f.FileInfo().IsDir() {
			if f.Name != "index/" && f.Name != "ID/" {
				return fmt.Errorf("vuln: archive entry %q is not part of the database layout", f.Name)
			}
			continue
		}
		if !allowedPath(f.Name) {
			return fmt.Errorf("vuln: archive entry %q is not part of the database layout", f.Name)
		}
		if !f.Mode().IsRegular() {
			return fmt.Errorf("vuln: archive entry %q is not a regular file", f.Name)
		}
		if seen[f.Name] {
			return fmt.Errorf("vuln: archive entry %q appears twice", f.Name)
		}
		seen[f.Name] = true
		if f.UncompressedSize64 > uint64(lim.MaxEntryBytes) {
			return fmt.Errorf("vuln: archive entry %q declares %d bytes, limit %d", f.Name, f.UncompressedSize64, lim.MaxEntryBytes)
		}
		declared += f.UncompressedSize64
	}
	if declared > uint64(lim.MaxTotalBytes) {
		return fmt.Errorf("vuln: archive declares %d bytes, limit %d", declared, lim.MaxTotalBytes)
	}
	for _, p := range []string{dbMetaPath, modulesPath, vulnsPath} {
		if !seen[p] {
			return fmt.Errorf("vuln: archive has no %s", p)
		}
	}
	for _, sub := range []string{"", "index", "ID"} {
		if err := os.Mkdir(filepath.Join(dir, sub), 0o755); err != nil {
			return err
		}
	}
	var total int64
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		// Declared sizes are not trusted: the copy is bounded as well.
		n, err := extractOne(f, filepath.Join(dir, filepath.FromSlash(f.Name)), lim.MaxEntryBytes)
		if err != nil {
			return err
		}
		total += n
		if total > lim.MaxTotalBytes {
			return fmt.Errorf("vuln: archive exceeds %d extracted bytes", lim.MaxTotalBytes)
		}
	}
	return nil
}

func extractOne(f *zip.File, dest string, limit int64) (int64, error) {
	rc, err := f.Open()
	if err != nil {
		return 0, fmt.Errorf("vuln: archive entry %q: %w", f.Name, err)
	}
	defer rc.Close()
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(out, io.LimitReader(rc, limit+1))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return n, fmt.Errorf("vuln: archive entry %q: %w", f.Name, err)
	}
	if n > limit {
		return n, fmt.Errorf("vuln: archive entry %q exceeds %d bytes", f.Name, limit)
	}
	return n, nil
}

// Identify computes the identity of a database directory. It refuses a
// directory that holds anything outside the fixed layout, a symbolic link,
// or a non-regular file, and requires index/db.json to carry a modified
// time.
func Identify(dir string, lim Limits) (SnapshotID, error) {
	if lim == (Limits{}) {
		lim = DefaultLimits
	}
	var id SnapshotID
	var paths []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("vuln: %s is a symbolic link", rel)
		}
		if d.IsDir() {
			if rel != "index" && rel != "ID" {
				return fmt.Errorf("vuln: unexpected directory %s", rel)
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("vuln: %s is not a regular file", rel)
		}
		if !allowedPath(rel) {
			return fmt.Errorf("vuln: unexpected file %s", rel)
		}
		paths = append(paths, rel)
		if len(paths) > lim.MaxEntries {
			return fmt.Errorf("vuln: more than %d files", lim.MaxEntries)
		}
		return nil
	})
	if err != nil {
		return id, err
	}
	sort.Strings(paths)
	h := sha256.New()
	for _, rel := range paths {
		sum, n, err := hashFile(filepath.Join(dir, filepath.FromSlash(rel)), lim.MaxEntryBytes)
		if err != nil {
			return id, err
		}
		id.Bytes += n
		if id.Bytes > lim.MaxTotalBytes {
			return id, fmt.Errorf("vuln: database exceeds %d bytes", lim.MaxTotalBytes)
		}
		fmt.Fprintf(h, "%s %s\n", rel, sum)
	}
	id.Files = len(paths)
	id.Hash = "sha256:" + hex.EncodeToString(h.Sum(nil))
	for _, p := range []string{dbMetaPath, modulesPath, vulnsPath} {
		if i := sort.SearchStrings(paths, p); i >= len(paths) || paths[i] != p {
			return id, fmt.Errorf("vuln: database has no %s", p)
		}
	}
	raw, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(dbMetaPath)))
	if err != nil {
		return id, err
	}
	var meta struct {
		Modified *time.Time `json:"modified"`
	}
	if err := json.Unmarshal(raw, &meta); err != nil {
		return id, fmt.Errorf("vuln: %s: %w", dbMetaPath, err)
	}
	if meta.Modified == nil || meta.Modified.IsZero() {
		return id, fmt.Errorf("vuln: %s has no modified time", dbMetaPath)
	}
	id.Modified = meta.Modified.UTC()
	return id, nil
}

func hashFile(p string, limit int64) (string, int64, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, limit+1))
	if err != nil {
		return "", n, err
	}
	if n > limit {
		return "", n, fmt.Errorf("vuln: %s exceeds %d bytes", p, limit)
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// ErrSnapshotMismatch means a directory is not the recorded snapshot.
var ErrSnapshotMismatch = errors.New("vuln: database snapshot does not match its recorded identity")

// Verify checks that dir is exactly the snapshot recorded as want.
func Verify(dir string, want SnapshotID, lim Limits) error {
	got, err := Identify(dir, lim)
	if err != nil {
		return err
	}
	if got.Hash != want.Hash || got.Files != want.Files || got.Bytes != want.Bytes || !got.Modified.Equal(want.Modified) {
		return fmt.Errorf("%w: have %s (%d files, %d bytes, modified %s), want %s (%d files, %d bytes, modified %s)",
			ErrSnapshotMismatch, got.Hash, got.Files, got.Bytes, got.Modified.Format(time.RFC3339), want.Hash, want.Files, want.Bytes, want.Modified.Format(time.RFC3339))
	}
	return nil
}

// CheckConsistency requires the indexes to agree with the entries: every
// ID file is listed in vulns.json and names itself, and every ID the
// modules index refers to exists. The scanner trusts modules.json to decide
// which entries to read, so an entry missing from it would be invisible.
func CheckConsistency(dir string) error {
	var vulns []struct {
		ID string `json:"id"`
	}
	if err := readJSON(filepath.Join(dir, "index", "vulns.json"), &vulns); err != nil {
		return err
	}
	var mods []struct {
		Path  string `json:"path"`
		Vulns []struct {
			ID string `json:"id"`
		} `json:"vulns"`
	}
	if err := readJSON(filepath.Join(dir, "index", "modules.json"), &mods); err != nil {
		return err
	}
	entries, err := os.ReadDir(filepath.Join(dir, "ID"))
	if err != nil {
		return err
	}
	files := map[string]bool{}
	for _, e := range entries {
		id := strings.TrimSuffix(e.Name(), ".json")
		files[id] = true
		var head struct {
			ID string `json:"id"`
		}
		if err := readJSON(filepath.Join(dir, "ID", e.Name()), &head); err != nil {
			return err
		}
		if head.ID != id {
			return fmt.Errorf("vuln: ID/%s declares id %q", e.Name(), head.ID)
		}
	}
	listed := map[string]bool{}
	for _, v := range vulns {
		if !files[v.ID] {
			return fmt.Errorf("vuln: index/vulns.json lists %s with no entry file", v.ID)
		}
		listed[v.ID] = true
	}
	for id := range files {
		if !listed[id] {
			return fmt.Errorf("vuln: entry %s is not in index/vulns.json", id)
		}
	}
	for _, m := range mods {
		if m.Path == "" {
			return errors.New("vuln: index/modules.json has an entry with no path")
		}
		for _, v := range m.Vulns {
			if !files[v.ID] {
				return fmt.Errorf("vuln: index/modules.json lists %s for %s with no entry file", v.ID, m.Path)
			}
		}
	}
	return nil
}

func readJSON(p string, v any) error {
	raw, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("vuln: %s: %w", path.Base(filepath.ToSlash(p)), err)
	}
	return nil
}
