// Package names holds tool names and naming helpers shared by tools,
// policy, and the runner so that no package needs to import another for a
// string constant.
package names

import (
	"strings"

	"github.com/joeylking/repo-steward/internal/manifest"
)

// Tool names.
const (
	Profile      = "get_repository_profile"
	Candidates   = "list_candidates"
	ReadFile     = "read_file"
	ListDir      = "list_directory"
	Search       = "search_files"
	DepSource    = "read_dependency_source"
	Diff         = "git_diff"
	ApplyUpgrade = "apply_upgrade"
	WriteFile    = "write_file"
	Normalize    = "normalize_manifests"
	Validate     = "run_validation"
	Prepare      = "prepare_proposal"
	Blocked      = "report_blocked"
	Publish      = "publish_proposal"
)

// ReadOnly lists the tools that never change anything.
var ReadOnly = []string{Profile, Candidates, ReadFile, ListDir, Search, DepSource, Diff}

// HeadRef names the branch a proposal for target would be pushed to.
func HeadRef(t manifest.Target) string {
	last := t.Module
	if i := strings.LastIndex(last, "/"); i >= 0 {
		last = last[i+1:]
	}
	return "repo-steward/" + last + "-" + t.Version
}
