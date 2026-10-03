package bench

import rtbench "github.com/joeylking/agent-runtime/bench"

// Comparison renders the newest result per mode and model in dir as one
// Markdown table with explicit denominators, followed by a per-scenario
// matrix. Results from different commits are refused unless mixed is set.
func Comparison(dir string, mixed bool) (string, error) {
	files, err := rtbench.ReadDir(dir)
	if err != nil {
		return "", err
	}
	return rtbench.Compare(rtbench.Latest(files), rtbench.CompareOptions{Order: []string{"baseline", "scripted"}, AllowMixedCommits: mixed, LinkPrefix: "results/"})
}
