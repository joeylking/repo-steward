# repo-steward

[![ci](https://github.com/joeylking/repo-steward/actions/workflows/ci.yml/badge.svg)](https://github.com/joeylking/repo-steward/actions/workflows/ci.yml)

An agentic repository maintenance tool built on
[agent-runtime](https://github.com/joeylking/agent-runtime). It performs one bounded dependency upgrade
on a local Go checkout, validates it in a sandbox, repairs breakage within
explicit scope limits, and prepares a pull request that a human approves before
anything leaves the machine.

## Status

v0.1. The whole path exists: exact snapshots, a container sandbox,
fail-closed validation, candidate discovery, a deterministic baseline
pipeline, the agent path on
[agent-runtime](https://github.com/joeylking/agent-runtime) with scoped
tools, policy, approvals, and resume, a scripted agent for deterministic
tests, a model-driven agent against local Ollama models on the runtime's
provider, renderer, trace, and operator read models, committed
benchmarks over eleven scenarios, and publication under a hash-bound
approval with journaled, reconcilable operations. No paid model call is
made by any development, test, or CI path; the one paid provider exists
for explicit, capped measurements, see Benchmarks. Publication is tested against a fake GitHub server and a local bare
repository, and has run once for real: on 2026-09-22 a model-mode run on a
private Go repository of the author's upgraded pgx from v5.7.4 to v5.7.5,
paused for approval, and on resume pushed one branch and opened one pull
request on github.com, changing go.mod and go.sum only.

- [docs/architecture.md](docs/architecture.md): stages, evidence, sandbox profiles, runs and recovery.
- [docs/security.md](docs/security.md): assets, boundaries, every control with its status and test, accepted risks.
- [docs/status.md](docs/status.md): the milestone-by-milestone control table.
- [docs/decisions](docs/decisions): why it is built this way.
- [benchmarks/README.md](benchmarks/README.md): scoring and committed results.
- [Wiki](https://github.com/joeylking/repo-steward/wiki): the same system at length, for users, developers, and operators, with a glossary and the design decisions explained.

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
- The runtime is pinned in `go.mod` at released versions: `agent-runtime
  v0.2.0`, and the provider adapters at their own nested module tags,
  `providers/ollama/v0.1.0` and `providers/anthropic/v0.1.0`.
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

```sh
# Publication. The destination is parsed from the source's origin remote
# (or given with -destination owner/repo) and verified before the run
# starts: the base branch on github.com must be exactly at the local HEAD.
# The run pauses on a publication approval bound to the frozen proposal;
# approve and resume push the commit and open the pull request. The token
# is read from GITHUB_TOKEN, used for the API and for the push through a
# credential helper, and never written anywhere.
export GITHUB_TOKEN=...   # fine-grained, contents and pull requests on the repo
go run ./cmd/repo-steward maintain ~/src/myrepo -mode model -publish
go run ./cmd/repo-steward approve <run-id> -note "reviewed the diff"
go run ./cmd/repo-steward resume <run-id>
```

Scope limits are flags on `maintain`: `-scope-files-soft`, `-scope-files-hard`,
`-scope-lines-soft`, `-scope-lines-hard`. Crossing a soft limit pauses the
run for one expansion approval, which raises the soft limits to the hard
ones; crossing a hard limit ends the run. The proposal commit lives under
`refs/repo-steward/proposals/<id>` in the scratch clone recorded in the
output; the scratch checkout's HEAD is never moved. Against a real repository omit `-fixture-proxy`; dependency
acquisition then reaches the public module proxy and checksum database, and
everything that executes repository code still runs with no network.
`-dependency module@version` pins one exact target; only versions newer
than the current one are ever candidates, so a pin cannot downgrade.

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
each scenario declares: acceptable outcomes, allowed and required files,
hidden oracle checks on the proposal tree, forbidden proposal text for
injection cases, and a model-call bound. Denominators are stated and
completions are never added to refusals. Eleven scenarios cover a patch
upgrade, two repairs, a closure-driven regression, a failing baseline, an
ineligible major, a major beyond scope, injected instructions, a protected
change, a toolchain gap, and a hard scope limit. Results are committed under
`benchmarks/results/` as data. See [benchmarks/README.md](benchmarks/README.md).

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
| `prepare_proposal` | local | Readiness, then a frozen proposal commit. Terminal unless publication is enabled. |
| `publish_proposal` | remote, terminal | Only with `-publish`. Always requires a publication approval bound to the proposal's hash; verifies the proposal and the working tree again on resume; pushes the commit and opens the pull request as two journaled operations. |
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
The rendering and that mapping are the runtime's `render` package; what
this repository owns is the domain text, which is the facts, the system
prompt, the opening message, and the labelled tool-result format. A reply
that contains no executable tool call, or one cut off by the output cap, is
recorded as an invalid decision and answered on the next step with a nudge
naming what was wrong: it costs one step against the limits, and the audit
log shows it, instead of a second model call hidden inside one step.
The system prompt is fixed and never contains repository content; files and
command output reach the model only inside tool results, labelled as data.
The runtime enforces call, token, and cost limits before every request and
records every attempt. The provider adapters are the runtime's nested
modules `providers/ollama` and `providers/anthropic`, so the transport
classification, the price tables, and the paid-model refusal are shared
rather than rebuilt here. Local models through Ollama are the development
provider precisely because they cost nothing to iterate against, and they
are priced free so that free and unpriced stay distinguishable. The one
paid provider, `anthropic:<model>`, is constructed by nothing in tests or
CI, refuses to run without a key in `ANTHROPIC_API_KEY` or `CLAUDE_KEY`,
refuses to run without a known price and a `-max-cost-usd` cap, and charges
cache writes at their higher rate so the cap never undercounts.
`bench run` adds `-max-total-cost-usd`, which it stops before a run could
exceed. Both caps are written as `5`, `4.41`, or `$0.50` and parsed by the
runtime's dollar parser, which refuses anything else. It is used only for explicit, budgeted measurements.

## Integration tests

```sh
go test -tags integration -p 1 ./...
go test -tags faultinject ./internal/manifest/ ./internal/proposal/
```

## Smoke scenarios against public modules

`internal/smoke` holds five scenarios that clone a real public repository
at a pinned commit, upgrade one direct dependency to a pinned version
through the public module proxy and checksum database, and check the
outcome: two proposals (a patch on go-playground/validator, a minor on
labstack/echo), a pin to an older version that yields no candidate, a
target whose go directive exceeds the repository's pinned toolchain, and a
repository whose test suite reaches the network and therefore fails its
baseline in the sandbox. They need the network and the toolchain images
the targets declare, so they run under their own tag and in a separate
on-demand and weekly workflow, not on every push.

```sh
go test -tags smoke -count=1 -p 1 -v ./internal/smoke/
```

Writing them exposed two defects that the synthetic fixtures had hidden:
the build check passed `-o` unconditionally, which `go build` refuses for
a library-only module, and a `go.mod` under a directory the go tool
ignores, such as `_examples`, was refused as a nested module. Both are
fixed and covered by unit tests.

Integration tests need the engine and the pinned image and never pull.
Packages run serially because `inspect` reaps every container carrying the
sandbox label. The fault-injection tests crash a child process at each
journaled point and recover in the parent; they need no engine.
