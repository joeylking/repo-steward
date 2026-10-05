package proposal

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/joeylking/repo-steward/internal/task"
	"github.com/joeylking/repo-steward/internal/vuln"
)

// VulnCheck makes readiness require vulnerability evidence: the run chose
// its upgrade to clear the Targeted findings, keyed "<id> <module>".
type VulnCheck struct {
	Targeted []string
}

// ScanEvidence is what readiness found in the scan records. It is part of
// the readiness report, and so of the frozen proposal and its hash.
type ScanEvidence struct {
	BaseScanID     string    `json:"base_scan_id,omitempty"`
	PostScanID     string    `json:"post_scan_id,omitempty"`
	ScannerVersion string    `json:"scanner_version,omitempty"`
	ScannerSHA256  string    `json:"scanner_sha256,omitempty"`
	DBSnapshotID   string    `json:"db_snapshot_id,omitempty"`
	DBModified     time.Time `json:"db_modified,omitempty"`
	// Resolved are the targeted keys absent from the post scan at every
	// level and every found version.
	Resolved []string `json:"resolved,omitempty"`
	// Introduced and Escalated are post findings new to the candidate, or
	// reported at a higher level than in the base scan.
	Introduced []string `json:"introduced,omitempty"`
	Escalated  []string `json:"escalated,omitempty"`
}

// Scan failure codes.
const (
	CodeScanNotBound       = "scan_not_bound"
	CodeScanInconclusive   = "scan_inconclusive"
	CodeAdvisoryUnresolved = "advisory_not_resolved"
	CodeAdvisoryIntroduced = "advisory_introduced"
)

// ScanBinding is what scan evidence must be bound to.
type ScanBinding struct {
	BaseTree        string
	CandidateTree   string
	ConfigHash      string
	ToolchainDigest string
}

// CheckScans applies the vulnerability readiness rules to a run's scan
// records, newest first as the store lists them. Every rule is evaluated
// and every failure returned:
//
//  1. a conclusive post scan of the candidate tree under the same
//     configuration and toolchain, run with the scanner and database
//     snapshot of a conclusive base scan of the base tree;
//  2. every targeted finding absent from that post scan at every level;
//  3. nothing introduced or escalated relative to the base scan, standard
//     library included.
//
// Without a usable post scan, rules 2 and 3 fail too: an inconclusive scan
// shows nothing, including that a finding is gone.
func CheckScans(records []task.ScanRecord, b ScanBinding, check VulnCheck) (*ScanEvidence, []Failure) {
	ev := &ScanEvidence{}
	var fails []Failure
	fail := func(code, format string, args ...any) {
		fails = append(fails, Failure{Code: code, Detail: fmt.Sprintf(format, args...)})
	}
	bound := func(r task.ScanRecord, kind, tree string) bool {
		return r.Kind == kind && r.TreeHash == tree && r.ConfigHash == b.ConfigHash && r.ToolchainDigest == b.ToolchainDigest
	}
	var baseRec, postRec *task.ScanRecord
	for i := range records {
		r := &records[i]
		if baseRec == nil && bound(*r, "base", b.BaseTree) && r.Conclusive {
			baseRec = r
		}
		if postRec == nil && bound(*r, "post", b.CandidateTree) {
			postRec = r
		}
	}
	var base, post *vuln.Scan
	if baseRec == nil {
		fail(CodeScanNotBound, "no conclusive base scan of tree %s under the current configuration", b.BaseTree)
	} else {
		ev.BaseScanID, ev.ScannerVersion, ev.ScannerSHA256, ev.DBSnapshotID, ev.DBModified = baseRec.ID, baseRec.ScannerVersion, baseRec.ScannerSHA256, baseRec.DBSnapshotID, baseRec.DBModified
		var err error
		if base, err = decodeScan(*baseRec); err != nil {
			fail(CodeScanNotBound, "base scan %s: %v", baseRec.ID, err)
		}
	}
	usable := false
	switch {
	case postRec == nil:
		fail(CodeScanNotBound, "no scan of candidate tree %s under the current configuration", b.CandidateTree)
	default:
		ev.PostScanID = postRec.ID
		mismatch := false
		if baseRec != nil {
			for _, d := range []struct{ what, post, base string }{
				{"scanner version", postRec.ScannerVersion, baseRec.ScannerVersion},
				{"scanner sha256", postRec.ScannerSHA256, baseRec.ScannerSHA256},
				{"database snapshot", postRec.DBSnapshotID, baseRec.DBSnapshotID},
				{"database modified time", postRec.DBModified.UTC().Format(time.RFC3339Nano), baseRec.DBModified.UTC().Format(time.RFC3339Nano)},
				{"go version", postRec.GoVersion, baseRec.GoVersion},
			} {
				if d.post != d.base {
					mismatch = true
					fail(CodeScanNotBound, "post scan %s ran with %s %q, the base scan with %q", postRec.ID, d.what, d.post, d.base)
				}
			}
		}
		var err error
		post, err = decodeScan(*postRec)
		switch {
		case err != nil:
			fail(CodeScanNotBound, "post scan %s: %v", postRec.ID, err)
		case !postRec.Conclusive:
			fail(CodeScanInconclusive, "post scan of tree %s is inconclusive: %s", b.CandidateTree, postRec.Reason)
		default:
			usable = baseRec != nil && base != nil && !mismatch
		}
	}
	if !usable {
		for _, k := range check.Targeted {
			fail(CodeAdvisoryUnresolved, "%s: cannot be shown resolved without a conclusive post scan bound to the base scan", k)
		}
		fail(CodeAdvisoryIntroduced, "the candidate cannot be shown to introduce no vulnerability without a conclusive post scan bound to the base scan")
		return ev, fails
	}
	for _, k := range check.Targeted {
		bf, inBase := findingByKey(base, k)
		pf, inPost := findingByKey(post, k)
		switch {
		case !inBase:
			fail(CodeAdvisoryUnresolved, "%s: not in the base scan, so the run did not target it from evidence", k)
		case inPost:
			fail(CodeAdvisoryUnresolved, "%s: still reported at %s (%s level); the base scan found %s", k, pf.FoundVersion, pf.Level, bf.FoundVersion)
		default:
			ev.Resolved = append(ev.Resolved, k)
		}
	}
	d, err := vuln.Compare(base, post)
	if err != nil {
		fail(CodeAdvisoryIntroduced, "%v", err)
		return ev, fails
	}
	for _, f := range d.Introduced {
		ev.Introduced = append(ev.Introduced, f.Key)
		fail(CodeAdvisoryIntroduced, "%s: introduced at %s %s (%s level)", f.Key, f.Module, f.FoundVersion, f.Level)
	}
	for _, f := range d.Escalated {
		bf, _ := base.Finding(f.OSV, f.Module)
		ev.Escalated = append(ev.Escalated, f.Key)
		fail(CodeAdvisoryIntroduced, "%s: escalated from %s to %s level", f.Key, bf.Level, f.Level)
	}
	return ev, fails
}

// decodeScan reads a record's parsed scan and checks it agrees with the
// record's own verdict.
func decodeScan(r task.ScanRecord) (*vuln.Scan, error) {
	var s vuln.Scan
	if err := json.Unmarshal(r.Scan, &s); err != nil {
		return nil, err
	}
	if s.Conclusive != r.Conclusive {
		return nil, fmt.Errorf("record says conclusive=%v, the scan %v", r.Conclusive, s.Conclusive)
	}
	sort.Slice(s.Findings, func(i, j int) bool { return s.Findings[i].Key < s.Findings[j].Key })
	return &s, nil
}

func findingByKey(s *vuln.Scan, key string) (vuln.Finding, bool) {
	for _, f := range s.Findings {
		if f.Key == key {
			return f, true
		}
	}
	return vuln.Finding{}, false
}
