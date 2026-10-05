package proposal

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/joeylking/repo-steward/internal/coverage"
	"github.com/joeylking/repo-steward/internal/gitx"
	"github.com/joeylking/repo-steward/internal/validate"
	"github.com/joeylking/repo-steward/internal/workspace"
)

// CodeNotExercised is the readiness failure for a source change that the
// repository's tests do not execute, or whose execution could not be
// verified.
const CodeNotExercised = "repair_not_exercised"

// CoverageEvidence is what readiness found about test coverage of the
// changed source. It is present only when the change touches anything
// beyond the manifests.
type CoverageEvidence struct {
	coverage.Verdict
	// ValidationID is the validation whose coverage run was judged.
	ValidationID string `json:"validation_id,omitempty"`
	// Approval is set when an operator approved this exact tree without
	// test coverage; readiness then passes despite the verdict.
	Approval *UnexercisedApproval `json:"approval,omitempty"`
}

// UnexercisedApproval is an operator's approval of a repair no test
// exercises, bound to the candidate tree it was asked for.
type UnexercisedApproval struct {
	ID        string `json:"id"`
	Tree      string `json:"tree"`
	DecidedBy string `json:"decided_by,omitempty"`
	Note      string `json:"note,omitempty"`
}

// SourceChanged reports whether a change set touches anything other than
// go.mod and go.sum. Only such a change needs coverage evidence.
func SourceChanged(cs workspace.ChangeSet) bool {
	for _, f := range cs.Files {
		if f.Path != "go.mod" && f.Path != "go.sum" {
			return true
		}
	}
	return false
}

// Unexercised renders the verdict's unverified ranges for a failure detail
// or an approval, one per entry.
func Unexercised(v coverage.Verdict) []string {
	out := make([]string, 0, len(v.Unverified))
	for _, r := range v.Unverified {
		out = append(out, r.String())
	}
	return out
}

// CheckCoverage applies the coverage rule to the change from the base tree
// to cs.Tree with the coverage run recorded in post, the bound validation
// of that tree, which may be nil.
func CheckCoverage(ctx context.Context, ws *workspace.Workspace, cs workspace.ChangeSet, modulePath string, post *validate.Run) (coverage.Verdict, error) {
	g := ws.Git()
	var files []coverage.FileChange
	for _, f := range cs.Files {
		if f.Path == "go.mod" || f.Path == "go.sum" {
			continue
		}
		fc := coverage.FileChange{Path: f.Path, Status: f.Status, Binary: f.Binary}
		if !f.Binary {
			if f.Status != "A" {
				b, err := g.Run(ctx, "show", cs.BaseTree+":"+f.Path)
				if err != nil {
					return coverage.Verdict{}, err
				}
				fc.Old = b
			}
			if f.Status != "D" {
				b, err := g.Run(ctx, "show", cs.Tree+":"+f.Path)
				if err != nil {
					return coverage.Verdict{}, err
				}
				fc.New = b
			}
			diff, err := g.Run(ctx, "diff-tree", "-p", "-U0", "--no-color", "--no-ext-diff", "--no-textconv", "--no-renames", cs.BaseTree, cs.Tree, "--", ":(literal)"+f.Path)
			if err != nil {
				return coverage.Verdict{}, err
			}
			if fc.Hunks, err = coverage.ParseHunks(diff); err != nil {
				return coverage.Verdict{}, err
			}
			if len(fc.Hunks) == 0 {
				// git found no text hunks in a change it listed: treat it
				// as the binary change it must be.
				fc.Binary = true
			}
		}
		files = append(files, fc)
	}
	var ev *coverage.Evidence
	if post != nil {
		ev = post.Coverage
	}
	return coverage.Check(coverage.Input{ModulePath: modulePath, Files: files, Evidence: ev, PackageFuncs: packageFuncs(ctx, g, cs.Tree)}), nil
}

// packageFuncs lists the functions declared by the non-test Go files of a
// directory of tree.
func packageFuncs(ctx context.Context, g *gitx.Git, tree string) func(string) (map[string]bool, error) {
	var entries []gitx.TreeEntry
	var listErr error
	listed := false
	return func(dir string) (map[string]bool, error) {
		if !listed {
			entries, listErr = g.LsTree(ctx, tree)
			listed = true
		}
		if listErr != nil {
			return nil, listErr
		}
		out := map[string]bool{}
		for _, e := range entries {
			if e.Type != "blob" || path.Dir(e.Path) != dir || !strings.HasSuffix(e.Path, ".go") || strings.HasSuffix(e.Path, "_test.go") {
				continue
			}
			src, err := g.Run(ctx, "cat-file", "blob", e.Blob)
			if err != nil {
				return nil, err
			}
			m, err := coverage.DeclaredFuncs(src)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", e.Path, err)
			}
			for k := range m {
				out[k] = true
			}
		}
		return out, nil
	}
}

// coverageSummary is the proposal body's statement of the coverage
// evidence, deterministic for a given readiness.
func coverageSummary(c *CoverageEvidence) string {
	if c == nil {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Changed source executed by tests: %d of %d required coverage blocks in %d changed source file(s), go test -coverpkg=./... on the validated tree.\n", c.Executed, c.Blocks, c.Files)
	if c.Approval != nil {
		fmt.Fprintf(&b, "Not exercised by any test: %s.\n", strings.Join(Unexercised(c.Verdict), "; "))
	}
	return b.String()
}

// CoverageWarning is the statement that opens the body of a proposal whose
// repair an operator approved without test coverage, or "" for any other.
func CoverageWarning(r *Readiness) string {
	if r == nil || r.Coverage == nil || r.Coverage.Approval == nil {
		return ""
	}
	a := r.Coverage.Approval
	by := ""
	if a.DecidedBy != "" {
		by = " by " + a.DecidedBy
	}
	return fmt.Sprintf("WARNING: NO TEST EXERCISES PART OF THIS REPAIR. It was approved without test coverage%s in approval %s, for tree %s; readiness did not verify it.\n\n", by, a.ID, a.Tree)
}

// CoverageSummary is coverageSummary for callers that build a body.
func CoverageSummary(r *Readiness) string {
	if r == nil {
		return ""
	}
	return coverageSummary(r.Coverage)
}
