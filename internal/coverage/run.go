package coverage

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/joeylking/repo-steward/internal/sandbox"
)

// OutputCap bounds the profile read from the container. A profile that
// reaches it is truncated and therefore no evidence.
const OutputCap = 64 << 20

// Script runs the repository's tests with coverage of every package of the
// main module, so that a test in one package counts for code it reaches in
// another, and writes the profile, and nothing else, to standard output.
// The test output goes to standard error. The profile is written inside
// the container's own /tmp and read back through standard output, so no
// file a container wrote is ever read from the host. Duplicate entries
// from different test binaries are collapsed with sort -u, which with
// -covermode=set leaves at most an executed and an unexecuted entry per
// block. The last line counts the profile's lines, so a cut is detected.
const Script = `go test -count=1 -covermode=set -coverpkg=./... -coverprofile=/tmp/repo-steward-cover.out ./... 1>&2; s=$?; ` +
	`if [ "$s" -ne 0 ]; then exit "$s"; fi; ` +
	`[ -f /tmp/repo-steward-cover.out ] || { echo "repo-steward: go test wrote no coverage profile" >&2; exit 97; }; ` +
	`{ head -n 1 /tmp/repo-steward-cover.out && tail -n +2 /tmp/repo-steward-cover.out | LC_ALL=C sort -u; } > /tmp/repo-steward-cover.sorted || exit 98; ` +
	`cat /tmp/repo-steward-cover.sorted && printf '` + EndMarker + `%s\n' "$(wc -l < /tmp/repo-steward-cover.sorted)"`

// Evidence is one coverage run, stored with the validation it belongs to.
// The profile is kept as the container wrote it and parsed only when it is
// judged, so the parsing rules apply to the stored bytes.
type Evidence struct {
	ExitCode        int           `json:"exit_code"`
	TimedOut        bool          `json:"timed_out,omitempty"`
	StdoutTruncated bool          `json:"stdout_truncated,omitempty"`
	Profile         string        `json:"profile"`
	Stderr          string        `json:"stderr,omitempty"`
	Duration        time.Duration `json:"duration_ns"`
}

// Usable returns the parsed profile, or why the run is no evidence.
func (e *Evidence) Usable() (*Profile, string) {
	switch {
	case e == nil:
		return nil, "no coverage run for this tree"
	case e.TimedOut:
		return nil, "the coverage run timed out"
	case e.StdoutTruncated:
		return nil, "the coverage profile exceeded the capture limit"
	case e.ExitCode != 0:
		return nil, "the coverage run exited " + strconv.Itoa(e.ExitCode) + ": " + firstLine(e.Stderr)
	}
	p, err := ParseProfile([]byte(e.Profile))
	if err != nil {
		return nil, "the coverage profile is unusable: " + err.Error()
	}
	return p, ""
}

// Run executes Script against the snapshot mounted in sb, which must be an
// execute-profile sandbox with a build cache of its own.
func Run(ctx context.Context, sb sandbox.Sandbox, timeout time.Duration, runID, stepID string) (*Evidence, error) {
	res, err := sb.Run(ctx, sandbox.ExecSpec{Profile: sandbox.Execute, Argv: []string{"sh", "-c", Script}, Timeout: timeout, OutputCap: OutputCap, RunID: runID, StepID: stepID})
	if err != nil {
		return nil, err
	}
	stderr := string(res.Stderr)
	if len(stderr) > 8000 {
		stderr = stderr[len(stderr)-8000:]
	}
	return &Evidence{ExitCode: res.ExitCode, TimedOut: res.TimedOut, StdoutTruncated: res.StdoutTruncated, Profile: string(res.Stdout), Stderr: stderr, Duration: res.Duration}, nil
}

func firstLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			if len(l) > 200 {
				l = l[:200]
			}
			return l
		}
	}
	return ""
}
