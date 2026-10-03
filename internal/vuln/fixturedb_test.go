package vuln

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/joeylking/repo-steward/internal/fixture"
)

// fixtureDBHash pins the fixture database. A change to FixtureAdvisories or
// to the generator changes it, and the golden scans in testdata were made
// against this exact database (their config message carries its modified
// time; this hash is what scan evidence would bind to).
const fixtureDBHash = "sha256:c9e6e3d76931886694d5e6832bc3066576b02a7eb3cea8c7a0486aad4dcd1d41"

func TestWriteFixtureDB_DeterministicAndPinned(t *testing.T) {
	a := filepath.Join(t.TempDir(), "a")
	b := filepath.Join(t.TempDir(), "b")
	if err := WriteFixtureDB(a); err != nil {
		t.Fatal(err)
	}
	if err := WriteFixtureDB(b); err != nil {
		t.Fatal(err)
	}
	ida, err := Identify(a, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	idb, err := Identify(b, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if ida != idb {
		t.Fatalf("two writes differ: %+v vs %+v", ida, idb)
	}
	if ida.Hash != fixtureDBHash {
		t.Fatalf("fixture database hash %s, pinned %s", ida.Hash, fixtureDBHash)
	}
	if !ida.Modified.Equal(FixtureTime) || ida.Files != 3+len(FixtureAdvisories()) {
		t.Fatalf("identity %+v", ida)
	}
	if err := Verify(a, ida, Limits{}); err != nil {
		t.Fatal(err)
	}
	// Optional: materialize the database for manual scanner runs.
	if out := os.Getenv("VULN_WRITE_FIXTURE_DB"); out != "" {
		if err := WriteFixtureDB(out); err != nil {
			t.Fatal(err)
		}
		t.Logf("fixture database written to %s (%s)", out, ida.Hash)
	}
}

func TestWriteDB_RefusesExistingDestination(t *testing.T) {
	dir := t.TempDir()
	if err := WriteFixtureDB(dir); err == nil {
		t.Fatal("wrote over an existing directory")
	}
}

func TestWriteDB_IndexFixedFollowsLatestRange(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "db")
	advs := []Advisory{
		{ID: "GO-X-1", Module: "example.com/m", Ranges: []Range{{Fixed: "v1.0.0"}, {Introduced: "v1.1.0"}}, Packages: []AffectedPackage{{Path: "example.com/m"}}},
		{ID: "GO-X-2", Module: "example.com/m", Ranges: []Range{{Fixed: "v1.0.0"}, {Introduced: "v1.1.0", Fixed: "v1.1.5"}}, Packages: []AffectedPackage{{Path: "example.com/m"}}},
	}
	if err := WriteDB(dest, advs); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dest, "index", "modules.json"))
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"path":"example.com/m","vulns":[{"id":"GO-X-1","modified":"2026-01-01T00:00:00Z"},{"id":"GO-X-2","modified":"2026-01-01T00:00:00Z","fixed":"1.1.5"}]}]`
	if string(raw) != want {
		t.Fatalf("modules.json\n got %s\nwant %s", raw, want)
	}
	entry, err := os.ReadFile(filepath.Join(dest, "ID", "GO-X-2.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(entry), `"events":[{"introduced":"0"},{"fixed":"1.0.0"},{"introduced":"1.1.0"},{"fixed":"1.1.5"}]`) {
		t.Fatalf("events not in OSV form: %s", entry)
	}
}

func TestWriteDB_Refusals(t *testing.T) {
	cases := map[string][]Advisory{
		"bad id":       {{ID: "../x", Module: "m", Ranges: []Range{{}}, Packages: []AffectedPackage{{Path: "m"}}}},
		"no range":     {{ID: "GO-X-1", Module: "m", Packages: []AffectedPackage{{Path: "m"}}}},
		"bad version":  {{ID: "GO-X-1", Module: "m", Ranges: []Range{{Fixed: "1.2.3"}}, Packages: []AffectedPackage{{Path: "m"}}}},
		"duplicate id": {{ID: "GO-X-1", Module: "m", Ranges: []Range{{}}, Packages: []AffectedPackage{{Path: "m"}}}, {ID: "GO-X-1", Module: "m", Ranges: []Range{{}}, Packages: []AffectedPackage{{Path: "m"}}}},
	}
	for name, advs := range cases {
		t.Run(name, func(t *testing.T) {
			dest := filepath.Join(t.TempDir(), "db")
			if err := WriteDB(dest, advs); err == nil {
				t.Fatal("accepted")
			}
			if _, err := os.Stat(dest); err == nil {
				t.Fatal("a failed write left the destination behind")
			}
		})
	}
}

// TestFixtureAdvisories_MatchFixtureModules keeps the advisory set honest
// about the fixtures it describes: every module, package, and symbol it
// names exists in some fixture module version, and every version bound is
// a version the fixture proxy serves.
func TestFixtureAdvisories_MatchFixtureModules(t *testing.T) {
	sources, err := fixture.ModuleSources()
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range FixtureAdvisories() {
		if a.Module == "stdlib" {
			continue
		}
		versions, ok := sources[a.Module]
		if !ok {
			t.Errorf("%s: module %s is not a fixture module", a.ID, a.Module)
			continue
		}
		for _, r := range a.Ranges {
			for _, v := range []string{r.Introduced, r.Fixed} {
				if _, ok := versions[v]; v != "" && !ok {
					t.Errorf("%s: version %s of %s is not served", a.ID, v, a.Module)
				}
			}
		}
		for _, p := range a.Packages {
			dir := strings.TrimPrefix(strings.TrimPrefix(p.Path, a.Module), "/")
			for _, sym := range p.Symbols {
				found := false
				for _, files := range versions {
					for name, content := range files {
						if filepath.ToSlash(filepath.Dir(name)) == dirOrDot(dir) && strings.Contains(string(content), "func "+sym+"(") {
							found = true
						}
					}
				}
				if !found {
					t.Errorf("%s: %s.%s exists in no version of %s", a.ID, p.Path, sym, a.Module)
				}
			}
		}
	}
}

func dirOrDot(d string) string {
	if d == "" {
		return "."
	}
	return d
}

func TestFixtureDB_EntriesAreOSV(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "db")
	if err := WriteFixtureDB(dest); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dest, "ID", "GO-TEST-0001.json"))
	if err != nil {
		t.Fatal(err)
	}
	var e map[string]any
	if err := json.Unmarshal(raw, &e); err != nil {
		t.Fatal(err)
	}
	aff := e["affected"].([]any)[0].(map[string]any)
	if aff["package"].(map[string]any)["name"] != "example.com/lib" {
		t.Fatalf("affected package %v", aff["package"])
	}
}
