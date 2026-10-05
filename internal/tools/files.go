package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/joeylking/repo-steward/internal/faultpoint"
	"github.com/joeylking/repo-steward/internal/session"
	"github.com/joeylking/repo-steward/internal/steward/names"
)

// File content reaches the model in windows of whole lines, each line
// prefixed with its number, so that a long file can be read to the end and a
// compiler's file:line can be found without counting.
const (
	maxWindowBytes = 20000
	maxListed      = 60
)

// numbered is one window of a text file.
type numbered struct {
	Content   string `json:"content"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	Total     int    `json:"total_lines"`
	Next      int    `json:"next_start_line,omitempty"`
}

// window returns the lines of b from start (1-based; zero means 1), as many
// whole lines as fit in maxWindowBytes and at least one, each prefixed with
// its line number and a tab.
func window(b []byte, start int) (numbered, error) {
	if start == 0 {
		start = 1
	}
	if start < 1 {
		return numbered{}, fmt.Errorf("start_line must be 1 or more, got %d", start)
	}
	lines := strings.SplitAfter(string(b), "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	w := numbered{Total: len(lines)}
	if len(lines) == 0 {
		if start != 1 {
			return numbered{}, fmt.Errorf("start_line %d is past the end: the file is empty", start)
		}
		return w, nil
	}
	if start > len(lines) {
		return numbered{}, fmt.Errorf("start_line %d is past the end: the file has %d lines", start, len(lines))
	}
	var sb strings.Builder
	size, end := 0, start-1
	for end < len(lines) {
		line := lines[end]
		if size > 0 && size+len(line) > maxWindowBytes {
			break
		}
		size += len(line)
		end++
		fmt.Fprintf(&sb, "%6d\t%s", end, line)
		if !strings.HasSuffix(line, "\n") {
			sb.WriteString("\n")
		}
	}
	w.Content, w.StartLine, w.EndLine = sb.String(), start, end
	if end < len(lines) {
		w.Next = end + 1
	}
	return w, nil
}

// readText reads a regular UTF-8 file below root after refusing symlink
// components, explaining a missing path by its nearest existing directory.
func readText(root *os.Root, rel string, limit int) ([]byte, error) {
	if err := noSymlinks(root, rel); err != nil {
		return nil, err
	}
	info, err := root.Lstat(rel)
	if err != nil {
		return nil, absent(root, rel, err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("%s is a directory; list it instead", rel)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", rel)
	}
	if info.Size() > int64(limit) {
		return nil, fmt.Errorf("%s is %d bytes, larger than the %d byte read limit", rel, info.Size(), limit)
	}
	b, err := root.ReadFile(rel)
	if err != nil {
		return nil, err
	}
	if !utf8.Valid(b) {
		return nil, fmt.Errorf("%s is not valid UTF-8 text", rel)
	}
	return b, nil
}

// absent turns a not-exist error for rel into one that names the nearest
// existing directory above it and its entries, so that a guessed path
// costs one step and shows where to look. Any other error is returned
// unchanged.
func absent(root *os.Root, rel string, err error) error {
	if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	dir := path.Dir(rel)
	for dir != "." {
		if info, e := root.Lstat(dir); e == nil && info.IsDir() {
			break
		}
		dir = path.Dir(dir)
	}
	entries, e := listNames(root, dir)
	if e != nil {
		return fmt.Errorf("%s does not exist", rel)
	}
	label := dir
	if dir == "." {
		label = "the top-level directory"
	}
	return fmt.Errorf("%s does not exist; %s contains: %s", rel, label, entries)
}

// listNames lists a directory's entries, directories marked with a slash,
// .git left out, quoted and capped at maxListed.
func listNames(root *os.Root, dir string) (string, error) {
	if err := noSymlinks(root, dir); err != nil {
		return "", err
	}
	entries, err := fs.ReadDir(root.FS(), dir)
	if err != nil {
		return "", err
	}
	var out []string
	for _, e := range entries {
		n := e.Name()
		if n == ".git" {
			continue
		}
		if e.IsDir() {
			n += "/"
		}
		out = append(out, fmt.Sprintf("%q", n))
	}
	sort.Strings(out)
	more := ""
	if len(out) > maxListed {
		more = fmt.Sprintf(", and %d more", len(out)-maxListed)
		out = out[:maxListed]
	}
	if len(out) == 0 {
		return "(nothing)", nil
	}
	return strings.Join(out, ", ") + more, nil
}

// ---- writes ----------------------------------------------------------------

// write_file and edit_file are one write path. Each call is projected to
// the full content the file would have; that projection, and nothing the
// model says about it, is what the policy checks (every path rule, then the
// scope of the projected diff) and what the tool checks again and writes.

// Project computes the path and full content a write_file or edit_file
// call would leave, without touching anything. current returns the file's
// present content; it is consulted only by edit_file, and its errors are
// returned unchanged, so a path rule it applies is reported as such.
func Project(tool string, args json.RawMessage, current func(path string) ([]byte, error)) (string, []byte, error) {
	switch tool {
	case names.WriteFile:
		var a struct{ Path, Content string }
		if err := decode(args, &a); err != nil {
			return "", nil, err
		}
		return a.Path, []byte(a.Content), nil
	case names.EditFile:
		var a struct {
			Path    string `json:"path"`
			OldText string `json:"old_text"`
			NewText string `json:"new_text"`
		}
		if err := decode(args, &a); err != nil {
			return "", nil, err
		}
		if a.OldText == "" {
			return "", nil, errors.New("old_text is empty; to create a file use write_file")
		}
		if a.OldText == a.NewText {
			return "", nil, errors.New("new_text is the same as old_text; nothing would change")
		}
		cur, err := current(a.Path)
		if err != nil {
			return "", nil, err
		}
		content, err := replaceOnce(a.Path, string(cur), a.OldText, a.NewText)
		if err != nil {
			return "", nil, err
		}
		return a.Path, []byte(content), nil
	}
	return "", nil, fmt.Errorf("%s is not a write tool", tool)
}

// replaceOnce replaces the one occurrence of old in cur. Zero or several
// occurrences, overlapping ones counted, are refused with what was found.
func replaceOnce(p, cur, old, new string) (string, error) {
	var at []int
	for i := 0; i <= len(cur)-len(old); {
		j := strings.Index(cur[i:], old)
		if j < 0 {
			break
		}
		at = append(at, i+j)
		i += j + 1
	}
	switch len(at) {
	case 1:
		return cur[:at[0]] + new + cur[at[0]+len(old):], nil
	case 0:
		msg := fmt.Sprintf("old_text occurs 0 times in %s", p)
		if numberedLines.MatchString(old) {
			msg += "; it starts with line numbers, which read_file shows but which are not part of the file"
		} else if n := strings.Count(squash(cur), squash(old)); n > 0 && squash(old) != "" {
			msg += fmt.Sprintf("; %d place(s) match if whitespace is ignored, so copy the text exactly, including tabs and line breaks", n)
		}
		return "", errors.New(msg + "; read the file again for its current text")
	}
	lines := make([]string, 0, len(at))
	for _, i := range at {
		lines = append(lines, fmt.Sprint(1+strings.Count(cur[:i], "\n")))
	}
	return "", fmt.Errorf("old_text occurs %d times in %s (at lines %s); include enough surrounding text to match exactly one", len(at), p, strings.Join(lines, ", "))
}

// numberedLines matches text whose first line begins with a read_file line
// number prefix.
var numberedLines = regexp.MustCompile(`^ *\d+\t`)

func squash(s string) string { return strings.Join(strings.Fields(s), " ") }

// ProjectWrite applies every write rule to a write_file or edit_file call
// and returns the cleaned path and the content the file would have. The
// path rules come first, so a protected or ignored path is refused as
// such before its content is read. Policy calls it before allowing a write;
// the tool calls it again before writing.
func ProjectWrite(ctx context.Context, s *session.Session, tool string, args json.RawMessage) (string, []byte, error) {
	p, content, err := Project(tool, args, func(p string) ([]byte, error) {
		rel, err := checkPath(ctx, s, p)
		if err != nil {
			return nil, err
		}
		root, err := os.OpenRoot(s.WS.Dir)
		if err != nil {
			return nil, err
		}
		defer root.Close()
		if _, err := root.Lstat(rel); errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%s does not exist; edit_file changes an existing file, write_file creates one", rel)
		}
		return readText(root, rel, maxWriteBytes)
	})
	if err != nil {
		return "", nil, err
	}
	rel, err := CheckWritable(ctx, s, p, content)
	return rel, content, err
}

// CheckWritable applies every write rule to a path and the content it
// would hold and returns the cleaned path.
func CheckWritable(ctx context.Context, s *session.Session, p string, content []byte) (string, error) {
	rel, err := checkPath(ctx, s, p)
	if err != nil {
		return "", err
	}
	if len(content) > maxWriteBytes {
		return "", fmt.Errorf("content is %d bytes, larger than the %d byte write limit", len(content), maxWriteBytes)
	}
	if !utf8.Valid(content) {
		return "", errors.New("content is not valid UTF-8 text")
	}
	return rel, nil
}

// checkPath applies the path rules of a write: contained, not the root,
// not protected, not ignored, no symlink component, and a regular file if
// it exists.
func checkPath(ctx context.Context, s *session.Session, p string) (string, error) {
	rel, err := cleanPath(p)
	if err != nil {
		return "", err
	}
	if rel == "." {
		return "", errors.New("path is the repository root")
	}
	if s.IsProtected(rel) {
		return "", fmt.Errorf("%s is protected and cannot be written", rel)
	}
	if ignored, err := s.IsIgnored(ctx, rel); err != nil {
		return "", err
	} else if ignored {
		return "", fmt.Errorf("%s matches an ignore rule and could never enter the candidate tree", rel)
	}
	root, err := os.OpenRoot(s.WS.Dir)
	if err != nil {
		return "", err
	}
	defer root.Close()
	if err := noSymlinks(root, rel); err != nil {
		return "", err
	}
	if info, err := root.Lstat(rel); err == nil && !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s exists and is not a regular file", rel)
	}
	return rel, nil
}

// commitWrite is the execution of write_file and edit_file: project the
// call again under every rule, then replace the file atomically, keeping
// an existing file's permissions. It returns the cleaned path and the
// file's content before and after.
func commitWrite(ctx context.Context, s *session.Session, tool string, args json.RawMessage) (string, []byte, []byte, error) {
	rel, content, err := ProjectWrite(ctx, s, tool, args)
	if err != nil {
		return "", nil, nil, err
	}
	root, err := os.OpenRoot(s.WS.Dir)
	if err != nil {
		return "", nil, nil, err
	}
	defer root.Close()
	perm := os.FileMode(0o644)
	var before []byte
	if info, err := root.Lstat(rel); err == nil {
		perm = info.Mode().Perm()
		if before, err = root.ReadFile(rel); err != nil {
			return "", nil, nil, err
		}
	}
	if dir := path.Dir(rel); dir != "." {
		if err := root.MkdirAll(dir, 0o755); err != nil {
			return "", nil, nil, err
		}
	}
	tmp := rel + ".repo-steward-tmp"
	if err := root.WriteFile(tmp, content, perm); err != nil {
		return "", nil, nil, err
	}
	if err := root.Chmod(tmp, perm); err != nil {
		root.Remove(tmp)
		return "", nil, nil, err
	}
	if err := root.Rename(tmp, rel); err != nil {
		root.Remove(tmp)
		return "", nil, nil, err
	}
	faultpoint.Hit("tools.write_file.after_write")
	return rel, before, content, nil
}
