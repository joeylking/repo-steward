# repo-steward

An agentic repository maintenance tool built on
[agent-runtime](https://github.com/joeylking/agent-runtime). It performs one bounded dependency upgrade
on a local Go checkout, validates it in a sandbox, repairs breakage within
explicit scope limits, and prepares a pull request that a human approves before
anything leaves the machine.

## Status

Milestones 0 and 1 and the first batch of Milestone 2: fixtures, exact
snapshots, container sandbox, fail-closed validation, candidate discovery,
the no-model `inspect` command, the deterministic `maintain -mode baseline`
pipeline, the agent path on
[agent-runtime](https://github.com/joeylking/agent-runtime) with scoped
tools, policy, approvals, and resume, a scripted agent for deterministic
tests, and a model-driven agent that runs against a local Ollama model. No
paid model calls anywhere; no GitHub yet. See [docs/status.md](docs/status.md)
for what is implemented and which test verifies it.

## Data directory layout

Everything a run needs to be inspected or resumed lives under the data
directory: `steward.db` holds the runtime's runs, steps, approvals, and
events alongside the tasks, promotions, validation records, and proposals;
`runs/<id>/` holds the scratch clone, snapshots, and staging; `cache/mod/`
holds the module cache per module path. A run's options and candidate
facts are persisted at start so `resume` reconstructs the session under
the same configuration. Directories are never deleted and recreated at the
same path within a run, because VM-backed engines cache path lookups.

## Requirements

- Go 1.27 or later and Git.
- A Docker-compatible engine reachable over a unix socket for `inspect`
  (Docker Desktop, OrbStack, Colima, or Rancher Desktop). `fixture` and
  `snapshot` commands need no engine.
- With a VM-backed engine on macOS, the data directory and the repository
  must be under a shared path. Colima shares `$HOME` by default; the default
  data directory is under `$HOME` for that reason. The sandbox probes every
  mount and fails hard if the engine cannot see it.

## Quick start

```sh
go test -race ./...

# Materialize a fixture repository and the offline module proxy it resolves from.
go run ./cmd/repo-steward fixture setup patch-safe -dest ~/tmp/patch-safe
go run ./cmd/repo-steward fixture proxy -dest ~/tmp/proxy

# One-time bootstrap with network access: pull the pinned toolchain image.
docker pull golang@sha256:3d699e4d15d0f8f13c9195c0632a16702b8cbdece2955af1c23b37ae5d55a253

# Inspect: profile, cold-cache candidate discovery, baseline validation.
# With -fixture-proxy every container runs with the network disabled.
go run ./cmd/repo-steward inspect ~/tmp/patch-safe -fixture-proxy ~/tmp/proxy
```

`inspect` prints a JSON report and exits 0 when the baseline is clean, 2 when
the repository is unsupported, and 3 when the baseline fails or is
inconclusive.

```sh
# Baseline maintenance: pick the smallest eligible upgrade, apply it under
# the manifest gates, validate the exact result, and freeze a proposal
# commit in a scratch clone. Nothing is pushed and the source is untouched.
go run ./cmd/repo-steward maintain ~/tmp/patch-safe -mode baseline \
  -author "Your Name <you@example.com>" -fixture-proxy ~/tmp/proxy
```

```sh
# Scripted agent mode: replay an embedded scenario through the runtime,
# the full tool set, and the policy. -trace prints every runtime event.
go run ./cmd/repo-steward maintain ~/tmp/breaking-minor -mode scripted -scenario S2 \
  -author "Your Name <you@example.com>" -fixture-proxy ~/tmp/proxy -trace
```

`maintain` exits 0 when a proposal was prepared, 2 when unsupported, 3 on
baseline problems, 5 when the run paused for an approval, and 4 for any
other explained non-result such as a regression introduced by the upgrade
or a run that reported itself blocked.

```sh
# A paused run is decided and continued in separate processes. The
# approval is bound by hash to exactly the request that was shown.
go run ./cmd/repo-steward runs list
go run ./cmd/repo-steward runs show <run-id>
go run ./cmd/repo-steward approve <run-id> -note "small file"
go run ./cmd/repo-steward resume <run-id> -fixture-proxy ~/tmp/proxy

# A run whose process died mid-step is continued the same way: the
# interrupted step is recorded, journaled operations are reconciled, and
# the agent proceeds. No side effect is re-executed.
go run ./cmd/repo-steward resume <run-id> -fixture-proxy ~/tmp/proxy
```

```sh
# Model mode: a local model decides. Requires an Ollama server with a
# tool-calling model pulled. Every call is recorded with usage and latency;
# -record captures responses for later replay with -replay.
ollama serve &
ollama pull qwen3:30b-a3b
go run ./cmd/repo-steward maintain ~/tmp/breaking-minor -mode model \
  -model ollama:qwen3:30b-a3b -author "Your Name <you@example.com>" \
  -fixture-proxy ~/tmp/proxy -record ~/tmp/recordings -trace
```

Scope limits are flags on `maintain`: `-scope-files-soft`, `-scope-files-hard`,
`-scope-lines-soft`, `-scope-lines-hard`. Crossing a soft limit pauses the
run for one expansion approval, which raises the soft limits to the hard
ones; crossing a hard limit ends the run. The proposal commit lives under
`refs/repo-steward/proposals/<id>` in the scratch clone recorded in the
output; the scratch checkout's HEAD is never moved. Against a real repository omit `-fixture-proxy`; dependency
acquisition then reaches the public module proxy and checksum database, and
everything that executes repository code still runs with no network.

## What inspect does

1. Reads HEAD and lists its tree. Symlinks and submodules are refused.
2. Materializes the tree from raw objects and verifies every file.
3. Profiles go.mod and the tree: single module, supported go directive,
   no cgo, no vendoring, no workspace.
4. Starts the sandbox with the digest-pinned image for that Go version,
   reaps orphaned containers, and probes the mounts.
5. Populates the module cache and discovers outdated direct dependencies
   with per-version eligibility from policy (acquire profile).
6. Runs build, vet, and test with no network, read-only source and cache,
   and reports each check as pass, fail, or inconclusive (execute profile).

## What maintain does in baseline mode

1. Refuses a dirty source checkout, then clones it into a scratch workspace
   with a separate Git directory.
2. Profiles and validates the base tree exactly as `inspect` does.
3. Selects the smallest eligible upgrade: patch before minor before major,
   then the highest version in that class, then alphabetical module.
4. Runs `go get` against a staging copy of the manifests through `-modfile`
   in the mutate profile, checks Gate A (exact target, no reversions, no
   replace, exclude, go, or toolchain changes, transitive increases only
   within the target's requirement closure), and promotes the staging
   result under a journal that recovery can finish or abort.
5. Runs `go mod tidy` the same way and checks Gate B (Gate A plus tidy
   idempotence and no direct dependency removed).
6. Validates the exact candidate tree, compares findings with the baseline,
   and refuses on any introduced finding.
7. Evaluates readiness: bound validation, manifest rules including cache
   verification and target resolution, protected paths, scope limits.
8. Freezes the proposal from a persisted commit recipe and points a
   proposal ref at it.

## Benchmarks

`bench run` executes scenarios through a mode and scores them against what
each scenario declares, with denominators stated and completions never
added to refusals. Results are committed under `benchmarks/results/` as
data. See [benchmarks/README.md](benchmarks/README.md).

## The agent path

In agent modes the runtime drives a decision loop. Each decision is a
tool call that the runtime schema-validates and passes to the policy
before anything executes. The tools are the only capabilities the agent
has; none can reach the network, the operator's checkout, or a protected
file:

| Tool | Class | Notes |
|---|---|---|
| `get_repository_profile`, `list_candidates` | read | Facts computed by deterministic code. |
| `read_file`, `list_directory`, `search_files`, `git_diff` | read | Working tree only; symlink components and traversal refused. |
| `read_dependency_source` | read | Files of a module version from the module cache. |
| `apply_upgrade` | local | Exact eligible target only, once per run, through manifest staging and Gate A. |
| `write_file` | local | Regular source files only; tests, CI, security, and manifest paths denied; ignored paths refused; scope checked on the projected diff before the write. |
| `normalize_manifests` | local | `go mod tidy` through staging and Gate B. |
| `run_validation` | read | Build, vet, test on the exact candidate tree; introduced findings relative to the baseline. |
| `prepare_proposal` | local, terminal | Readiness, then a frozen proposal commit. |
| `report_blocked` | terminal | Ends the run with an explained non-result. |

The policy denies tools outside the current phase, checks eligibility on
the exact module and version, denies protected and ignored writes, aborts
on hard scope limits, asks for one scope expansion approval on soft limits,
and aborts when the validation budget or the no-progress budget is spent.

The scripted agent in `-mode scripted` replays a fixed decision list. It
proves the orchestration and control behaviour deterministically and says
nothing about a real model's repair quality, which is measured separately.

The model agent in `-mode model` is stateless between steps: each decision
renders the recorded steps into a message list, asks the model through the
runtime's accounting caller, and maps the first tool call to a decision.
The system prompt is fixed and never contains repository content; files and
command output reach the model only inside tool results, labelled as data.
The runtime enforces call, token, and cost limits before every request and
records every attempt. Local models through Ollama are the development
provider precisely because they cost nothing to iterate against; a paid
provider, when one is added, is used only for an explicit, budgeted
measurement and never in tests or CI.

## Integration tests

```sh
go test -tags integration -p 1 ./...
go test -tags faultinject ./internal/manifest/ ./internal/proposal/
```

Integration tests need the engine and the pinned image and never pull.
Packages run serially because `inspect` reaps every container carrying the
sandbox label. The fault-injection tests crash a child process at each
journaled point and recover in the parent; they need no engine.
