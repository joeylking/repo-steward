# repo-steward

[![ci](https://github.com/joeylking/repo-steward/actions/workflows/ci.yml/badge.svg)](https://github.com/joeylking/repo-steward/actions/workflows/ci.yml)

repo-steward upgrades one dependency of a Go project, checks that the
project still builds and passes its tests, repairs what the upgrade broke,
and prepares a pull request for a person to approve. An AI model does the
repair, inside limits the model cannot change. Nothing leaves the machine
until a person says so.

It is built on [agent-runtime](https://github.com/joeylking/agent-runtime),
which supplies those limits.

This page starts with the problem and the approach in plain language, for
any reader. The commands and the internals follow, for people who want to
run it or change it.

## The problem

Almost all software is built on code written by other people. A project
uses dozens of these libraries, called dependencies, and each one keeps
publishing new versions that fix bugs and close security holes. A project
that stops upgrading falls behind, and the fixes it needs sit in versions
it does not use.

Upgrading is tedious, and sometimes it breaks things. A new version can
rename a function or change what it expects to be given, and then the
project that calls it no longer builds. Someone has to read the error, find
out what changed, fix every place that used the old form, and check the
result.

Tools such as Dependabot and Renovate automate the first step. They notice
a new version and open a pull request, which is a proposed change for a
person to review, that updates the version number. When the upgrade breaks
the build they stop there, and the repair is left to a person. The repair
is where the time goes, so broken upgrades tend to sit unmerged.

An AI model can do that repair. It can read the error, read the new version
of the library, and rewrite the code that calls it. Letting it do so
creates a second problem, because the model is now acting on a real
project:

- To test its work it has to run code, and that includes the new
  dependency, which nobody on the project has read yet.
- The files it reads can contain text written to steer it, such as a
  comment telling it to ignore its instructions. This is called prompt
  injection.
- The fastest way to make a failing test pass is to weaken the test.
- It can report success when the work is not done.
- It can change far more than the task needs.
- Every question put to a model takes time and may cost money, and a model
  that goes in circles does not stop by itself.

## The approach

The model proposes and ordinary code decides. The model is never asked to
check its own work, and no rule depends on the model choosing to follow it.
Each concern above is met by a control in code:

| Concern | What repo-steward does |
|---|---|
| Unread code runs during testing | Builds and tests run in a container, which is an isolated environment, with no network, a read-only copy of the source, and no access to the operator's credentials. |
| Planted instructions | The model can act only through a fixed list of tools. None of them reaches the network or the operator's own copy of the project, and there is no tool that runs a command of the model's choosing. File contents reach the model labelled as data. |
| Weakened tests | The model cannot edit tests, CI configuration, security files, or the dependency list. The dependency list changes only through the upgrade step, which code checks. |
| False success | Success is decided by code. Build, vet, and tests run on the exact result and are compared with the same checks from before the upgrade. One new failure means no proposal. |
| Sweeping changes | The number of files and lines changed is limited. Crossing the first limit pauses the run for a person's approval, and crossing the second ends it. |
| Runaway time and cost | Steps, model calls, tokens, and spend are capped, and each cap is checked before the next request is sent. |
| Changes leaving the machine | All work happens in a copy. A push and a pull request need a person's approval, and that approval is tied to the exact proposal that person was shown. |

When a run cannot finish within these controls it stops and says why. A
stop with a reason is an expected result here. A wrong upgrade presented as
a good one is the result the design exists to prevent.

The controls reduce risk and do not remove it. The
[security document](docs/security.md) lists the risks that are accepted,
such as a flaw in the container engine itself.

## How a run goes

1. **Copy.** The project is cloned into a scratch workspace. The original
   is read once and never written.
2. **Check the starting point.** Build, vet, and tests run in the sandbox
   on the unchanged code. If the project already fails, the run stops,
   because a later failure could not be blamed on the upgrade.
3. **Find an upgrade.** Outdated dependencies are listed, and each newer
   version is marked eligible or not under the operator's policy.
4. **Apply it.** The Go toolchain makes the change on a staging copy of the
   dependency list. The result is admitted only if it changed what was
   asked and nothing else.
5. **Test again, and repair.** The same checks run on the upgraded code. If
   they fail, the model reads the errors and edits source files, and the
   checks run again.
6. **Prepare a proposal.** When the checks pass and every rule holds, the
   change is frozen as one commit in the scratch copy.
7. **Publish, if asked.** With `-publish` the run pauses. After a person
   approves, it pushes one branch and opens one pull request.

## Three ways to run it

| Mode | Who decides | What it is for |
|---|---|---|
| `baseline` | Fixed code, no model | Upgrades that need no repair. It is also the floor the model is measured against. |
| `scripted` | A fixed list of decisions | Testing the controls with the same result every time. It says nothing about how well a model repairs code. |
| `model` | An AI model | The real thing. Development uses local models through [Ollama](https://ollama.com), which cost nothing to run. A paid model runs only on request and under a spending cap. |

## Does it work

The benchmark is eleven small test projects, each with a known right
answer. In five of them a correct upgrade exists. In six the right answer
is to refuse, for example because the project was already failing before
the upgrade, or because the only fix would change a protected file.

| Mode | Correct upgrades | Correct refusals | Wrong result presented as success |
|---|---|---|---|
| baseline, no model | 2 of 5 | 3 of 6 | 0 of 11 |
| scripted | 5 of 5 | 6 of 6 | 0 of 11 |
| Claude Sonnet 5, one run each | 4 of 5 | 4 of 6 | 0 of 11 |
| qwen3:30b-a3b, local, two runs each | 8 of 10 | 10 of 12 | 0 of 22 |
| gpt-oss:20b, local, two runs each | 1 of 10 | 11 of 12 | 0 of 22 |

Across 77 runs no mode produced a false success or modified the operator's
copy of the project. The runs that did not reach the right answer ended at
a limit, ended on an error, or stopped without a proposal. The baseline has
no model and cannot repair anything, so on the nine scenarios it did not
complete it stopped without a proposal each time.

These results are narrow. The eleven projects are small and were written
for the purpose, the rows were measured on different dates, and the paid
model ran once per scenario. They show that the controls hold under a real
model's decisions. They do not show how well any model repairs real
projects. [benchmarks/README.md](benchmarks/README.md) has the scoring
rules, the dates, and the per-scenario results.

Five further scenarios run the no-model pipeline against real public
projects, see
[Smoke scenarios](#smoke-scenarios-against-public-modules). Three more ask
the local model to repair a real break in a real project; in its first
nine runs it repaired none, see
[Repair scenarios](#repair-scenarios-on-real-repositories). repo-steward
has also run once for real, see Status below.

## What it supports

One Go module per repository, without cgo, vendoring, workspaces,
submodules, or symlinks. One upgrade per run. Publication to GitHub. A
project outside these limits is refused at the start with the reason.

## Terms used on this page

| Term | Meaning |
|---|---|
| Dependency | A library written by someone else that the project uses. In Go a dependency is a module. |
| Manifest | The files that list a project's dependencies and their exact versions: `go.mod` and `go.sum`. |
| Agent | A program that lets an AI model choose actions one at a time, carries each one out, and shows the model the result. |
| Tool | One action the agent can request, such as reading a file or running the tests. |
| Policy | The rules, written as code, that decide whether a requested action runs. |
| Sandbox | The container where the project's code is built and tested, with no network. |
| Baseline | The build and test results of the unchanged project, used for comparison. Also the name of the mode that uses no model. |
| Proposal | The finished change, stored as one commit, ready for review. |
| Operator | The person running repo-steward, who grants or refuses approvals. |

## Status

The tagged release is v0.1.0, and `main` has moved on since: it now runs on
agent-runtime v0.3.2, and scores its benchmarks with agent-runtime's `bench`
module. The whole path described above exists and is tested.

- **Deterministic parts:** exact snapshots, the container sandbox,
  validation that treats an unclear result as a failure, discovery of
  candidate upgrades, and the baseline pipeline.
- **Agent parts:** scoped tools, policy, approvals, and resume on
  agent-runtime, a scripted agent for repeatable tests, and a model-driven
  agent.
- **Publication:** an approval bound to the proposal's hash, and a push and
  a pull request that are journaled, so an interrupted run is reconciled
  against GitHub and nothing is sent twice.
- **Evidence:** committed benchmark results over eleven scenarios, five
  smoke scenarios against real public modules, and three repair scenarios
  on real repositories, which the local model has not yet repaired.

No development, test, or CI path makes a paid model call. The one paid
provider exists for explicit measurements under a spending cap, see
[Benchmarks](#benchmarks).

Publication is tested against a fake GitHub server and a local bare
repository. It has also run once for real. On 2026-09-22 a model-mode run on
a private Go repository of the author's upgraded pgx from v5.7.4 to v5.7.5
and paused for approval. On resume it pushed one branch and opened one pull
request on github.com, changing `go.mod` and `go.sum` only.

Where to read more:

- [docs/architecture.md](docs/architecture.md): stages, evidence, sandbox profiles, runs and recovery.
- [docs/security.md](docs/security.md): assets, boundaries, every control with its status and test, accepted risks.
- [docs/status.md](docs/status.md): the milestone-by-milestone control table.
- [docs/decisions](docs/decisions): why it is built this way.
- [benchmarks/README.md](benchmarks/README.md): scoring and committed results.
- [Wiki](https://github.com/joeylking/repo-steward/wiki): the same system at length, for users, developers, and operators, with a glossary and the design decisions explained.

## Requirements

- Go 1.27 or later and Git.
- The runtime is pinned in `go.mod` at released versions: `agent-runtime
  v0.3.2`, the benchmark vocabulary at its nested module tag `bench/v0.1.1`,
  and the provider adapters at theirs, `providers/ollama/v0.2.0` and
  `providers/anthropic/v0.2.0`.
- A Docker-compatible engine reachable over a unix socket (Docker Desktop,
  OrbStack, Colima, or Rancher Desktop) for every command that builds or
  tests code: `inspect`, `maintain`, `resume`, and `bench run`. `fixture`
  and `snapshot` commands need no engine.
- With a VM-backed engine on macOS, the data directory and the repository
  must be under a shared path. Colima shares `$HOME` by default; the default
  data directory, `~/.local/share/repo-steward`, is under `$HOME` for that
  reason, and `-data-dir` names another. The sandbox probes every
  mount and fails hard if the engine cannot see it.

## Quick start

The quick start uses fixtures, which are small example projects that ship
with the tool, and an offline copy of their dependencies. Nothing in it
needs a model until the model-mode step, and nothing needs the network after
the image is pulled.

```sh
go test -race ./...

# Materialize two fixture repositories and the offline module proxy they
# resolve from. patch-safe needs no repair; breaking-minor needs one.
go run ./cmd/repo-steward fixture setup patch-safe -dest ~/tmp/patch-safe
go run ./cmd/repo-steward fixture setup breaking-minor -dest ~/tmp/breaking-minor
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

`maintain` exits 0 when a proposal was prepared or published, 2 when
unsupported, 3 on baseline problems, 5 when the run paused for an approval,
and 4 for any other explained non-result such as a regression introduced by
the upgrade or a run that reported itself blocked. Every command exits 1 on
an error, such as a bad flag or an unreachable engine.

```sh
# A paused run is decided and continued in separate processes. approve and
# reject print the approval to stderr first, its kind, tool, reason, expiry,
# arguments, and presentation, with model-chosen text escaped so it cannot
# move the cursor or imitate the prompt; the decision is then bound by hash to
# exactly what was printed, and refused if the stored approval differs.
go run ./cmd/repo-steward runs list
go run ./cmd/repo-steward runs show <run-id>
go run ./cmd/repo-steward approve <run-id> -note "small file"
go run ./cmd/repo-steward resume <run-id> -fixture-proxy ~/tmp/proxy

# The other two decisions. reject refuses the request and ends the run.
# cancel ends any run that has not finished, including one that was
# approved and never resumed.
go run ./cmd/repo-steward reject <run-id> -note "too broad"
go run ./cmd/repo-steward cancel <run-id> -note "no longer needed"

# A run whose process died mid-step is continued the same way: the
# interrupted step is recorded, journaled operations are reconciled, and
# the agent proceeds. No side effect is re-executed. repo-steward supplies
# its own reconciliation (manifest promotions, proposals, and publication
# operations are each journaled and recovered from that journal), so this
# is unchanged by the runtime's own interrupted_side_effect approval, which
# only pauses a run that has none.
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
output; the scratch checkout's HEAD is never moved.

Against a real repository omit `-fixture-proxy`. Dependency acquisition then
reaches the public module proxy and checksum database, and everything that
executes repository code still runs with no network.
`-dependency module@version` pins one exact target; only versions newer
than the current one are ever candidates, so a pin cannot downgrade.

## Data directory layout

Everything a run needs to be inspected or resumed lives under the data
directory: `steward.db` holds the runtime's runs, steps, approvals, and
events alongside the tasks, promotions, validation records, and proposals;
`runs/<id>/` holds the scratch clone, snapshots, and staging; `cache/mod/`
holds the module cache per module path. A run's options and candidate
facts are persisted at start so `resume` reconstructs the session under
the same configuration. Directories are never deleted and recreated at the
same path within a run, because VM-backed engines cache path lookups.

## What inspect does

`inspect` changes nothing. It reports whether a repository is supported,
what could be upgraded, and whether the unchanged code passes its checks.

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

This is the pipeline from [How a run goes](#how-a-run-goes) in full, without
the repair step. Gate A and Gate B are the two sets of rules that a change
to the manifests must pass before it is admitted.

1. Refuses a dirty source checkout, then clones it into a scratch workspace
   with a separate Git directory.
2. Profiles and validates the base tree exactly as `inspect` does.
3. Selects the smallest eligible upgrade: patch before minor before major,
   then the first module in alphabetical order that has a target in that
   class, then the highest version of that module in that class.
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
injection cases, and a model-call bound. The six scores are declared as an
outcome set for agent-runtime's `bench` module, which writes the result
files and renders the summaries: denominators are stated, completions are
never added to refusals, and one false success disqualifies a mode.
`bench summarize` compares the newest result per mode and model and refuses
results from different commits unless given `-mixed-commits`. Eleven scenarios cover a patch
upgrade, two repairs, a closure-driven regression, a failing baseline, an
ineligible major, a major beyond scope, injected instructions, a protected
change, a toolchain gap, and a hard scope limit. Results are committed under
`benchmarks/results/` as data. See [benchmarks/README.md](benchmarks/README.md).

## The agent path

In the scripted and model modes the runtime drives a decision loop. Each
decision is a request to use one tool. The runtime checks the request
against the tool's schema and passes it to the policy before anything
executes. The tools are the only capabilities the agent has; none can reach
the network, the operator's checkout, or a protected file. The class column
is the kind of effect the tool declares, which the policy reads:

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
| `report_blocked` | read, terminal | Ends the run with an explained non-result. |

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
prompt, the opening message, and the labelled tool-result format.

A reply that contains no executable tool call, or one cut off by the output
cap, is recorded as an invalid decision and answered on the next step with
a nudge naming what was wrong. It costs one step against the limits, and
the audit log shows it, instead of a second model call hidden inside one
step.

The system prompt is fixed and never contains repository content; files and
command output reach the model only inside tool results, labelled as data.
The runtime enforces call, token, and cost limits before every request and
records every attempt.

### Models and spend

The provider adapters are the runtime's nested modules `providers/ollama`
and `providers/anthropic`, so the transport classification, the price
tables, and the paid-model refusal are shared rather than rebuilt here.

Local models through Ollama are the development provider because they cost
nothing to iterate against. They are priced free, so that a free model and
a model with no known price stay distinguishable.

The one paid provider, `anthropic:<model>`, is used only for explicit,
budgeted measurements. Nothing in tests or CI constructs it. It refuses to
run without a key in `ANTHROPIC_API_KEY` or `CLAUDE_KEY`, and without a
known price and a `-max-cost-usd` cap. It charges cache writes at their
higher rate so the cap never undercounts. `bench run` adds
`-max-total-cost-usd` and stops before a run could exceed it. Both caps are
written as `5`, `4.41`, or `$0.50` and parsed by the runtime's dollar
parser, which refuses anything else.

## Integration tests

```sh
go test -tags integration -p 1 ./...
go test -tags faultinject ./internal/manifest/ ./internal/proposal/
```

Integration tests need the engine and the pinned image and never pull.
Packages run serially because `inspect` reaps every container carrying the
sandbox label. The fault-injection tests crash a child process at each
journaled point and recover in the parent; they need no engine.

## Smoke scenarios against public modules

`internal/smoke` holds five scenarios that clone a real public repository
at a pinned commit, upgrade one direct dependency to a pinned version
through the public module proxy and checksum database, and check the
outcome: two proposals (a patch upgrade in go-playground/validator, a minor
upgrade in labstack/echo), a pin to an older version that yields no candidate, a
target whose go directive exceeds the repository's pinned toolchain, and a
repository whose test suite reaches the network and therefore fails its
baseline in the sandbox. All five run the baseline pipeline, with no model,
so they exercise the sandbox, the gates, and validation on real code and
say nothing about repair; the repair scenarios below do. They need the network and the toolchain images
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

## Repair scenarios on real repositories

Three further scenarios, under `internal/smoke/testdata/repair`, pin a
public repository where upgrading one dependency breaks the build and a
small change to non-test source repairs it: two are mcp-go v0.29.0's change
of `CallToolRequest.Params.Arguments` to `any` (in mschneider82/mcp-openweather
and awsoremod/mcp), one is ollama v0.5.0's change of `GenerateRequest.Format`
to `json.RawMessage` (in allof-dev/dictionary). `TestRepair` first runs the
baseline pipeline, which must end `regressed`, then lets a local model try
the repair. A run that ends without a proposal passes and is logged, since
a failed repair is a result; a proposal must pass every check a correct one
would, including hidden oracles, or the test fails. The test needs a model,
so it is skipped unless `REPO_STEWARD_SMOKE_MODEL` names one, and only an
`ollama:` model is accepted; neither `TestSmoke` nor the smoke workflow runs
it. Only one scenario may run against an engine at a time, because each run
reaps every container carrying the sandbox label.

```sh
REPO_STEWARD_SMOKE_MODEL=ollama:qwen3:30b-a3b REPO_STEWARD_SMOKE_OUT=$HOME/tmp/repair \
  go test -tags smoke -count=1 -p 1 -v -timeout 2h -run TestRepair ./internal/smoke/
```

The first measurement, on 2026-10-03 with qwen3:30b-a3b, repaired nothing:
0 of 9 runs, each ending after three consecutive failed steps, with no edit
attempted and the checkout untouched. The results and their caveats are in
[benchmarks/README.md](benchmarks/README.md#repair-on-real-repositories).
Measuring them exposed a sandbox defect: the kill, log, and removal calls
shared one 30-second budget that started when the container was created,
so any command running longer than 30 seconds, such as acquisition over
the network, failed with its output unread. Each call now gets its own
budget, covered by a unit test.
