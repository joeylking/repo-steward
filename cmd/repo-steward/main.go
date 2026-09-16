// Command repo-steward is the CLI. Milestone 0 provides fixture setup,
// snapshot construction, and the no-model inspect workflow; maintain, runs,
// approve, and reject arrive with later milestones.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/joeylking/repo-steward/internal/deps"
	"github.com/joeylking/repo-steward/internal/fixture"
	"github.com/joeylking/repo-steward/internal/gitx"
	"github.com/joeylking/repo-steward/internal/inspect"
	"github.com/joeylking/repo-steward/internal/modproxy"
	"github.com/joeylking/repo-steward/internal/snapshot"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "repo-steward:", err)
		os.Exit(1)
	}
}

const usage = `usage:
  repo-steward inspect <repo-path> [-data-dir DIR] [-fixture-proxy DIR] [-pull] [-allow-major] [-dependency MODULE] [-check-timeout DURATION]
  repo-steward fixture list
  repo-steward fixture setup <name> [-dest DIR]
  repo-steward fixture proxy [-dest DIR]
  repo-steward snapshot build <repo-path> [-dest DIR] [-max-files N] [-max-bytes N]
`

func run(args []string) error {
	if len(args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		return fmt.Errorf("missing command")
	}
	ctx := context.Background()
	if args[0] == "inspect" {
		return runInspect(ctx, args[1:])
	}
	switch args[0] + " " + args[1] {
	case "fixture list":
		names, err := fixture.Names()
		if err != nil {
			return err
		}
		for _, n := range names {
			def, err := fixture.Load(n)
			if err != nil {
				return err
			}
			fmt.Printf("%-20s %s\n", n, def.Description)
		}
		return nil
	case "fixture setup":
		name, rest, err := positional(args[2:], "fixture setup: expected a fixture name")
		if err != nil {
			return err
		}
		fs := flag.NewFlagSet("fixture setup", flag.ContinueOnError)
		dest := fs.String("dest", "", "destination directory (default: a new temporary directory)")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		if *dest == "" {
			d, err := os.MkdirTemp("", "repo-steward-fixture-"+name+"-")
			if err != nil {
				return err
			}
			*dest = d
		}
		repo, err := fixture.Setup(ctx, name, *dest)
		if err != nil {
			return err
		}
		return printJSON(repo)
	case "fixture proxy":
		fs := flag.NewFlagSet("fixture proxy", flag.ContinueOnError)
		dest := fs.String("dest", "", "destination directory (default: a new temporary directory)")
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		if *dest == "" {
			d, err := os.MkdirTemp("", "repo-steward-proxy-")
			if err != nil {
				return err
			}
			*dest = d
		}
		ix, err := modproxy.Build(*dest)
		if err != nil {
			return err
		}
		return printJSON(map[string]any{"dir": ix.Dir, "goproxy": modproxy.URL(ix.Dir), "modules": ix.Modules})
	case "snapshot build":
		arg, rest, err := positional(args[2:], "snapshot build: expected a repository path")
		if err != nil {
			return err
		}
		fs := flag.NewFlagSet("snapshot build", flag.ContinueOnError)
		dest := fs.String("dest", "", "destination directory (default: a new temporary directory)")
		maxFiles := fs.Int("max-files", snapshot.DefaultLimits().MaxFiles, "maximum number of files")
		maxBytes := fs.Int64("max-bytes", snapshot.DefaultLimits().MaxTotalBytes, "maximum total bytes")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		repoPath, err := filepath.Abs(arg)
		if err != nil {
			return err
		}
		if *dest == "" {
			d, err := os.MkdirTemp("", "repo-steward-snapshot-")
			if err != nil {
				return err
			}
			*dest = d
		}
		limits := snapshot.DefaultLimits()
		limits.MaxFiles = *maxFiles
		limits.MaxTotalBytes = *maxBytes

		g := gitx.New(repoPath)
		start := time.Now()
		base, err := g.RevParse(ctx, "HEAD^{tree}")
		if err != nil {
			return err
		}
		candidate, err := snapshot.BuildCandidateTree(ctx, g, base)
		if err != nil {
			return err
		}
		m, err := snapshot.Materialize(ctx, g, candidate, *dest, limits)
		if err != nil {
			return err
		}
		return printJSON(map[string]any{
			"repo":           repoPath,
			"base_tree":      base,
			"candidate_tree": m.Tree,
			"changed":        candidate != base,
			"dir":            m.Dir,
			"file_count":     m.FileCount,
			"total_bytes":    m.TotalBytes,
			"verified":       m.Verified,
			"elapsed_ms":     time.Since(start).Milliseconds(),
		})
	default:
		fmt.Fprint(os.Stderr, usage)
		return fmt.Errorf("unknown command %q", args[0]+" "+args[1])
	}
}

// runInspect performs a no-model inspection and prints the report. Exit
// status 2 means the repository is unsupported and 3 means the baseline is
// failing or inconclusive.
func runInspect(ctx context.Context, args []string) error {
	repoPath, rest, err := positional(args, "inspect: expected a repository path")
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("inspect", flag.ContinueOnError)
	dataDir := fs.String("data-dir", "", "data directory (default: $XDG_DATA_HOME/repo-steward or ~/.local/share/repo-steward)")
	proxyDir := fs.String("fixture-proxy", "", "file-based module proxy directory; disables network and checksum database (fixtures only)")
	pull := fs.Bool("pull", false, "pull the pinned toolchain image if it is not present (one-time bootstrap)")
	allowMajor := fs.Bool("allow-major", false, "treat major upgrades as eligible")
	dependency := fs.String("dependency", "", "restrict eligibility to one module")
	checkTimeout := fs.Duration("check-timeout", 10*time.Minute, "timeout per validation check")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	pol := deps.DefaultPolicy()
	pol.AllowMajor = *allowMajor
	pol.NamedDependency = *dependency
	rep, err := inspect.Run(ctx, inspect.Options{RepoPath: repoPath, DataDir: *dataDir, FixtureProxyDir: *proxyDir, AllowPull: *pull, Policy: pol, CheckTimeout: *checkTimeout})
	if err != nil {
		return err
	}
	if err := printJSON(rep); err != nil {
		return err
	}
	switch rep.Outcome {
	case inspect.OutcomeUnsupported:
		os.Exit(2)
	case inspect.OutcomeBaselineFailing, inspect.OutcomeBaselineInconclusive:
		os.Exit(3)
	}
	return nil
}

// positional takes the first argument as a positional value and returns the
// remainder for flag parsing, so flags may follow the positional.
func positional(args []string, msg string) (string, []string, error) {
	if len(args) == 0 || len(args[0]) == 0 || args[0][0] == '-' {
		return "", nil, fmt.Errorf("%s", msg)
	}
	return args[0], args[1:], nil
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
