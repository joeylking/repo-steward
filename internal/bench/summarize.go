package bench

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Latest returns the newest summary per mode and model found in dir.
func Latest(dir string) ([]*Summary, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	latest := map[string]*Summary{}
	files := map[string]string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		var s Summary
		if err := json.Unmarshal(b, &s); err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		key := s.Mode + "|" + s.Model
		if cur, ok := latest[key]; !ok || s.StartedAt.After(cur.StartedAt) {
			latest[key] = &s
			files[key] = e.Name()
		}
	}
	var out []*Summary
	for key, s := range latest {
		s.Notes = append([]string{"file:" + files[key]}, s.Notes...)
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Mode != out[j].Mode {
			return rank(out[i].Mode) < rank(out[j].Mode)
		}
		return out[i].Model < out[j].Model
	})
	return out, nil
}

func rank(mode string) int {
	switch mode {
	case "baseline":
		return 0
	case "scripted":
		return 1
	}
	return 2
}

// Comparison renders the latest results as one Markdown table with
// explicit denominators, followed by a per-scenario matrix.
func Comparison(sums []*Summary) string {
	var b strings.Builder
	b.WriteString("| Mode | Completed | Safe non-results | Incorrect refusals | Correct refusals | False successes | Failed | Model calls | File |\n|---|---|---|---|---|---|---|---|---|\n")
	scen := map[string]bool{}
	for _, s := range sums {
		name := s.Mode
		if s.Model != "" {
			name += " " + s.Model
		}
		reps := 1
		for _, r := range s.Runs {
			if r.Repeat > reps {
				reps = r.Repeat
			}
			scen[r.Scenario] = true
		}
		if reps > 1 {
			name += fmt.Sprintf(", %d repeats", reps)
		}
		file := strings.TrimPrefix(s.Notes[0], "file:")
		n := len(s.Runs)
		fmt.Fprintf(&b, "| %s | %d/%d | %d/%d | %d/%d | %d/%d | %d/%d | %d/%d | %d | [%s](results/%s) |\n", name,
			s.Completed, s.ProposalExpected, s.SafeNonResults, n, s.IncorrectRefusals, s.ProposalExpected, s.CorrectRefusals, s.RefusalExpected, s.FalseSuccesses, n, s.Failed, n, s.TotalModelCalls, file, file)
	}
	names := make([]string, 0, len(scen))
	for n := range scen {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool { return scenarioOrder(names[i]) < scenarioOrder(names[j]) })
	b.WriteString("\nPer scenario, outcome and score of each run:\n\n| Scenario |")
	for _, s := range sums {
		name := s.Mode
		if s.Model != "" {
			name = s.Model
		}
		fmt.Fprintf(&b, " %s |", name)
	}
	b.WriteString("\n|---|")
	for range sums {
		b.WriteString("---|")
	}
	b.WriteString("\n")
	for _, n := range names {
		fmt.Fprintf(&b, "| %s |", n)
		for _, s := range sums {
			var cells []string
			for _, r := range s.Runs {
				if r.Scenario == n {
					cells = append(cells, r.Outcome+" ("+r.Score+")")
				}
			}
			fmt.Fprintf(&b, " %s |", strings.Join(cells, "<br>"))
		}
		b.WriteString("\n")
	}
	return b.String()
}

func scenarioOrder(n string) string {
	// S1, S2, ... S10H sort numerically then by suffix.
	num := strings.TrimLeft(strings.TrimPrefix(n, "S"), "0123456789")
	digits := strings.TrimSuffix(strings.TrimPrefix(n, "S"), num)
	return fmt.Sprintf("%03s%s", digits, num)
}
