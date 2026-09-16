package lock_test

import (
	"bufio"
	"errors"
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
