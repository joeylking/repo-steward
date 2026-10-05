package deps

import (
	"context"
	"strings"
	"testing"

	"github.com/joeylking/repo-steward/internal/vuln"
)

func finding(id, mod, found, fixed string, level vuln.Level) vuln.Finding {
	return vuln.Finding{Key: id + " " + mod, OSV: id, Module: mod, Stdlib: mod == vuln.StdlibModule, FoundVersion: found, FixedVersion: fixed, Level: level}
}

func TestPlanVulnFixes(t *testing.T) {
	lib := ModuleFacts{Current: "v1.2.1", Direct: true, Versions: []string{"v1.2.1", "v1.2.4", "v1.2.5", "v1.3.0", "v2.0.0+incompatible"}}
	cases := []struct {
		name     string
		findings []vuln.Finding
		facts    map[string]ModuleFacts
		pol      Policy
		// want is "module@version" for each target in order.
		want []string
		// notFixable maps "<id> <module>" to a fragment of its reason.
		notFixable map[string]string
		unfixed    []string
	}{
		{
			name:     "lowest fixing version, not the latest",
			findings: []vuln.Finding{finding("GO-1", "example.com/lib", "v1.2.1", "v1.2.4", vuln.LevelSymbol)},
			facts:    map[string]ModuleFacts{"example.com/lib": lib},
			pol:      DefaultPolicy(),
			want:     []string{"example.com/lib@v1.2.4"},
		},
		{
			name: "several findings: the highest fix wins",
			findings: []vuln.Finding{
				finding("GO-1", "example.com/lib", "v1.2.1", "v1.2.4", vuln.LevelSymbol),
				finding("GO-2", "example.com/lib", "v1.2.1", "v1.2.5", vuln.LevelModule),
			},
			facts: map[string]ModuleFacts{"example.com/lib": lib},
			pol:   DefaultPolicy(),
			want:  []string{"example.com/lib@v1.2.5"},
		},
		{
			name: "a finding with no fix leaves the module targetable for the others",
			findings: []vuln.Finding{
				finding("GO-1", "example.com/lib", "v1.2.1", "v1.2.4", vuln.LevelSymbol),
				finding("GO-3", "example.com/lib", "v1.2.1", "", vuln.LevelSymbol),
			},
			facts:   map[string]ModuleFacts{"example.com/lib": lib},
			pol:     DefaultPolicy(),
			want:    []string{"example.com/lib@v1.2.4"},
			unfixed: []string{"GO-3"},
		},
		{
			name:       "no fix at all",
			findings:   []vuln.Finding{finding("GO-3", "example.com/lib", "v1.2.1", "", vuln.LevelSymbol)},
			facts:      map[string]ModuleFacts{"example.com/lib": lib},
			pol:        DefaultPolicy(),
			notFixable: map[string]string{"GO-3 example.com/lib": "no fixed version is published"},
		},
		{
			name:       "fix only in a major the policy forbids",
			findings:   []vuln.Finding{finding("GO-4", "example.com/lib", "v1.2.1", "v2.0.0+incompatible", vuln.LevelPackage)},
			facts:      map[string]ModuleFacts{"example.com/lib": lib},
			pol:        DefaultPolicy(),
			notFixable: map[string]string{"GO-4 example.com/lib": "major upgrades are not allowed by policy"},
		},
		{
			name:     "the same major fix with majors allowed",
			findings: []vuln.Finding{finding("GO-4", "example.com/lib", "v1.2.1", "v2.0.0+incompatible", vuln.LevelPackage)},
			facts:    map[string]ModuleFacts{"example.com/lib": lib},
			pol:      Policy{AllowMajor: true, AllowMinor: true, AllowPatch: true},
			want:     []string{"example.com/lib@v2.0.0+incompatible"},
		},
		{
			name:       "fix in a minor under a patch-only policy",
			findings:   []vuln.Finding{finding("GO-5", "example.com/lib", "v1.2.1", "v1.3.0", vuln.LevelSymbol)},
			facts:      map[string]ModuleFacts{"example.com/lib": lib},
			pol:        Policy{AllowPatch: true},
			notFixable: map[string]string{"GO-5 example.com/lib": "the lowest fixing version v1.3.0 is ineligible: minor upgrades are not allowed by policy; none of the 1 later versions"},
		},
		{
			name:       "module on the deny list",
			findings:   []vuln.Finding{finding("GO-1", "example.com/lib", "v1.2.1", "v1.2.4", vuln.LevelSymbol)},
			facts:      map[string]ModuleFacts{"example.com/lib": lib},
			pol:        Policy{AllowMinor: true, AllowPatch: true, Deny: []string{"example.com/lib"}},
			notFixable: map[string]string{"GO-1 example.com/lib": "deny list"},
		},
		{
			name:     "a pin at a clearing version is honoured even when it is not the lowest",
			findings: []vuln.Finding{finding("GO-1", "example.com/lib", "v1.2.1", "v1.2.4", vuln.LevelSymbol)},
			facts:    map[string]ModuleFacts{"example.com/lib": lib},
			pol:      Policy{AllowMinor: true, AllowPatch: true, NamedDependency: "example.com/lib@v1.2.5"},
			want:     []string{"example.com/lib@v1.2.5"},
		},
		{
			name:       "a pin below the fix",
			findings:   []vuln.Finding{finding("GO-2", "example.com/lib", "v1.2.1", "v1.2.5", vuln.LevelSymbol)},
			facts:      map[string]ModuleFacts{"example.com/lib": lib},
			pol:        Policy{AllowMinor: true, AllowPatch: true, NamedDependency: "example.com/lib@v1.2.4"},
			notFixable: map[string]string{"GO-2 example.com/lib": "operator pinned version v1.2.4, which does not clear the findings (they need v1.2.5 or later)"},
		},
		{
			name:       "a pin naming a different module",
			findings:   []vuln.Finding{finding("GO-1", "example.com/lib", "v1.2.1", "v1.2.4", vuln.LevelSymbol)},
			facts:      map[string]ModuleFacts{"example.com/lib": lib},
			pol:        Policy{AllowMinor: true, AllowPatch: true, NamedDependency: "example.com/other"},
			notFixable: map[string]string{"GO-1 example.com/lib": "operator named a different dependency (example.com/other)"},
		},
		{
			name:     "a pre-release fix is skipped for the next release",
			findings: []vuln.Finding{finding("GO-6", "example.com/pre", "v1.0.0", "v1.1.0-rc.1", vuln.LevelSymbol)},
			facts:    map[string]ModuleFacts{"example.com/pre": {Current: "v1.0.0", Direct: true, Versions: []string{"v1.0.0", "v1.1.0-rc.1", "v1.1.0"}}},
			pol:      DefaultPolicy(),
			want:     []string{"example.com/pre@v1.1.0"},
		},
		{
			name:       "a fix published only as a pre-release",
			findings:   []vuln.Finding{finding("GO-6", "example.com/pre", "v1.0.0", "v1.1.0-rc.1", vuln.LevelSymbol)},
			facts:      map[string]ModuleFacts{"example.com/pre": {Current: "v1.0.0", Direct: true, Versions: []string{"v1.0.0", "v1.1.0-rc.1"}}},
			pol:        DefaultPolicy(),
			notFixable: map[string]string{"GO-6 example.com/pre": "pre-release versions are ineligible"},
		},
		{
			name:       "the fixed version is not published",
			findings:   []vuln.Finding{finding("GO-7", "example.com/lib", "v1.2.1", "v1.4.0", vuln.LevelSymbol)},
			facts:      map[string]ModuleFacts{"example.com/lib": {Current: "v1.2.1", Direct: true, Versions: []string{"v1.2.4", "v1.3.0"}}},
			pol:        DefaultPolicy(),
			notFixable: map[string]string{"GO-7 example.com/lib": "v1.4.0 is not in the module's published version list"},
		},
		{
			name:       "versions could not be listed",
			findings:   []vuln.Finding{finding("GO-1", "example.com/lib", "v1.2.1", "v1.2.4", vuln.LevelSymbol)},
			facts:      map[string]ModuleFacts{},
			pol:        DefaultPolicy(),
			notFixable: map[string]string{"GO-1 example.com/lib": ReasonNoFacts},
		},
		{
			name: "ordering: level, then direct before indirect, then path",
			findings: []vuln.Finding{
				finding("GO-A", "example.com/zzz", "v1.0.0", "v1.0.1", vuln.LevelModule),
				finding("GO-B", "example.com/yyy", "v1.0.0", "v1.0.1", vuln.LevelPackage),
				finding("GO-C", "example.com/indirect", "v1.0.0", "v1.0.1", vuln.LevelSymbol),
				finding("GO-D", "example.com/direct", "v1.0.0", "v1.0.1", vuln.LevelSymbol),
				finding("GO-E", "example.com/adirect", "v1.0.0", "v1.0.1", vuln.LevelSymbol),
				finding("GO-F", "stdlib", "v1.22.0", "v1.22.1", vuln.LevelSymbol),
			},
			facts: map[string]ModuleFacts{
				"example.com/zzz":      {Current: "v1.0.0", Direct: true, Versions: []string{"v1.0.1"}},
				"example.com/yyy":      {Current: "v1.0.0", Direct: false, Versions: []string{"v1.0.1"}},
				"example.com/indirect": {Current: "v1.0.0", Direct: false, Versions: []string{"v1.0.1"}},
				"example.com/direct":   {Current: "v1.0.0", Direct: true, Versions: []string{"v1.0.1"}},
				"example.com/adirect":  {Current: "v1.0.0", Direct: true, Versions: []string{"v1.0.1"}},
			},
			pol:  DefaultPolicy(),
			want: []string{"example.com/adirect@v1.0.1", "example.com/direct@v1.0.1", "example.com/indirect@v1.0.1", "example.com/yyy@v1.0.1", "example.com/zzz@v1.0.1"},
		},
		{
			name:     "an indirect module is a candidate",
			findings: []vuln.Finding{finding("GO-7", "example.com/inner", "v1.0.0", "v1.0.1", vuln.LevelSymbol)},
			facts:    map[string]ModuleFacts{"example.com/inner": {Current: "v1.0.0", Direct: false, Versions: []string{"v1.0.0", "v1.0.1", "v1.1.0"}}},
			pol:      DefaultPolicy(),
			want:     []string{"example.com/inner@v1.0.1"},
		},
		{
			name: "the module's level is its highest fixable finding's",
			findings: []vuln.Finding{
				finding("GO-1", "example.com/a", "v1.0.0", "v1.0.1", vuln.LevelModule),
				finding("GO-2", "example.com/a", "v1.0.0", "", vuln.LevelSymbol),
				finding("GO-3", "example.com/b", "v1.0.0", "v1.0.1", vuln.LevelPackage),
			},
			facts: map[string]ModuleFacts{
				"example.com/a": {Current: "v1.0.0", Direct: true, Versions: []string{"v1.0.1"}},
				"example.com/b": {Current: "v1.0.0", Direct: true, Versions: []string{"v1.0.1"}},
			},
			pol:     DefaultPolicy(),
			want:    []string{"example.com/b@v1.0.1", "example.com/a@v1.0.1"},
			unfixed: []string{"GO-2"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan := PlanVulnFixes(tc.findings, tc.facts, tc.pol)
			var got []string
			var unfixed []string
			for _, tg := range plan.Targets {
				got = append(got, tg.Module+"@"+tg.Version)
				for _, u := range tg.Unfixed {
					unfixed = append(unfixed, u.ID)
				}
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("targets = %v, want %v (not fixable %+v)", got, tc.want, plan.NotFixable)
			}
			if strings.Join(unfixed, ",") != strings.Join(tc.unfixed, ",") {
				t.Fatalf("unfixed = %v, want %v", unfixed, tc.unfixed)
			}
			if len(plan.NotFixable) != len(tc.notFixable) {
				t.Fatalf("not fixable = %+v, want %v", plan.NotFixable, tc.notFixable)
			}
			for _, n := range plan.NotFixable {
				want, ok := tc.notFixable[n.ID+" "+n.Module]
				if !ok || !strings.Contains(n.Reason, want) {
					t.Fatalf("not fixable %+v, want reason containing %q", n, want)
				}
			}
			if sel, ok := plan.Selected(); ok != (len(tc.want) > 0) || ok && sel.Module+"@"+sel.Version != tc.want[0] {
				t.Fatalf("selected %+v %v", sel, ok)
			}
		})
	}
}

func TestPlanVulnFixes_TargetDetail(t *testing.T) {
	plan := PlanVulnFixes([]vuln.Finding{
		finding("GO-1", "example.com/inner", "v1.0.0", "v1.0.1", vuln.LevelPackage),
		finding("GO-2", "example.com/inner", "v1.0.0", "v1.0.1", vuln.LevelSymbol),
	}, map[string]ModuleFacts{"example.com/inner": {Current: "v1.0.0", Versions: []string{"v1.0.1", "v1.1.0"}}}, DefaultPolicy())
	tg, ok := plan.Selected()
	if !ok || tg.Direct || tg.Needed != "v1.0.1" || tg.Delta != "patch" || tg.Level != vuln.LevelSymbol || strings.Join(tg.Fixes, ",") != "GO-1 example.com/inner,GO-2 example.com/inner" {
		t.Fatalf("target = %+v", tg)
	}
}

func TestPublishedVersions(t *testing.T) {
	fs := &fakeSandbox{outputs: map[string]string{
		"go list -e -m -versions -json example.com/inner example.com/gone": `{"Path":"example.com/inner","Versions":["v1.0.0","v1.0.1"]}
{"Path":"example.com/gone","Error":{"Err":"not found"}}`,
	}}
	got, err := PublishedVersions(context.Background(), fs, []string{"example.com/inner", "example.com/gone"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got["example.com/inner"], ",") != "v1.0.0,v1.0.1" || got["example.com/gone"] != nil || len(fs.calls) != 1 {
		t.Fatalf("versions = %v, calls %d", got, len(fs.calls))
	}
}
