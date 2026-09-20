// Package deps discovers dependency upgrade candidates. Every fact comes
// from the Go toolchain running in the acquire profile; eligibility comes
// from operator policy. The model, when it arrives, reasons over these facts
// and cannot change them.
package deps

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"

	"github.com/joeylking/repo-steward/internal/sandbox"
)

// Policy is the operator's eligibility configuration.
type Policy struct {
	AllowMajor bool     `json:"allow_major"`
	AllowMinor bool     `json:"allow_minor"`
	AllowPatch bool     `json:"allow_patch"`
	Deny       []string `json:"deny,omitempty"`
	// NamedDependency restricts eligibility to one module.
	NamedDependency string `json:"named_dependency,omitempty"`
}

// DefaultPolicy allows minor and patch upgrades.
func DefaultPolicy() Policy { return Policy{AllowMinor: true, AllowPatch: true} }

// Target is one version a module could move to.
type Target struct {
	Version  string   `json:"version"`
	Delta    string   `json:"delta"` // major minor patch
	Eligible bool     `json:"eligible"`
	Reasons  []string `json:"reasons,omitempty"`
}

// Candidate is a direct dependency with at least one newer version.
type Candidate struct {
	Module     string   `json:"module"`
	Current    string   `json:"current"`
	Latest     string   `json:"latest"`
	Delta      string   `json:"delta"`
	Targets    []Target `json:"targets"`
	Retracted  bool     `json:"retracted"`
	Deprecated string   `json:"deprecated,omitempty"`
}

// EligibleTargets returns the eligible targets in ascending order.
func (c Candidate) EligibleTargets() []Target {
	var out []Target
	for _, t := range c.Targets {
		if t.Eligible {
			out = append(out, t)
		}
	}
	return out
}

// moduleInfo mirrors the fields of go list -m -json that discovery uses.
type moduleInfo struct {
	Path       string   `json:"Path"`
	Version    string   `json:"Version"`
	Main       bool     `json:"Main"`
	Indirect   bool     `json:"Indirect"`
	Retracted  []string `json:"Retracted"`
	Deprecated string   `json:"Deprecated"`
	Versions   []string `json:"Versions"`
	Update     *struct {
		Version string `json:"Version"`
	} `json:"Update"`
	Error *struct {
		Err string `json:"Err"`
	} `json:"Error"`
}

// Timeout bounds each toolchain invocation.
const Timeout = 5 * time.Minute

func acquire(ctx context.Context, sb sandbox.Sandbox, argv ...string) (sandbox.ExecResult, error) {
	res, err := sb.Run(ctx, sandbox.ExecSpec{Profile: sandbox.Acquire, Argv: argv, Timeout: Timeout, StepID: "deps"})
	if err != nil {
		return res, err
	}
	if res.TimedOut {
		return res, fmt.Errorf("deps: %s timed out", strings.Join(argv, " "))
	}
	if res.ExitCode != 0 {
		return res, fmt.Errorf("deps: %s: exit %d: %s", strings.Join(argv, " "), res.ExitCode, strings.TrimSpace(string(res.Stderr)))
	}
	if res.StdoutTruncated {
		return res, fmt.Errorf("deps: %s: output truncated", strings.Join(argv, " "))
	}
	return res, nil
}

// Download populates the module cache with everything the main module
// needs to build, so later execute-profile commands run offline.
func Download(ctx context.Context, sb sandbox.Sandbox) error {
	_, err := acquire(ctx, sb, "go", "mod", "download")
	return err
}

func decodeModules(r io.Reader) ([]moduleInfo, error) {
	dec := json.NewDecoder(r)
	var out []moduleInfo
	for {
		var m moduleInfo
		if err := dec.Decode(&m); err == io.EOF {
			return out, nil
		} else if err != nil {
			return nil, fmt.Errorf("deps: parse module list: %w", err)
		}
		out = append(out, m)
	}
}

// Discover lists direct dependencies that have a newer version and their
// eligible targets. Versions come from the module's version list rather
// than from the toolchain's upgrade suggestion, because the suggestion
// omits +incompatible majors for modules that have a go.mod; a benchmark
// scenario depends on seeing those.
func Discover(ctx context.Context, sb sandbox.Sandbox, pol Policy) ([]Candidate, error) {
	res, err := acquire(ctx, sb, "go", "list", "-m", "-u", "-json", "all")
	if err != nil {
		return nil, err
	}
	mods, err := decodeModules(bytes.NewReader(res.Stdout))
	if err != nil {
		return nil, err
	}
	var out []Candidate
	for _, m := range mods {
		if m.Main || m.Indirect || m.Error != nil || m.Version == "" {
			continue
		}
		vres, err := acquire(ctx, sb, "go", "list", "-m", "-versions", "-json", m.Path)
		if err != nil {
			return nil, err
		}
		vinfo, err := decodeModules(bytes.NewReader(vres.Stdout))
		if err != nil {
			return nil, err
		}
		versions := []string{}
		if len(vinfo) == 1 {
			versions = vinfo[0].Versions
		}
		targets := targets(m.Path, m.Version, versions, pol)
		if len(targets) == 0 {
			continue
		}
		latest := targets[len(targets)-1].Version
		if m.Update != nil && semver.Compare(m.Update.Version, latest) > 0 {
			latest = m.Update.Version
		}
		out = append(out, Candidate{Module: m.Path, Current: m.Version, Latest: latest, Delta: delta(m.Version, latest), Targets: targets, Retracted: len(m.Retracted) > 0, Deprecated: m.Deprecated})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Module < out[j].Module })
	return out, nil
}

// delta classifies the change from cur to next.
func delta(cur, next string) string {
	switch {
	case semver.Major(cur) != semver.Major(next):
		return "major"
	case semver.MajorMinor(cur) != semver.MajorMinor(next):
		return "minor"
	default:
		return "patch"
	}
}

// targets evaluates every listed version newer than cur against the policy.
func targets(modPath, cur string, versions []string, pol Policy) []Target {
	sorted := append([]string(nil), versions...)
	semver.Sort(sorted)
	var out []Target
	for _, v := range sorted {
		if !semver.IsValid(v) || semver.Compare(v, cur) <= 0 {
			continue
		}
		t := Target{Version: v, Delta: delta(cur, v), Eligible: true}
		deny := func(reason string) {
			t.Eligible = false
			t.Reasons = append(t.Reasons, reason)
		}
		if semver.Prerelease(v) != "" {
			deny("pre-release versions are ineligible")
		}
		if module.IsPseudoVersion(v) {
			deny("pseudo-versions are ineligible")
		}
		switch t.Delta {
		case "major":
			if !pol.AllowMajor {
				deny("major upgrades are not allowed by policy")
			}
		case "minor":
			if !pol.AllowMinor {
				deny("minor upgrades are not allowed by policy")
			}
		case "patch":
			if !pol.AllowPatch {
				deny("patch upgrades are not allowed by policy")
			}
		}
		for _, d := range pol.Deny {
			if d == modPath {
				deny("module is on the operator deny list")
			}
		}
		if pol.NamedDependency != "" && pol.NamedDependency != modPath {
			deny("operator named a different dependency")
		}
		out = append(out, t)
	}
	return out
}
