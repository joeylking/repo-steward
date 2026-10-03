package vuln

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/mod/semver"
)

// Advisory is a vulnerability declared in Go for a synthetic database. It
// covers the subset of OSV the scanner reads: one affected module, its
// version ranges, and the vulnerable packages and symbols.
type Advisory struct {
	ID      string
	Aliases []string
	Summary string
	Details string
	// Module is the module path, or "stdlib" for the standard library.
	Module string
	// Ranges are affected intervals. Versions carry a leading "v"; the
	// generator writes them in OSV form without it.
	Ranges   []Range
	Packages []AffectedPackage
	// Published and Modified default to FixtureTime.
	Published, Modified time.Time
	// Withdrawn, when set, marks the entry withdrawn at that time.
	Withdrawn *time.Time
}

// Range is one affected interval: Introduced inclusive ("" means from the
// beginning), Fixed exclusive ("" means not fixed).
type Range struct {
	Introduced string
	Fixed      string
}

// AffectedPackage is a vulnerable package and the symbols in it. No symbols
// means every symbol in the package is vulnerable.
type AffectedPackage struct {
	Path    string
	Symbols []string
}

// FixtureTime is the modified time of every fixture advisory, and so of
// the fixture database.
var FixtureTime = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// FixtureAdvisories is the declared advisory set for the fixture modules
// served by internal/modproxy. Each one exists to make the scanner produce
// one kind of result against an existing fixture repository:
//
//	GO-TEST-0001  lib < v1.2.4, Greet         patch-safe calls lib.Greet: symbol level; gone at v1.2.4
//	GO-TEST-0002  lib >= v1.3.0, Greet, no fix breaking-minor upgraded to v1.3.0 gains it: introduced, no fix
//	GO-TEST-0003  toolkit, text/strutil, no fix moved-package requires toolkit v1.0.0, which lacks that package: module level
//	GO-TEST-0004  stdlib fmt.Sscanf           every fixture imports fmt and never calls Sscanf: package level, stdlib
//	GO-TEST-0005  util < v0.2.0, Trim         closure-regression reaches util.Trim directly and through core.Run: symbol level; the scanner reports one trace, through core.Run
//	GO-TEST-0006  lib < v1.2.4, Greet, withdrawn  must never be reported
//
// No fixture can express a third-party advisory at package level: every
// exported symbol of every fixture module is called by some fixture
// application, and the fixtures are pinned, so GO-TEST-0004 uses the
// standard library for that level instead.
func FixtureAdvisories() []Advisory {
	withdrawn := FixtureTime
	return []Advisory{
		{
			ID:       "GO-TEST-0001",
			Aliases:  []string{"CVE-0000-0001"},
			Summary:  "Greeting injection in example.com/lib",
			Details:  "Greet echoes an empty name unchanged. Fixed in v1.2.4.",
			Module:   "example.com/lib",
			Ranges:   []Range{{Fixed: "v1.2.4"}},
			Packages: []AffectedPackage{{Path: "example.com/lib", Symbols: []string{"Greet"}}},
		},
		{
			ID:       "GO-TEST-0002",
			Summary:  "Context misuse in example.com/lib v1.3",
			Details:  "Greet ignores context values. No fixed version exists.",
			Module:   "example.com/lib",
			Ranges:   []Range{{Introduced: "v1.3.0"}},
			Packages: []AffectedPackage{{Path: "example.com/lib", Symbols: []string{"Greet"}}},
		},
		{
			ID:       "GO-TEST-0003",
			Summary:  "Case folding in example.com/toolkit/text/strutil",
			Details:  "Upper mishandles special casing. No fixed version exists.",
			Module:   "example.com/toolkit",
			Ranges:   []Range{{}},
			Packages: []AffectedPackage{{Path: "example.com/toolkit/text/strutil", Symbols: []string{"Upper"}}},
		},
		{
			ID:       "GO-TEST-0004",
			Summary:  "Synthetic scanning flaw in fmt",
			Details:  "Sscanf is declared vulnerable in every Go release before 1.99.0.",
			Module:   "stdlib",
			Ranges:   []Range{{Fixed: "v1.99.0"}},
			Packages: []AffectedPackage{{Path: "fmt", Symbols: []string{"Sscanf"}}},
		},
		{
			ID:       "GO-TEST-0005",
			Summary:  "Whitespace handling in example.com/util",
			Details:  "Trim is declared vulnerable before v0.2.0.",
			Module:   "example.com/util",
			Ranges:   []Range{{Fixed: "v0.2.0"}},
			Packages: []AffectedPackage{{Path: "example.com/util", Symbols: []string{"Trim"}}},
		},
		{
			ID:        "GO-TEST-0006",
			Summary:   "Withdrawn report against example.com/lib",
			Details:   "Withdrawn; must not be reported.",
			Module:    "example.com/lib",
			Ranges:    []Range{{Fixed: "v1.2.4"}},
			Packages:  []AffectedPackage{{Path: "example.com/lib", Symbols: []string{"Greet"}}},
			Withdrawn: &withdrawn,
		},
	}
}

// osvEntry is the OSV subset written for an advisory, with fields in the
// order the public database uses.
type osvEntry struct {
	SchemaVersion    string           `json:"schema_version"`
	ID               string           `json:"id"`
	Modified         time.Time        `json:"modified"`
	Published        time.Time        `json:"published"`
	Withdrawn        *time.Time       `json:"withdrawn,omitempty"`
	Aliases          []string         `json:"aliases,omitempty"`
	Summary          string           `json:"summary,omitempty"`
	Details          string           `json:"details"`
	Affected         []osvAffected    `json:"affected"`
	DatabaseSpecific *osvDatabaseSpec `json:"database_specific,omitempty"`
}

type osvAffected struct {
	Package struct {
		Name      string `json:"name"`
		Ecosystem string `json:"ecosystem"`
	} `json:"package"`
	Ranges            []osvRange `json:"ranges,omitempty"`
	EcosystemSpecific struct {
		Imports []osvImport `json:"imports,omitempty"`
	} `json:"ecosystem_specific"`
}

type osvRange struct {
	Type   string     `json:"type"`
	Events []osvEvent `json:"events"`
}

type osvEvent struct {
	Introduced string `json:"introduced,omitempty"`
	Fixed      string `json:"fixed,omitempty"`
}

type osvImport struct {
	Path    string   `json:"path"`
	Symbols []string `json:"symbols,omitempty"`
}

type osvDatabaseSpec struct {
	URL          string `json:"url,omitempty"`
	ReviewStatus string `json:"review_status,omitempty"`
}

func osvVersion(v string) (string, error) {
	if !semver.IsValid(v) {
		return "", fmt.Errorf("invalid version %q", v)
	}
	return strings.TrimPrefix(v, "v"), nil
}

func (a Advisory) entry() (osvEntry, string, error) {
	e := osvEntry{SchemaVersion: "1.3.1", ID: a.ID, Modified: a.Modified, Published: a.Published, Withdrawn: a.Withdrawn, Aliases: a.Aliases, Summary: a.Summary, Details: a.Details}
	if e.Modified.IsZero() {
		e.Modified = FixtureTime
	}
	if e.Published.IsZero() {
		e.Published = FixtureTime
	}
	if !entryName.MatchString("ID/" + a.ID + ".json") {
		return e, "", fmt.Errorf("vuln: advisory id %q is not a valid file name", a.ID)
	}
	if a.Module == "" || len(a.Ranges) == 0 || len(a.Packages) == 0 {
		return e, "", fmt.Errorf("vuln: advisory %s needs a module, a range, and a package", a.ID)
	}
	var aff osvAffected
	aff.Package.Name = a.Module
	aff.Package.Ecosystem = "Go"
	r := osvRange{Type: "SEMVER"}
	// The index's "fixed" is the fix of the latest range, empty when the
	// latest range is open; the scanner skips an entry whose index fix is
	// at or below the module version, so it must not overstate the fix.
	indexFixed := ""
	for _, rg := range a.Ranges {
		intro := "0"
		if rg.Introduced != "" {
			v, err := osvVersion(rg.Introduced)
			if err != nil {
				return e, "", fmt.Errorf("vuln: advisory %s: %w", a.ID, err)
			}
			intro = v
		}
		r.Events = append(r.Events, osvEvent{Introduced: intro})
		indexFixed = ""
		if rg.Fixed != "" {
			v, err := osvVersion(rg.Fixed)
			if err != nil {
				return e, "", fmt.Errorf("vuln: advisory %s: %w", a.ID, err)
			}
			r.Events = append(r.Events, osvEvent{Fixed: v})
			indexFixed = v
		}
	}
	aff.Ranges = []osvRange{r}
	for _, p := range a.Packages {
		if p.Path == "" {
			return e, "", fmt.Errorf("vuln: advisory %s has a package with no path", a.ID)
		}
		aff.EcosystemSpecific.Imports = append(aff.EcosystemSpecific.Imports, osvImport{Path: p.Path, Symbols: p.Symbols})
	}
	e.Affected = []osvAffected{aff}
	e.DatabaseSpecific = &osvDatabaseSpec{ReviewStatus: "REVIEWED"}
	return e, indexFixed, nil
}

// WriteDB writes a database for advisories into dest, which must not
// exist. Files are written into a temporary sibling directory and renamed
// into place, so dest either does not exist or is complete. The output is
// a function of the advisories alone.
func WriteDB(dest string, advisories []Advisory) error {
	if _, err := os.Lstat(dest); err == nil {
		return fmt.Errorf("vuln: %s already exists", dest)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dest), ".vulndb-gen-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if err := os.Chmod(tmp, 0o755); err != nil {
		return err
	}
	for _, sub := range []string{"index", "ID"} {
		if err := os.Mkdir(filepath.Join(tmp, sub), 0o755); err != nil {
			return err
		}
	}

	type modVuln struct {
		ID       string    `json:"id"`
		Modified time.Time `json:"modified"`
		Fixed    string    `json:"fixed,omitempty"`
	}
	type vulnMeta struct {
		ID       string    `json:"id"`
		Modified time.Time `json:"modified"`
		Aliases  []string  `json:"aliases,omitempty"`
	}
	modules := map[string][]modVuln{}
	var vulns []vulnMeta
	var latest time.Time
	seen := map[string]bool{}
	for _, a := range advisories {
		if seen[a.ID] {
			return fmt.Errorf("vuln: advisory %s declared twice", a.ID)
		}
		seen[a.ID] = true
		e, fixed, err := a.entry()
		if err != nil {
			return err
		}
		if err := writeJSON(filepath.Join(tmp, "ID", a.ID+".json"), e); err != nil {
			return err
		}
		modules[a.Module] = append(modules[a.Module], modVuln{ID: e.ID, Modified: e.Modified, Fixed: fixed})
		vulns = append(vulns, vulnMeta{ID: e.ID, Modified: e.Modified, Aliases: e.Aliases})
		if e.Modified.After(latest) {
			latest = e.Modified
		}
	}
	if latest.IsZero() {
		latest = FixtureTime
	}
	type moduleMeta struct {
		Path  string    `json:"path"`
		Vulns []modVuln `json:"vulns"`
	}
	var mods []moduleMeta
	for p, vs := range modules {
		sort.Slice(vs, func(i, j int) bool { return vs[i].ID < vs[j].ID })
		mods = append(mods, moduleMeta{Path: p, Vulns: vs})
	}
	sort.Slice(mods, func(i, j int) bool { return mods[i].Path < mods[j].Path })
	sort.Slice(vulns, func(i, j int) bool { return vulns[i].ID < vulns[j].ID })
	if mods == nil {
		mods = []moduleMeta{}
	}
	if vulns == nil {
		vulns = []vulnMeta{}
	}
	if err := writeJSON(filepath.Join(tmp, "index", "modules.json"), mods); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(tmp, "index", "vulns.json"), vulns); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(tmp, "index", "db.json"), map[string]time.Time{"modified": latest.UTC()}); err != nil {
		return err
	}
	if err := CheckConsistency(tmp); err != nil {
		return err
	}
	return os.Rename(tmp, dest)
}

// WriteFixtureDB writes the fixture advisory database into dest.
func WriteFixtureDB(dest string) error { return WriteDB(dest, FixtureAdvisories()) }

func writeJSON(p string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return os.WriteFile(p, raw, 0o644)
}
