//go:build integration

package steward_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/joeylking/repo-steward/internal/deps"
	"github.com/joeylking/repo-steward/internal/fixture"
	"github.com/joeylking/repo-steward/internal/modproxy"
	"github.com/joeylking/repo-steward/internal/steward"
	"github.com/joeylking/repo-steward/internal/testtmp"
)

// Model mode is exercised from recordings of a local model's responses.
// The recordings are keyed by the exact request, so any change to the
// system prompt, the tool specs, or the tool result shapes invalidates
// them; re-record with maintain -mode model -record. The model server is
// pointed at a closed port to prove nothing reaches it.
func TestModelMode_ReplaysRecordedRuns(t *testing.T) {
	t.Setenv("OLLAMA_HOST", "127.0.0.1:1")
	recordings, err := filepath.Abs("testdata/recordings/qwen3-30b-a3b")
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{"patch-safe": "go.mod,go.sum", "breaking-minor": "go.mod,go.sum,main.go"}
	for fx, wantFiles := range cases {
		t.Run(fx, func(t *testing.T) {
			root := testtmp.Dir(t)
			r, err := fixture.Setup(ctx, fx, filepath.Join(root, "repo"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := modproxy.Build(filepath.Join(root, "proxy")); err != nil {
				t.Fatal(err)
			}
			res, err := steward.RunModel(ctx, steward.Options{SourcePath: r.Path, DataDir: filepath.Join(root, "data"), FixtureProxyDir: filepath.Join(root, "proxy"), Policy: deps.DefaultPolicy(), Author: author},
				steward.ModelSpec{Provider: "ollama", Name: "qwen3:30b-a3b", ReplayDir: filepath.Join(recordings, fx)})
			if err != nil {
				t.Fatalf("%v (run %+v)", err, res.Run)
			}
			if res.Outcome != steward.OutcomeProposalPrepared {
				t.Fatalf("outcome %s run %+v detail %v", res.Outcome, res.Run, res.Detail)
			}
			var paths []string
			for _, f := range res.Proposal.Files {
				paths = append(paths, f.Path)
			}
			if got := strings.Join(paths, ","); got != wantFiles {
				t.Fatalf("files = %s, want %s", got, wantFiles)
			}
			if res.Run.Status != "COMPLETED" || res.Run.Steps == 0 {
				t.Fatalf("run = %+v", res.Run)
			}
			verifyProposal(t, res, filepath.Join(root, "data"), wantFiles)
		})
	}
	if entries, _ := os.ReadDir(recordings); len(entries) < 2 {
		t.Fatal("recordings missing")
	}
}
