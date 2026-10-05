//go:build smoke

package smoke

import (
	"embed"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/joeylking/repo-steward/internal/deps"
	"github.com/joeylking/repo-steward/internal/gitx"
	"github.com/joeylking/repo-steward/internal/steward"
	"github.com/joeylking/repo-steward/internal/testtmp"
)

//go:embed testdata/vulnerable/*.json
var vulnerableDeclarations embed.FS

// vulnerableScenario pins a run of maintain -select vulnerable: the
// advisory that must be found at base and cleared, and the upgrade that
// must clear it.
type vulnerableScenario struct {
	Scenario
	ExpectedAdvisory string `json:"expected_advisory"`
}

// TestSmokeVulnerable runs vulnerable selection against the live Go
// vulnerability database. The scanner is built through the public module
// proxy, and the database is fetched from https://vuln.go.dev.
func TestSmokeVulnerable(t *testing.T) {
	root := testtmp.Dir(t)
	dataDir := filepath.Join(root, "data")
	clones := map[string]string{}
	author := gitx.Identity{Name: "Smoke", Email: "smoke@example.invalid", When: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	entries, err := vulnerableDeclarations.ReadDir("testdata/vulnerable")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		raw, _ := vulnerableDeclarations.ReadFile("testdata/vulnerable/" + e.Name())
		var sc vulnerableScenario
		if err := json.Unmarshal(raw, &sc); err != nil {
			t.Fatalf("%s: %v", e.Name(), err)
		}
		if sc.Name == "" || sc.Commit == "" || sc.ExpectedAdvisory == "" || sc.Dependency == "" || sc.Version == "" {
			t.Fatalf("%s: incomplete declaration %+v", e.Name(), sc)
		}
		t.Run(sc.Name, func(t *testing.T) {
			src := clone(t, root, sc.Scenario, clones)
			res, err := steward.RunBaseline(ctx, steward.Options{SourcePath: src, DataDir: dataDir, Policy: deps.DefaultPolicy(), Author: author, Select: steward.SelectVulnerable})
			if res != nil {
				b, _ := json.MarshalIndent(res, "", "  ")
				os.WriteFile(filepath.Join(root, sc.Name+".json"), b, 0o644)
			}
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			v := res.Vulnerabilities
			if v == nil || v.Base == nil || !v.Base.Conclusive {
				t.Fatalf("outcome %s: no conclusive base scan: %+v detail %v", res.Outcome, v, res.Detail)
			}
			key := sc.ExpectedAdvisory + " " + sc.Dependency
			found := false
			for _, f := range v.Base.ThirdParty {
				found = found || f.Key == key
			}
			if !found {
				t.Fatalf("%s not found at base; third-party findings %+v", key, v.Base.ThirdParty)
			}
			if res.Outcome != sc.Expected {
				t.Fatalf("outcome %s, want %s (detail %v readiness %+v)", res.Outcome, sc.Expected, res.Detail, res.Readiness)
			}
			if status, err := gitx.New(src).Run(ctx, "status", "--porcelain"); err != nil || len(status) != 0 {
				t.Fatalf("source checkout modified: %s %v", status, err)
			}
			if res.Selected == nil || res.Selected.Module != sc.Dependency || res.Selected.Version != sc.Version {
				t.Fatalf("selected %+v, want %s@%s", res.Selected, sc.Dependency, sc.Version)
			}
			ev := res.Readiness.Vulnerabilities
			if ev == nil || !contains(ev.Resolved, key) || len(ev.Introduced) != 0 || len(ev.Escalated) != 0 {
				t.Fatalf("scan evidence %+v", ev)
			}
			for _, f := range v.Post.ThirdParty {
				if f.Key == key {
					t.Fatalf("%s still reported after the upgrade: %+v", key, f)
				}
			}
			var files []string
			for _, f := range res.Proposal.Files {
				files = append(files, f.Path)
			}
			sort.Strings(files)
			if strings.Join(files, ",") != strings.Join(sc.Files, ",") {
				t.Fatalf("proposal files %v, want %v", files, sc.Files)
			}
			if !strings.Contains(res.Proposal.Body, "`"+sc.ExpectedAdvisory+"`") {
				t.Fatalf("body does not name %s:\n%s", sc.ExpectedAdvisory, res.Proposal.Body)
			}
			t.Logf("%s: %s -> %s clears %s (database %s, modified %s) in %s; proposal %s", sc.Name, sc.Dependency, sc.Version, sc.ExpectedAdvisory,
				v.Database.SnapshotID, v.Database.Modified.Format(time.RFC3339), time.Duration(res.Timings["total"])*time.Millisecond, res.Proposal.HeadCommit[:12])
		})
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
