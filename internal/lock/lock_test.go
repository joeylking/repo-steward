package lock_test

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/joeylking/repo-steward/internal/lock"
)

// TestMain doubles as the lock-holder helper process when LOCK_HELPER_PATH
// is set: it acquires the lock, prints "ready", and waits until stdin closes.
func TestMain(m *testing.M) {
	if p := os.Getenv("LOCK_HELPER_PATH"); p != "" {
		l, err := lock.Acquire(p)
		if err != nil {
			os.Stdout.WriteString("error: " + err.Error() + "\n")
			os.Exit(2)
		}
		os.Stdout.WriteString("ready\n")
		buf := make([]byte, 1)
		os.Stdin.Read(buf) // blocks until the parent closes stdin or kills us
		l.Release()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func startHolder(t *testing.T, path string) (*exec.Cmd, *os.File) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "LOCK_HELPER_PATH="+path)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "ready" {
		cmd.Process.Kill()
		t.Fatalf("helper did not become ready: %q %v", line, err)
	}
	return cmd, stdin.(*os.File)
}

func TestAcquire_ExclusiveWithinProcess(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.lock")
	a, err := lock.Acquire(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lock.Acquire(p); !errors.Is(err, lock.ErrHeld) {
		t.Fatalf("second acquire = %v, want ErrHeld", err)
	}
	if err := a.Release(); err != nil {
		t.Fatal(err)
	}
	b, err := lock.Acquire(p)
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	b.Release()
	if err := b.Release(); err != nil {
		t.Fatalf("double release: %v", err)
	}
}

func TestLock_SuspendedHolderBlocksSecondProcess(t *testing.T) {
	p := filepath.Join(t.TempDir(), "run.lock")
	cmd, stdin := startHolder(t, p)
	defer cmd.Process.Kill()

	if _, err := lock.Acquire(p); !errors.Is(err, lock.ErrHeld) {
		t.Fatalf("acquire while held = %v", err)
	}
	if err := cmd.Process.Signal(syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	// A stopped holder is still a holder: no heartbeat, no takeover.
	if _, err := lock.Acquire(p); !errors.Is(err, lock.ErrHeld) {
		t.Fatalf("acquire while holder suspended = %v, want ErrHeld", err)
	}
	if err := cmd.Process.Signal(syscall.SIGCONT); err != nil {
		t.Fatal(err)
	}
	stdin.Close() // holder resumes, releases, and exits normally
	if err := cmd.Wait(); err != nil {
		t.Fatalf("holder exit: %v", err)
	}
	l, err := lock.Acquire(p)
	if err != nil {
		t.Fatalf("acquire after holder exited: %v", err)
	}
	l.Release()
}

func TestLock_KilledHolderReleases(t *testing.T) {
	p := filepath.Join(t.TempDir(), "run.lock")
	cmd, _ := startHolder(t, p)
	if _, err := lock.Acquire(p); !errors.Is(err, lock.ErrHeld) {
		t.Fatalf("acquire while held = %v", err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	cmd.Wait()
	l, err := lock.Acquire(p)
	if err != nil {
		t.Fatalf("acquire after SIGKILL: %v", err)
	}
	l.Release()
}

func TestCheckLocal_TempDir(t *testing.T) {
	if err := lock.CheckLocal(filepath.Join(t.TempDir(), "data")); err != nil {
		t.Fatal(err)
	}
}

// The data directory is created owner-only; an existing one that group or
// others can only read or enter is tightened, and one they can write is
// refused with an error naming the path, its mode, and the fix.
func TestPrivate_CreatesTightensOrRefuses(t *testing.T) {
	root := t.TempDir()
	mode := func(p string) os.FileMode {
		t.Helper()
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		return fi.Mode().Perm()
	}
	created := filepath.Join(root, "a", "data")
	if err := lock.CheckLocal(created); err != nil {
		t.Fatal(err)
	}
	if m := mode(created); m != 0o700 {
		t.Fatalf("created %v, want 0700", m)
	}
	for _, m := range []os.FileMode{0o755, 0o750, 0o705, 0o744} {
		dir := filepath.Join(root, fmt.Sprintf("tighten-%o", m))
		os.Mkdir(dir, 0o700)
		os.Chmod(dir, m)
		if err := lock.Private(dir); err != nil {
			t.Fatalf("%v: %v", m, err)
		}
		if got := mode(dir); got != 0o700 {
			t.Fatalf("%v tightened to %v, want 0700", m, got)
		}
	}
	for _, m := range []os.FileMode{0o777, 0o775, 0o757, 0o720, 0o702} {
		dir := filepath.Join(root, fmt.Sprintf("refuse-%o", m))
		os.Mkdir(dir, 0o700)
		os.Chmod(dir, m)
		err := lock.CheckLocal(dir)
		var insecure lock.ErrInsecureDir
		if !errors.As(err, &insecure) || insecure.Path != dir {
			t.Fatalf("%v: error %v, want ErrInsecureDir for %s", m, err, dir)
		}
		for _, want := range []string{dir, (m | os.ModeDir).String(), "chmod 700 " + dir} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("%v: error %q does not name %q", m, err, want)
			}
		}
		if got := mode(dir); got != m {
			t.Fatalf("refused directory changed from %v to %v", m, got)
		}
	}
	file := filepath.Join(root, "file")
	os.WriteFile(file, nil, 0o600)
	if err := lock.Private(file); err == nil {
		t.Fatal("a file accepted as the data directory")
	}
}
