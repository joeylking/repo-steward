// Package lock provides exclusive, OS-held advisory locks for the local CLI.
// A lock is released by the kernel when the holding process exits by any
// means, and a suspended holder keeps its lock, so there is no takeover and
// no expiry. The data directory must be on a local filesystem.
package lock

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

// ErrHeld is returned when another open file description holds the lock.
var ErrHeld = errors.New("lock: held by another process")

// Lock is an acquired exclusive lock.
type Lock struct {
	f    *os.File
	path string
}

// Acquire takes an exclusive non-blocking lock on path, creating the file if
// needed. On success the file records the holder's pid, host, and time for
// display; that content is informational only.
func Acquire(path string) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		holder, _ := os.ReadFile(path)
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, fmt.Errorf("%w: %s (%s)", ErrHeld, path, string(holder))
		}
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	host, _ := os.Hostname()
	info := strconv.Itoa(os.Getpid()) + " " + host + " " + time.Now().UTC().Format(time.RFC3339)
	f.Truncate(0)
	f.Seek(0, 0)
	f.WriteString(info)
	return &Lock{f: f, path: path}, nil
}

// Path returns the lock file path.
func (l *Lock) Path() string { return l.path }

// Release unlocks and closes the file. Releasing twice is safe.
func (l *Lock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	err := syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	cerr := l.f.Close()
	l.f = nil
	if err != nil {
		return err
	}
	return cerr
}

// ErrNotLocal is returned when the directory is on a filesystem where
// advisory locks are unreliable.
var ErrNotLocal = errors.New("lock: directory is not on a local filesystem")

// CheckLocal verifies that dir is on a local filesystem and owner-only
// (see Private). It creates dir if necessary.
func CheckLocal(dir string) error {
	if err := Private(dir); err != nil {
		return err
	}
	name, err := fsTypeName(dir)
	if err != nil {
		return err
	}
	switch name {
	case "nfs", "nfs4", "smbfs", "cifs", "smb2", "afpfs", "webdav", "fuse.sshfs":
		return fmt.Errorf("%w: %s is %s", ErrNotLocal, dir, name)
	}
	return nil
}

// ErrInsecureDir is returned by Private for a data directory that another
// account owns or could write. The sandbox makes the caches and staging
// directories below the data directory world-writable for the container
// user, so the data directory is what keeps other local accounts out.
type ErrInsecureDir struct {
	Path   string
	Mode   fs.FileMode
	Reason string
}

func (e ErrInsecureDir) Error() string {
	return fmt.Sprintf("lock: %s is %s, %s; repo-steward keeps its data directory accessible to its owner only because the caches below it are world-writable: chmod 700 %s and chown it to the user that runs repo-steward", e.Path, e.Mode, e.Reason, e.Path)
}

// Private makes dir an owner-only directory. A missing dir is created with
// mode 0700. An existing one owned by another account, or that group or
// others can write, is refused with ErrInsecureDir; one they can only read
// or enter is tightened to 0700.
func Private(dir string) error {
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return err
	}
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	fi, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("lock: %s is not a directory", dir)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return ErrInsecureDir{Path: dir, Mode: fi.Mode(), Reason: "owned by another user"}
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return ErrInsecureDir{Path: dir, Mode: fi.Mode(), Reason: "writable by group or others"}
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return os.Chmod(dir, 0o700)
	}
	return nil
}
