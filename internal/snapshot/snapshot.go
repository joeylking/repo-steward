// Package snapshot builds candidate trees from a working tree and
// materializes a Git tree into a directory exactly: every supported entry's
// path, content, and mode is reproduced from raw objects and verified before
// the snapshot is accepted. Archive export and attribute processing are never
// involved, so export-ignore and filters cannot alter what is validated.
package snapshot

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/joeylking/repo-steward/internal/gitx"
)

// Limits bound what a snapshot may contain. They are enforced against the
// tree listing before any file is written.
type Limits struct {
	MaxFiles      int
	MaxTotalBytes int64
	MaxFileBytes  int64
}

// DefaultLimits are generous for source repositories and small for anything
// that looks like a data dump.
func DefaultLimits() Limits {
	return Limits{MaxFiles: 20000, MaxTotalBytes: 512 << 20, MaxFileBytes: 64 << 20}
}

// Entry is one regular file in a tree.
type Entry struct {
	Path string `json:"path"`
	Mode string `json:"mode"`
	Blob string `json:"blob"`
	Size int64  `json:"size"`
}

// Manifest describes an accepted snapshot.
type Manifest struct {
	Tree       string  `json:"tree"`
	Dir        string  `json:"dir"`
	Entries    []Entry `json:"entries"`
	FileCount  int     `json:"file_count"`
	TotalBytes int64   `json:"total_bytes"`
	Verified   bool    `json:"verified"`
}

var (
	// ErrUnsupportedEntry marks a tree with a symlink, submodule, or other
	// entry type the MVP refuses rather than reinterprets.
	ErrUnsupportedEntry = errors.New("snapshot: unsupported tree entry")
	// ErrLimit marks a tree that exceeds Limits.
	ErrLimit = errors.New("snapshot: limit exceeded")
	// ErrPath marks a path the snapshot refuses to write.
	ErrPath = errors.New("snapshot: unsafe path")
	// ErrVerify marks a materialized directory that does not match its tree.
	ErrVerify = errors.New("snapshot: verification failed")
)

// BuildCandidateTree writes the working tree of g as a tree object. The
// temporary index is seeded from baseTree, so tracked files stay tracked even
// when a later ignore rule matches them, ignored untracked files stay out,
// and new unignored files enter.
func BuildCandidateTree(ctx context.Context, g *gitx.Git, baseTree string) (string, error) {
	tmp, err := os.MkdirTemp("", "repo-steward-index-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	idx := filepath.Join(tmp, "index")
	if err := g.ReadTree(ctx, idx, baseTree); err != nil {
		return "", err
	}
	if err := g.AddAll(ctx, idx); err != nil {
		return "", err
	}
	return g.WriteTree(ctx, idx)
}

// List returns the regular-file entries of tree after checking every entry
// against the supported modes, path rules, and limits.
func List(ctx context.Context, g *gitx.Git, tree string, limits Limits) ([]Entry, error) {
	raw, err := g.LsTree(ctx, tree)
	if err != nil {
		return nil, err
	}
	entries := make([]Entry, 0, len(raw))
	for _, r := range raw {
		entries = append(entries, Entry{Path: r.Path, Mode: r.Mode, Blob: r.Blob, Size: r.Size})
		if r.Type != "blob" && r.Mode != "120000" && r.Mode != "160000" {
			return nil, fmt.Errorf("%w: %s has type %s", ErrUnsupportedEntry, r.Path, r.Type)
		}
	}
	if err := Check(entries, limits); err != nil {
		return nil, err
	}
	return entries, nil
}

// Check validates entries without touching the filesystem.
func Check(entries []Entry, limits Limits) error {
	if limits.MaxFiles > 0 && len(entries) > limits.MaxFiles {
		return fmt.Errorf("%w: %d files, limit %d", ErrLimit, len(entries), limits.MaxFiles)
	}
	var total int64
	seen := make(map[string]bool, len(entries))
	for _, e := range entries {
		switch e.Mode {
		case "100644", "100755":
		case "120000":
			return fmt.Errorf("%w: %s is a symbolic link", ErrUnsupportedEntry, e.Path)
		case "160000":
			return fmt.Errorf("%w: %s is a submodule", ErrUnsupportedEntry, e.Path)
		default:
			return fmt.Errorf("%w: %s has mode %s", ErrUnsupportedEntry, e.Path, e.Mode)
		}
		if err := checkPath(e.Path); err != nil {
			return err
		}
		if seen[e.Path] {
			return fmt.Errorf("%w: duplicate entry %s", ErrPath, e.Path)
		}
		seen[e.Path] = true
		if e.Size < 0 {
			return fmt.Errorf("%w: %s has no size", ErrUnsupportedEntry, e.Path)
		}
		if limits.MaxFileBytes > 0 && e.Size > limits.MaxFileBytes {
			return fmt.Errorf("%w: %s is %d bytes, limit %d", ErrLimit, e.Path, e.Size, limits.MaxFileBytes)
		}
		total += e.Size
		if limits.MaxTotalBytes > 0 && total > limits.MaxTotalBytes {
			return fmt.Errorf("%w: total exceeds %d bytes", ErrLimit, limits.MaxTotalBytes)
		}
	}
	return nil
}

// checkPath refuses anything that could escape or alias inside the
// destination. Git itself rejects most of these; the check is independent.
func checkPath(p string) error {
	if p == "" || strings.HasPrefix(p, "/") || strings.ContainsRune(p, 0) {
		return fmt.Errorf("%w: %q", ErrPath, p)
	}
	for _, c := range strings.Split(p, "/") {
		switch {
		case c == "", c == ".", c == "..":
			return fmt.Errorf("%w: %q", ErrPath, p)
		case strings.EqualFold(c, ".git"):
			return fmt.Errorf("%w: %q contains a .git component", ErrPath, p)
		}
	}
	return nil
}

// Materialize writes tree into dest, which must not exist or be empty, then
// verifies the result. The returned manifest has Verified set only when every
// entry was checked against its blob id and mode.
func Materialize(ctx context.Context, g *gitx.Git, tree, dest string, limits Limits) (*Manifest, error) {
	entries, err := List(ctx, g, tree, limits)
	if err != nil {
		return nil, err
	}
	if err := requireEmptyDir(dest); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return nil, err
	}
	byBlob := map[string][]Entry{}
	ids := make([]string, 0, len(entries))
	for _, e := range entries {
		if _, seen := byBlob[e.Blob]; !seen {
			ids = append(ids, e.Blob)
		}
		byBlob[e.Blob] = append(byBlob[e.Blob], e)
	}
	var total int64
	err = g.CatFileBatch(ctx, ids, func(id string, size int64, r io.Reader) error {
		targets := byBlob[id]
		if size != targets[0].Size {
			return fmt.Errorf("%w: blob %s declared %d bytes in the listing but %d in cat-file", ErrVerify, id, targets[0].Size, size)
		}
		content, err := io.ReadAll(r)
		if err != nil {
			return err
		}
		if int64(len(content)) != size {
			return fmt.Errorf("%w: blob %s truncated", ErrVerify, id)
		}
		for _, e := range targets {
			if err := writeEntry(dest, e, content); err != nil {
				return err
			}
			total += size
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	m := &Manifest{Tree: tree, Dir: dest, Entries: entries, FileCount: len(entries), TotalBytes: total}
	if err := Verify(dest, entries); err != nil {
		return m, err
	}
	m.Verified = true
	return m, nil
}

func writeEntry(dest string, e Entry, content []byte) error {
	perm := os.FileMode(0o644)
	if e.Mode == "100755" {
		perm = 0o755
	}
	p := filepath.Join(dest, filepath.FromSlash(e.Path))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(content); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Chmod(p, perm)
}

// Verify checks that dest contains exactly entries: same paths, no extra
// files, matching sizes, blob ids, and executable bits, and nothing but
// regular files and directories.
func Verify(dest string, entries []Entry) error {
	want := make(map[string]Entry, len(entries))
	for _, e := range entries {
		want[e.Path] = e
	}
	seen := 0
	err := filepath.WalkDir(dest, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dest, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%w: %s is not a regular file", ErrVerify, rel)
		}
		e, ok := want[rel]
		if !ok {
			return fmt.Errorf("%w: unexpected file %s", ErrVerify, rel)
		}
		seen++
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Size() != e.Size {
			return fmt.Errorf("%w: %s is %d bytes, tree says %d", ErrVerify, rel, info.Size(), e.Size)
		}
		execBit := info.Mode().Perm()&0o100 != 0
		if execBit != (e.Mode == "100755") {
			return fmt.Errorf("%w: %s executable bit is %v, tree mode is %s", ErrVerify, rel, execBit, e.Mode)
		}
		id, err := blobID(p, e.Size)
		if err != nil {
			return err
		}
		if id != e.Blob {
			return fmt.Errorf("%w: %s content hash %s, tree says %s", ErrVerify, rel, id, e.Blob)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if seen != len(entries) {
		return fmt.Errorf("%w: %d files on disk, tree has %d", ErrVerify, seen, len(entries))
	}
	return nil
}

// blobID computes the Git object id of a file's content as a blob.
func blobID(p string, size int64) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha1.New()
	h.Write([]byte("blob " + strconv.FormatInt(size, 10) + "\x00"))
	n, err := io.Copy(h, f)
	if err != nil {
		return "", err
	}
	if n != size {
		return "", fmt.Errorf("%w: %s changed size while hashing", ErrVerify, p)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func requireEmptyDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(entries) > 0 {
		return fmt.Errorf("snapshot: destination %s is not empty", dir)
	}
	return nil
}
