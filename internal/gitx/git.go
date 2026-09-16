// Package gitx wraps the Git command line with a fixed argument list, no
// shell, and an environment that ignores the operator's global and system
// configuration. Every command runs against one working directory.
package gitx

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Git runs commands in Dir.
type Git struct {
	Dir string
}

// New returns a Git bound to dir. dir need not exist yet for Init.
func New(dir string) *Git { return &Git{Dir: dir} }

var (
	emptyDirOnce sync.Once
	emptyDir     string
	emptyDirErr  error
)

// isolationDir returns an empty directory used as HOME and as the hooks path
// so that no user configuration or hook can influence a command.
func isolationDir() (string, error) {
	emptyDirOnce.Do(func() {
		d := filepath.Join(os.TempDir(), "repo-steward-git-isolation")
		if err := os.MkdirAll(d, 0o700); err != nil {
			emptyDirErr = err
			return
		}
		emptyDir = d
	})
	return emptyDir, emptyDirErr
}

// Env returns the environment every command runs with. It is built from
// scratch; nothing from the parent process is inherited except PATH.
func Env() ([]string, error) {
	iso, err := isolationDir()
	if err != nil {
		return nil, err
	}
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + iso,
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_OPTIONAL_LOCKS=0",
		"LC_ALL=C",
		"TZ=UTC",
	}, nil
}

// Error carries the failed command and its stderr.
type Error struct {
	Args   []string
	Stderr string
	Err    error
}

func (e *Error) Error() string {
	return fmt.Sprintf("git %s: %v: %s", strings.Join(e.Args, " "), e.Err, strings.TrimSpace(e.Stderr))
}

func (e *Error) Unwrap() error { return e.Err }

func (g *Git) command(ctx context.Context, extraEnv []string, args ...string) (*exec.Cmd, error) {
	iso, err := isolationDir()
	if err != nil {
		return nil, err
	}
	env, err := Env()
	if err != nil {
		return nil, err
	}
	full := append([]string{
		"-c", "core.hooksPath=" + iso,
		"-c", "protocol.file.allow=always",
		"-c", "submodule.recurse=false",
		"-c", "advice.detachedHead=false",
	}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Dir = g.Dir
	cmd.Env = append(env, extraEnv...)
	return cmd, nil
}

// Run executes git with args and returns stdout.
func (g *Git) Run(ctx context.Context, args ...string) ([]byte, error) {
	return g.RunWith(ctx, nil, nil, args...)
}

// RunWith executes git with extra environment variables and stdin.
func (g *Git) RunWith(ctx context.Context, extraEnv []string, stdin []byte, args ...string) ([]byte, error) {
	cmd, err := g.command(ctx, extraEnv, args...)
	if err != nil {
		return nil, err
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), &Error{Args: args, Stderr: stderr.String(), Err: err}
	}
	return stdout.Bytes(), nil
}

// Init creates an empty repository with a main branch and no commits.
func (g *Git) Init(ctx context.Context) error {
	if err := os.MkdirAll(g.Dir, 0o755); err != nil {
		return err
	}
	_, err := g.Run(ctx, "-c", "init.defaultBranch=main", "init", "-q")
	return err
}

// Identity is a commit author or committer with an explicit timestamp.
type Identity struct {
	Name  string
	Email string
	When  time.Time
}

// gitDate formats When as "<unix> <+hhmm>", the internal Git date format.
func (id Identity) gitDate() string {
	return strconv.FormatInt(id.When.Unix(), 10) + " " + id.When.Format("-0700")
}

func (id Identity) env(role string) []string {
	return []string{
		"GIT_" + role + "_NAME=" + id.Name,
		"GIT_" + role + "_EMAIL=" + id.Email,
		"GIT_" + role + "_DATE=" + id.gitDate(),
	}
}

// CommitRecipe is every input to a commit object. Two commits built from equal
// recipes have equal ids regardless of ambient configuration.
type CommitRecipe struct {
	Tree      string
	Parents   []string
	Author    Identity
	Committer Identity
	// Message is written to the commit byte for byte. It should end with a
	// newline; commit-tree does not add one.
	Message []byte
}

// CommitTree writes a commit object from the recipe and returns its id. It
// does not move any ref.
func (g *Git) CommitTree(ctx context.Context, r CommitRecipe) (string, error) {
	if r.Tree == "" {
		return "", errors.New("gitx: commit recipe has no tree")
	}
	if r.Author.Name == "" || r.Author.Email == "" || r.Author.When.IsZero() ||
		r.Committer.Name == "" || r.Committer.Email == "" || r.Committer.When.IsZero() {
		return "", errors.New("gitx: commit recipe requires complete author and committer identities")
	}
	args := []string{"commit-tree", r.Tree}
	for _, p := range r.Parents {
		args = append(args, "-p", p)
	}
	env := append(r.Author.env("AUTHOR"), r.Committer.env("COMMITTER")...)
	out, err := g.RunWith(ctx, env, r.Message, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// HashObject writes content as a blob with no filters applied and returns its id.
func (g *Git) HashObject(ctx context.Context, content []byte) (string, error) {
	out, err := g.RunWith(ctx, nil, content, "hash-object", "-w", "--stdin", "--no-filters")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// IndexEntry is one entry to place in an index.
type IndexEntry struct {
	Mode string // "100644", "100755", "120000", "160000"
	Blob string
	Path string
}

// UpdateIndex adds entries to the index file at indexFile (or the repository
// index when empty) using NUL-delimited index-info, which handles any path.
func (g *Git) UpdateIndex(ctx context.Context, indexFile string, entries []IndexEntry) error {
	var in bytes.Buffer
	for _, e := range entries {
		if strings.ContainsRune(e.Path, 0) {
			return fmt.Errorf("gitx: path contains NUL: %q", e.Path)
		}
		fmt.Fprintf(&in, "%s %s\t%s\x00", e.Mode, e.Blob, e.Path)
	}
	_, err := g.RunWith(ctx, indexEnv(indexFile), in.Bytes(), "update-index", "-z", "--add", "--index-info")
	return err
}

func indexEnv(indexFile string) []string {
	if indexFile == "" {
		return nil
	}
	return []string{"GIT_INDEX_FILE=" + indexFile}
}

// ReadTree loads tree into the index file (or the repository index when empty).
func (g *Git) ReadTree(ctx context.Context, indexFile, tree string) error {
	_, err := g.RunWith(ctx, indexEnv(indexFile), nil, "read-tree", tree)
	return err
}

// AddAll stages every change in the working tree into the index file,
// honouring ignore rules for untracked files.
func (g *Git) AddAll(ctx context.Context, indexFile string) error {
	_, err := g.RunWith(ctx, indexEnv(indexFile), nil, "add", "-A", "--", ".")
	return err
}

// WriteTree writes the index file (or the repository index) as a tree.
func (g *Git) WriteTree(ctx context.Context, indexFile string) (string, error) {
	out, err := g.RunWith(ctx, indexEnv(indexFile), nil, "write-tree")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// UpdateRef points ref at object.
func (g *Git) UpdateRef(ctx context.Context, ref, object string) error {
	_, err := g.Run(ctx, "update-ref", ref, object)
	return err
}

// SymbolicRef points a symbolic ref such as HEAD at target.
func (g *Git) SymbolicRef(ctx context.Context, name, target string) error {
	_, err := g.Run(ctx, "symbolic-ref", name, target)
	return err
}

// RevParse resolves rev to an object id.
func (g *Git) RevParse(ctx context.Context, rev string) (string, error) {
	out, err := g.Run(ctx, "rev-parse", "--verify", rev)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// ResetHard makes the working tree and index match HEAD. Untracked files are
// left alone.
func (g *Git) ResetHard(ctx context.Context) error {
	_, err := g.Run(ctx, "reset", "-q", "--hard")
	return err
}

// TreeEntry is one row of a recursive tree listing.
type TreeEntry struct {
	Mode string
	Type string
	Blob string
	Size int64 // -1 for non-blobs
	Path string
}

// LsTree lists tree recursively with sizes, NUL-delimited so any path is
// parsed exactly.
func (g *Git) LsTree(ctx context.Context, tree string) ([]TreeEntry, error) {
	out, err := g.Run(ctx, "ls-tree", "-r", "-l", "-z", tree)
	if err != nil {
		return nil, err
	}
	var entries []TreeEntry
	for len(out) > 0 {
		i := bytes.IndexByte(out, 0)
		if i < 0 {
			return nil, errors.New("gitx: ls-tree output not NUL-terminated")
		}
		rec := out[:i]
		out = out[i+1:]
		tab := bytes.IndexByte(rec, '\t')
		if tab < 0 {
			return nil, fmt.Errorf("gitx: malformed ls-tree record %q", rec)
		}
		meta := strings.Fields(string(rec[:tab]))
		if len(meta) != 4 {
			return nil, fmt.Errorf("gitx: malformed ls-tree metadata %q", rec[:tab])
		}
		e := TreeEntry{Mode: meta[0], Type: meta[1], Blob: meta[2], Size: -1, Path: string(rec[tab+1:])}
		if meta[3] != "-" {
			n, err := strconv.ParseInt(meta[3], 10, 64)
			if err != nil {
				return nil, fmt.Errorf("gitx: bad size in ls-tree record %q", rec)
			}
			e.Size = n
		}
		entries = append(entries, e)
	}
	return entries, nil
}

// CatFileBatch streams the objects named by ids in order. fn receives each
// object's declared size and a reader limited to exactly that many bytes,
// so content is parsed by size and never by delimiter.
func (g *Git) CatFileBatch(ctx context.Context, ids []string, fn func(id string, size int64, r io.Reader) error) error {
	cmd, err := g.command(ctx, nil, "cat-file", "--batch")
	if err != nil {
		return err
	}
	var in bytes.Buffer
	for _, id := range ids {
		in.WriteString(id)
		in.WriteByte('\n')
	}
	cmd.Stdin = &in
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	br := bufio.NewReaderSize(stdout, 1<<20)
	var loopErr error
	for _, want := range ids {
		header, err := br.ReadString('\n')
		if err != nil {
			loopErr = fmt.Errorf("gitx: cat-file header for %s: %w", want, err)
			break
		}
		fields := strings.Fields(header)
		if len(fields) == 2 && fields[1] == "missing" {
			loopErr = fmt.Errorf("gitx: object %s missing", want)
			break
		}
		if len(fields) != 3 || fields[0] != want {
			loopErr = fmt.Errorf("gitx: unexpected cat-file header %q for %s", strings.TrimSpace(header), want)
			break
		}
		size, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil || size < 0 {
			loopErr = fmt.Errorf("gitx: bad size in cat-file header %q", strings.TrimSpace(header))
			break
		}
		lr := &io.LimitedReader{R: br, N: size}
		if err := fn(want, size, lr); err != nil {
			loopErr = err
			break
		}
		if lr.N != 0 {
			if _, err := io.Copy(io.Discard, lr); err != nil {
				loopErr = err
				break
			}
		}
		// cat-file appends a newline after each object.
		if b, err := br.ReadByte(); err != nil || b != '\n' {
			loopErr = fmt.Errorf("gitx: missing object terminator after %s", want)
			break
		}
	}
	if loopErr != nil {
		cmd.Process.Kill()
		cmd.Wait()
		return loopErr
	}
	if err := cmd.Wait(); err != nil {
		return &Error{Args: []string{"cat-file", "--batch"}, Stderr: stderr.String(), Err: err}
	}
	return nil
}
