package deps

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"strings"

	"golang.org/x/mod/semver"

	"github.com/joeylking/repo-steward/internal/sandbox"
	"github.com/joeylking/repo-steward/internal/vuln"
)

// ModuleFacts are what target computation needs to know about one module
// with findings: its version in the build list, whether the main module's
// go.mod requires it directly, and its published versions.
type ModuleFacts struct {
	Current  string
	Direct   bool
	Versions []string
}

// FindingNote explains one finding that the selected upgrade does not clear.
type FindingNote struct {
	ID     string `json:"id"`
	Module string `json:"module"`
	Found  string `json:"found_version"`
	Fixed  string `json:"fixed_version,omitempty"`
	Reason string `json:"reason"`
}

// VulnTarget is the upgrade that clears every fixable finding of one module.
type VulnTarget struct {
	Module  string `json:"module"`
	Current string `json:"current"`
	Direct  bool   `json:"direct"`
	// Needed is the highest fixed version among the module's findings that
	// have one; Version is the lowest eligible published version at or
	// above it.
	Needed  string `json:"needed"`
	Version string `json:"version"`
	Delta   string `json:"delta"`
	// Level is the highest reachability level among the fixable findings.
	Level vuln.Level `json:"level"`
	// Fixes are the keys ("<id> <module>") of the findings the upgrade clears.
	Fixes []string `json:"fixes"`
	// Unfixed are findings of the same module with no published fix; the
	// upgrade leaves them in place.
	Unfixed []FindingNote `json:"unfixed,omitempty"`
}

// VulnPlan is the result of target computation.
type VulnPlan struct {
	// Targets are the eligible upgrades in selection order: highest level
	// first, then direct before indirect, then module path. One run applies
	// only the first.
	Targets []VulnTarget `json:"targets"`
	// NotFixable explains every third-party finding in a module that has no
	// eligible target.
	NotFixable []FindingNote `json:"not_fixable,omitempty"`
}

// Selected returns the upgrade a run applies.
func (p VulnPlan) Selected() (VulnTarget, bool) {
	if len(p.Targets) == 0 {
		return VulnTarget{}, false
	}
	return p.Targets[0], true
}

// Reasons a finding is not fixed.
const (
	ReasonNoFix        = "no fixed version is published"
	ReasonNoFacts      = "the module's versions could not be listed"
	ReasonNotPublished = "the fixed version %s is not in the module's published version list"
)

// PlanVulnFixes computes, for every third-party module with findings, the
// lowest published version that clears all of its fixable findings and is
// eligible under pol. Standard-library findings are ignored: repo-steward
// never changes the go directive. It is a pure function of its inputs.
func PlanVulnFixes(findings []vuln.Finding, facts map[string]ModuleFacts, pol Policy) VulnPlan {
	byModule := map[string][]vuln.Finding{}
	var mods []string
	for _, f := range findings {
		if f.Stdlib {
			continue
		}
		if _, ok := byModule[f.Module]; !ok {
			mods = append(mods, f.Module)
		}
		byModule[f.Module] = append(byModule[f.Module], f)
	}
	sort.Strings(mods)
	plan := VulnPlan{Targets: []VulnTarget{}}
	note := func(f vuln.Finding, reason string) FindingNote {
		return FindingNote{ID: f.OSV, Module: f.Module, Found: f.FoundVersion, Fixed: f.FixedVersion, Reason: reason}
	}
	for _, m := range mods {
		fs := byModule[m]
		sort.Slice(fs, func(i, j int) bool { return fs[i].Key < fs[j].Key })
		var fixable []vuln.Finding
		var unfixed []FindingNote
		needed := ""
		for _, f := range fs {
			if f.FixedVersion == "" {
				unfixed = append(unfixed, note(f, ReasonNoFix))
				continue
			}
			fixable = append(fixable, f)
			if needed == "" || semver.Compare(f.FixedVersion, needed) > 0 {
				needed = f.FixedVersion
			}
		}
		if len(fixable) == 0 {
			plan.NotFixable = append(plan.NotFixable, unfixed...)
			continue
		}
		mf, ok := facts[m]
		version, reason := "", ReasonNoFacts
		if ok {
			version, reason = lowestFixing(m, mf, needed, pol)
		}
		if version == "" {
			for _, f := range fixable {
				plan.NotFixable = append(plan.NotFixable, note(f, reason))
			}
			plan.NotFixable = append(plan.NotFixable, unfixed...)
			continue
		}
		t := VulnTarget{Module: m, Current: mf.Current, Direct: mf.Direct, Needed: needed, Version: version, Delta: delta(mf.Current, version), Unfixed: unfixed}
		for _, f := range fixable {
			t.Fixes = append(t.Fixes, f.Key)
			if f.Level.Rank() > t.Level.Rank() {
				t.Level = f.Level
			}
		}
		plan.Targets = append(plan.Targets, t)
	}
	sort.SliceStable(plan.Targets, func(i, j int) bool {
		a, b := plan.Targets[i], plan.Targets[j]
		if a.Level.Rank() != b.Level.Rank() {
			return a.Level.Rank() > b.Level.Rank()
		}
		if a.Direct != b.Direct {
			return a.Direct
		}
		return a.Module < b.Module
	})
	sort.SliceStable(plan.NotFixable, func(i, j int) bool {
		a, b := plan.NotFixable[i], plan.NotFixable[j]
		if a.Module != b.Module {
			return a.Module < b.Module
		}
		return a.ID < b.ID
	})
	return plan
}

// lowestFixing returns the lowest eligible version at or above needed, or
// an empty version and why there is none.
func lowestFixing(mod string, mf ModuleFacts, needed string, pol Policy) (string, string) {
	if named, pinned := splitPin(pol.NamedDependency); named != "" {
		if named != mod {
			return "", fmt.Sprintf("operator named a different dependency (%s)", named)
		}
		if pinned != "" && semver.Compare(pinned, needed) < 0 {
			return "", fmt.Sprintf("operator pinned version %s, which does not clear the findings (they need %s or later)", pinned, needed)
		}
	}
	var fixing []Target
	for _, t := range targets(mod, mf.Current, mf.Versions, pol) {
		if semver.Compare(t.Version, needed) < 0 {
			continue
		}
		if t.Eligible {
			return t.Version, ""
		}
		fixing = append(fixing, t)
	}
	if len(fixing) == 0 {
		return "", fmt.Sprintf(ReasonNotPublished, needed)
	}
	r := fmt.Sprintf("the lowest fixing version %s is ineligible: %s", fixing[0].Version, strings.Join(fixing[0].Reasons, "; "))
	if len(fixing) > 1 {
		r += fmt.Sprintf("; none of the %d later versions is eligible either", len(fixing)-1)
	}
	return "", r
}

// PublishedVersions lists the published versions of each module with one
// toolchain call in the acquire profile. A module the toolchain reports an
// error for is left out, so target computation explains it as unlisted.
func PublishedVersions(ctx context.Context, sb sandbox.Sandbox, mods []string) (map[string][]string, error) {
	out := map[string][]string{}
	if len(mods) == 0 {
		return out, nil
	}
	argv := append([]string{"go", "list", "-e", "-m", "-versions", "-json"}, mods...)
	res, err := acquire(ctx, sb, argv...)
	if err != nil {
		return nil, err
	}
	infos, err := decodeModules(bytes.NewReader(res.Stdout))
	if err != nil {
		return nil, err
	}
	for _, m := range infos {
		if m.Error != nil {
			continue
		}
		out[m.Path] = append([]string{}, m.Versions...)
	}
	return out, nil
}
