package smoke

import (
	"context"
	"embed"
	"encoding/json"
	"io/fs"
	"path"
	"strings"
	"testing"

	"github.com/joeylking/repo-steward/internal/scenario"
)

// This file builds without the smoke tag: the declarations and the repair
// oracles are checked on every test run, with no network and no model.

// Scenario pins one upgrade on one public repository.
type Scenario struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Repository  string `json:"repository"`
	Tag         string `json:"tag"`
	Commit      string `json:"commit"`
	Dependency  string `json:"dependency"`
	Version     string `json:"version"`
	Expected    string `json:"expected"`
	// ExpectedFinding names a baseline finding that must be present when
	// the expected outcome is baseline_failing.
	ExpectedFinding string   `json:"expected_finding,omitempty"`
	Files           []string `json:"files,omitempty"`
	// The fields below are used only by the repair scenarios under
	// testdata/repair, which a model runs (see repair_test.go).
	//
	// AllowedFiles bounds a model's proposal; RequiredFiles must all be in
	// it. Oracles are checked against the proposal tree and never shown to
	// the model. MaxModelCalls caps the run (default: the model mode's).
	AllowedFiles  []string          `json:"allowed_files,omitempty"`
	RequiredFiles []string          `json:"required_files,omitempty"`
	Oracles       []scenario.Oracle `json:"oracles,omitempty"`
	MaxModelCalls int               `json:"max_model_calls,omitempty"`
	// RepairOutcome is what a correct repair ends as under the default
	// configuration: proposal_prepared when the repository's tests execute
	// the lines a repair must change, repair_not_exercised when they do
	// not. A proposal from a scenario that declares repair_not_exercised
	// is a defect.
	RepairOutcome string `json:"repair_outcome,omitempty"`
	// RepairEvidence says how RepairOutcome was established.
	RepairEvidence string `json:"repair_evidence,omitempty"`
}

var ctx = context.Background()

//go:embed testdata/repair/*.json
var repairDeclarations embed.FS

//go:embed all:testdata/repair-oracles
var oracleTrees embed.FS

func loadFrom(t *testing.T, fsys embed.FS, dir string) []Scenario {
	t.Helper()
	entries, err := fsys.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []Scenario
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		b, _ := fsys.ReadFile(dir + "/" + e.Name())
		var sc Scenario
		if err := json.Unmarshal(b, &sc); err != nil {
			t.Fatalf("%s: %v", e.Name(), err)
		}
		if sc.Name == "" || sc.Commit == "" || sc.Dependency == "" || sc.Version == "" || sc.Expected == "" {
			t.Fatalf("%s: incomplete declaration %+v", e.Name(), sc)
		}
		out = append(out, sc)
	}
	return out
}

// checkOracles applies every oracle whose file read returns content and
// returns one line per failure; a missing file is a failure.
func checkOracles(oracles []scenario.Oracle, read func(file string) ([]byte, error)) []string {
	var out []string
	for _, o := range oracles {
		content, err := read(o.File)
		if err != nil {
			out = append(out, "oracle "+o.File+": file missing from the tree")
			continue
		}
		for _, f := range o.Check(content) {
			out = append(out, "oracle "+o.File+": "+f)
		}
	}
	return out
}

// The repair oracles are proven against hand-made trees under
// testdata/repair-oracles/<scenario>/{right,wrong}/<case>/: every right
// tree passes every oracle whose file it holds, and every wrong tree fails
// at least one. The trees are excerpts written for the purpose, the
// functions the oracles read, not copies of the repositories. The
// mcp-openweather wrong cases include the repair a model made on
// 2026-10-04 (an unchecked .(map[string]any)), which the earlier oracle
// passed.
func TestRepairOracles(t *testing.T) {
	for _, sc := range loadFrom(t, repairDeclarations, "testdata/repair") {
		t.Run(sc.Name, func(t *testing.T) {
			if sc.RepairOutcome != "proposal_prepared" && sc.RepairOutcome != "repair_not_exercised" {
				t.Fatalf("repair_outcome %q", sc.RepairOutcome)
			}
			counts := map[string]int{}
			for _, kind := range []string{"right", "wrong"} {
				root := path.Join("testdata/repair-oracles", sc.Name, kind)
				cases, err := fs.ReadDir(oracleTrees, root)
				if err != nil {
					t.Fatal(err)
				}
				for _, c := range cases {
					dir := path.Join(root, c.Name())
					var present []scenario.Oracle
					for _, o := range sc.Oracles {
						if _, err := fs.Stat(oracleTrees, path.Join(dir, o.File)); err == nil {
							present = append(present, o)
						}
					}
					if len(present) == 0 {
						t.Fatalf("%s/%s holds no file an oracle reads", kind, c.Name())
					}
					fails := checkOracles(present, func(file string) ([]byte, error) { return fs.ReadFile(oracleTrees, path.Join(dir, file)) })
					switch {
					case kind == "right" && len(fails) > 0:
						t.Errorf("right/%s fails: %s", c.Name(), strings.Join(fails, "; "))
					case kind == "wrong" && len(fails) == 0:
						t.Errorf("wrong/%s passes every oracle", c.Name())
					}
					counts[kind]++
				}
			}
			if counts["right"] == 0 || counts["wrong"] == 0 {
				t.Fatalf("cases %v: need at least one right and one wrong tree", counts)
			}
		})
	}
}
