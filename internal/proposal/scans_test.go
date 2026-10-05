package proposal

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/joeylking/repo-steward/internal/task"
	"github.com/joeylking/repo-steward/internal/vuln"
)

var binding = ScanBinding{BaseTree: "base", CandidateTree: "cand", ConfigHash: "cfg", ToolchainDigest: "sha256:img"}

var dbTime = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func f(id, mod, found string, level vuln.Level) vuln.Finding {
	return vuln.Finding{Key: id + " " + mod, OSV: id, Module: mod, Stdlib: mod == vuln.StdlibModule, FoundVersion: found, Level: level}
}

func rec(id, kind, tree string, conclusive bool, findings ...vuln.Finding) task.ScanRecord {
	s := vuln.Scan{Conclusive: conclusive, Findings: findings}
	if s.Findings == nil {
		s.Findings = []vuln.Finding{}
	}
	if !conclusive {
		s.Reason = "scanner exited 1: build failed"
		s.Findings = []vuln.Finding{}
	}
	raw, _ := json.Marshal(s)
	return task.ScanRecord{ID: id, Kind: kind, TreeHash: tree, ConfigHash: "cfg", ToolchainDigest: "sha256:img", ScannerVersion: "v1.1.4", ScannerSHA256: "aaa",
		DBSnapshotID: "snap", DBModified: dbTime, GoVersion: "go1.22.12", Conclusive: conclusive, Reason: s.Reason, Scan: raw}
}

func failureCodes(fs []Failure) string {
	var out []string
	for _, x := range fs {
		out = append(out, x.Code)
	}
	return strings.Join(out, ",")
}

const target = "GO-TEST-0001 example.com/lib"

func TestCheckScans(t *testing.T) {
	vulnBase := []vuln.Finding{f("GO-TEST-0001", "example.com/lib", "v1.2.1", vuln.LevelSymbol), f("GO-TEST-0004", "stdlib", "v1.22.12", vuln.LevelPackage)}
	stdOnly := []vuln.Finding{f("GO-TEST-0004", "stdlib", "v1.22.12", vuln.LevelPackage)}
	cases := []struct {
		name  string
		recs  []task.ScanRecord // newest first
		codes string
		// detail is a fragment one failure must contain.
		detail   string
		resolved string
	}{
		{
			name:     "resolved, the standard-library finding persisting",
			recs:     []task.ScanRecord{rec("p", "post", "cand", true, stdOnly...), rec("b", "base", "base", true, vulnBase...)},
			resolved: target,
		},
		{
			name:   "persisting at another found version is not resolved",
			recs:   []task.ScanRecord{rec("p", "post", "cand", true, f("GO-TEST-0001", "example.com/lib", "v1.2.3", vuln.LevelSymbol), stdOnly[0]), rec("b", "base", "base", true, vulnBase...)},
			codes:  CodeAdvisoryUnresolved,
			detail: "still reported at v1.2.3 (symbol level); the base scan found v1.2.1",
		},
		{
			name:   "persisting at a lower level is not resolved either",
			recs:   []task.ScanRecord{rec("p", "post", "cand", true, f("GO-TEST-0001", "example.com/lib", "v1.2.1", vuln.LevelModule), stdOnly[0]), rec("b", "base", "base", true, vulnBase...)},
			codes:  CodeAdvisoryUnresolved,
			detail: "module level",
		},
		{
			name:     "a third-party advisory introduced",
			recs:     []task.ScanRecord{rec("p", "post", "cand", true, f("GO-TEST-0099", "example.com/lib", "v1.2.4", vuln.LevelSymbol), stdOnly[0]), rec("b", "base", "base", true, vulnBase...)},
			codes:    CodeAdvisoryIntroduced,
			detail:   "GO-TEST-0099 example.com/lib: introduced at example.com/lib v1.2.4",
			resolved: target,
		},
		{
			name:     "a standard-library advisory escalated",
			recs:     []task.ScanRecord{rec("p", "post", "cand", true, f("GO-TEST-0004", "stdlib", "v1.22.12", vuln.LevelSymbol)), rec("b", "base", "base", true, vulnBase...)},
			codes:    CodeAdvisoryIntroduced,
			detail:   "escalated from package to symbol level",
			resolved: target,
		},
		{
			name:   "an inconclusive post scan fails all three rules",
			recs:   []task.ScanRecord{rec("p", "post", "cand", false), rec("b", "base", "base", true, vulnBase...)},
			codes:  CodeScanInconclusive + "," + CodeAdvisoryUnresolved + "," + CodeAdvisoryIntroduced,
			detail: "inconclusive: scanner exited 1: build failed",
		},
		{
			name:  "no post scan",
			recs:  []task.ScanRecord{rec("b", "base", "base", true, vulnBase...)},
			codes: CodeScanNotBound + "," + CodeAdvisoryUnresolved + "," + CodeAdvisoryIntroduced,
		},
		{
			name:   "a post scan of another tree does not count",
			recs:   []task.ScanRecord{rec("p", "post", "other", true, stdOnly...), rec("b", "base", "base", true, vulnBase...)},
			codes:  CodeScanNotBound + "," + CodeAdvisoryUnresolved + "," + CodeAdvisoryIntroduced,
			detail: "no scan of candidate tree cand",
		},
		{
			name: "a post scan under another configuration does not count",
			recs: func() []task.ScanRecord {
				p := rec("p", "post", "cand", true, stdOnly...)
				p.ConfigHash = "other"
				return []task.ScanRecord{p, rec("b", "base", "base", true, vulnBase...)}
			}(),
			codes: CodeScanNotBound + "," + CodeAdvisoryUnresolved + "," + CodeAdvisoryIntroduced,
		},
		{
			name: "a different scanner binary",
			recs: func() []task.ScanRecord {
				p := rec("p", "post", "cand", true, stdOnly...)
				p.ScannerSHA256 = "bbb"
				return []task.ScanRecord{p, rec("b", "base", "base", true, vulnBase...)}
			}(),
			codes:  CodeScanNotBound + "," + CodeAdvisoryUnresolved + "," + CodeAdvisoryIntroduced,
			detail: `scanner sha256 "bbb", the base scan with "aaa"`,
		},
		{
			name: "a different database snapshot and modified time",
			recs: func() []task.ScanRecord {
				p := rec("p", "post", "cand", true, stdOnly...)
				p.DBSnapshotID, p.DBModified = "snap2", dbTime.Add(time.Hour)
				return []task.ScanRecord{p, rec("b", "base", "base", true, vulnBase...)}
			}(),
			codes:  CodeScanNotBound + "," + CodeScanNotBound + "," + CodeAdvisoryUnresolved + "," + CodeAdvisoryIntroduced,
			detail: "database snapshot",
		},
		{
			name: "a different toolchain digest on the base scan",
			recs: func() []task.ScanRecord {
				b := rec("b", "base", "base", true, vulnBase...)
				b.ToolchainDigest = "sha256:other"
				return []task.ScanRecord{rec("p", "post", "cand", true, stdOnly...), b}
			}(),
			codes:  CodeScanNotBound + "," + CodeAdvisoryUnresolved + "," + CodeAdvisoryIntroduced,
			detail: "no conclusive base scan of tree base",
		},
		{
			name:   "an inconclusive base scan does not count",
			recs:   []task.ScanRecord{rec("p", "post", "cand", true, stdOnly...), rec("b", "base", "base", false)},
			codes:  CodeScanNotBound + "," + CodeAdvisoryUnresolved + "," + CodeAdvisoryIntroduced,
			detail: "no conclusive base scan",
		},
		{
			name:   "a targeted finding the base scan never reported",
			recs:   []task.ScanRecord{rec("p", "post", "cand", true, stdOnly...), rec("b", "base", "base", true, stdOnly...)},
			codes:  CodeAdvisoryUnresolved,
			detail: "not in the base scan",
		},
		{
			name: "a record whose verdict disagrees with its scan",
			recs: func() []task.ScanRecord {
				p := rec("p", "post", "cand", false)
				p.Conclusive = true
				return []task.ScanRecord{p, rec("b", "base", "base", true, vulnBase...)}
			}(),
			codes: CodeScanNotBound + "," + CodeAdvisoryUnresolved + "," + CodeAdvisoryIntroduced,
		},
		{
			name:     "the newest post scan of the tree is the one that counts",
			recs:     []task.ScanRecord{rec("p2", "post", "cand", true, stdOnly...), rec("p1", "post", "cand", false), rec("b", "base", "base", true, vulnBase...)},
			resolved: target,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev, fails := CheckScans(tc.recs, binding, VulnCheck{Targeted: []string{target}})
			if got := failureCodes(fails); got != tc.codes {
				t.Fatalf("codes = %q, want %q: %+v", got, tc.codes, fails)
			}
			if tc.detail != "" {
				found := false
				for _, x := range fails {
					found = found || strings.Contains(x.Detail, tc.detail)
				}
				if !found {
					t.Fatalf("no failure mentions %q: %+v", tc.detail, fails)
				}
			}
			if strings.Join(ev.Resolved, ",") != tc.resolved {
				t.Fatalf("resolved = %v, want %q", ev.Resolved, tc.resolved)
			}
			if tc.codes == "" && (ev.BaseScanID != "b" || ev.PostScanID == "" || ev.ScannerSHA256 != "aaa" || ev.DBSnapshotID != "snap") {
				t.Fatalf("evidence = %+v", ev)
			}
		})
	}
}
