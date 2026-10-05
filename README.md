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
| False success | Success is decided by code. Build, vet, and tests run on the exact result and are compared with the same checks from before the upgrade. One new failure means no proposal. When the model changed source code, the project's own tests must also run every changed line; a repair no test runs is not proposed, unless the operator asked to be consulted and approves that exact change. Tests that run a line do not prove it right, so this is a floor, and the person reviewing the pull request still decides. |
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
   change is frozen as one commit in the scratch copy. If the model changed
   source code, one of the rules is that the project's tests ran every
   changed line.
7. **Publish, if asked.** With `-publish` the run pauses. After a person
   approves, it pushes one branch and opens one pull request.

A run can also start from a known security hole instead of from whatever is
outdated. With `-select vulnerable`, step 3 becomes: scan the project with
Go's own vulnerability checker against a verified copy of the Go
vulnerability database, and pick the smallest upgrade that removes the
reported holes in one dependency. After step 5 the upgraded code is scanned
again, and the proposal is made only if those holes are gone and no new one
appeared. Only the no-model mode does this so far, and holes in Go's
standard library are reported but not fixed, because fixing them means
changing the Go version itself.

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
| qwen3:30b-a3b, local, two runs each | 9 of 10 | 10 of 12 | 0 of 22 |
| Before the 2026-10-04 prompt revision: | | | |
| Claude Sonnet 5, one run each | 4 of 5 | 4 of 6 | 0 of 11 |
| qwen3:30b-a3b, local, two runs each | 8 of 10 | 10 of 12 | 0 of 22 |
| gpt-oss:20b, local, two runs each | 1 of 10 | 11 of 12 | 0 of 22 |

The first three rows are at the current prompt and tools; the last three
were measured before the revision and are kept as history, not as a
comparison. Across 121 runs no mode produced a false success on these
projects or modified the operator's copy of the project. The runs that did not reach the right answer ended at
a limit, ended on an error, or stopped without a proposal. The baseline has
no model and cannot repair anything, so on the nine scenarios it did not
complete it stopped without a proposal each time.

These results are narrow. The eleven projects are small and were written
for the purpose, the rows were measured on different dates, and the paid
model ran once per scenario. They show that the controls hold under a real
model's decisions. They do not show how well any model repairs real
projects. [benchmarks/README.md](benchmarks/README.md) has the scoring
rules, the dates, and the per-scenario results.

Six further scenarios run the no-model pipeline against real public
projects, one of them fixing a real advisory, see
[Smoke scenarios](#smoke-scenarios-against-public-modules). Three more ask
the local model to repair a real break in a real project. In its first
nine runs it repaired none. After a revision of the prompt and tools, one
run prepared a proposal that is not a correct repair: it compiles and
passes validation, in a project with no tests, but adds a crash the
original code did not have. repo-steward now also requires the project's
tests to run every line a repair changes; run again, the model made the
same wrong repair and the run ended without a proposal, see
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

The current release is v0.2.0. It runs on agent-runtime v0.4.0 and scores
its benchmarks with agent-runtime's `bench` module. The whole path described above exists and is tested.

- **Deterministic parts:** exact snapshots, the container sandbox,
  validation that treats an unclear result as a failure, discovery of
  candidate upgrades, and the baseline pipeline.
- **Agent parts:** scoped tools, policy, approvals, and resume on
  agent-runtime, a scripted agent for repeatable tests, and a model-driven
  agent.
- **Publication:** an approval bound to the proposal's hash, and a push and
  a pull request that are journaled, so an interrupted run is reconciled
  against GitHub and nothing is sent twice.
- **Evidence:** committed benchmark results over eleven scenarios, six
  smoke scenarios against real public modules, and three repair scenarios
  on real repositories, where the local model has not produced a correct
  repair: its one proposal, on 2026-10-04, passed validation but was
  wrong, and the coverage rule added since refuses it.
- **Vulnerabilities:** `vulns` scans a repository with a pinned
  govulncheck against a verified snapshot of the Go vulnerability database
  and reports what it finds. `maintain -mode baseline -select vulnerable`
  acts on a third-party finding: it picks the lowest eligible upgrade that
  clears it, direct or indirect, and proposes it only when a second scan
  shows it gone and nothing new. Only baseline mode does this; the agent
  modes refuse the flag, and standard-library findings are not fixable yet.
  See [What vulns does](#what-vulns-does) and
  [Vulnerable selection](#what-maintain-does-with--select-vulnerable).

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
  v0.4.0`, the benchmark vocabulary at its nested module tag `bench/v0.1.1`,
  and the provider adapters at theirs, `providers/ollama/v0.2.0` and
  `providers/anthropic/v0.2.0`.
- A Docker-compatible engine reachable over a unix socket (Docker Desktop,
  OrbStack, Colima, or Rancher Desktop) for every command that builds or
  tests code: `inspect`, `vulns`, `maintain`, `resume`, and `bench run`.
  `fixture` and `snapshot` commands need no engine.
- For `vulns` and `maintain -select vulnerable`, network access to
  proxy.golang.org and sum.golang.org the first time each toolchain image
  is used, to build the scanner, and to https://vuln.go.dev on every run
  unless `-vulndb` names a database directory.
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
# Vulnerability report: scan with the pinned govulncheck against a synthetic
# database that plants advisories in the fixture modules. Building the
# scanner needs proxy.golang.org once per toolchain image.
go build -o ~/tmp/repo-steward ./cmd/repo-steward
~/tmp/repo-steward fixture vulndb -dest ~/tmp/vulndb
~/tmp/repo-steward vulns ~/tmp/patch-safe -fixture-proxy ~/tmp/proxy -vulndb ~/tmp/vulndb
```

`vulns` prints a JSON report on stdout and a short summary on stderr, and
exits 0 when the scan is conclusive and finds nothing in third-party
dependencies, 2 when the repository is unsupported, 3 when the scan is
inconclusive, and 4 when third-party findings are present. Findings in the
standard library alone exit 0, because they follow the toolchain image and
repo-steward cannot act on them.

```sh
# Baseline maintenance: pick the smallest eligible upgrade, apply it under
# the manifest gates, validate the exact result, and freeze a proposal
# commit in a scratch clone. Nothing is pushed and the source is untouched.
go run ./cmd/repo-steward maintain ~/tmp/patch-safe -mode baseline \
  -author "Your Name <you@example.com>" -fixture-proxy ~/tmp/proxy
```

```sh
# Vulnerable selection: scan patch-safe against the fixture database, pick
# lib v1.2.4, the lowest version that clears GO-TEST-0001, and propose it
# only if a scan of the upgraded tree shows the advisory gone and nothing
# new. The proposal body names the advisory.
~/tmp/repo-steward maintain ~/tmp/patch-safe -mode baseline -select vulnerable \
  -vulndb ~/tmp/vulndb -author "Your Name <you@example.com>" -fixture-proxy ~/tmp/proxy
```

```sh
# Scripted agent mode: replay an embedded scenario through the runtime,
# the full tool set, and the policy. -trace prints every runtime event.
go run ./cmd/repo-steward maintain ~/tmp/breaking-minor -mode scripted -scenario S2 \
  -author "Your Name <you@example.com>" -fixture-proxy ~/tmp/proxy -trace
```

`maintain` exits 0 when a proposal was prepared or published, 2 when
unsupported, 3 on baseline problems, 5 when the run paused for an approval,
6 when the upgrade could not be fetched because the module proxy or checksum
database was unreachable or failed (outcome `acquisition_failed`, with the
toolchain's message in the detail; a later run may succeed), and 4 for any
other explained non-result such as a regression introduced by the upgrade,
a run that reported itself blocked, or `repair_not_exercised`, a repair
that passed validation but that the project's tests do not run (see
[Repairs no test exercises](#repairs-no-test-exercises)). The outcomes of vulnerable
selection, `scan_inconclusive`, `no_vulnerabilities`, and
`no_fix_available`, are explained non-results and exit 4 too. Every command
exits 1 on an error, such as a bad flag, an unreachable engine, or
`-select vulnerable` with `-mode scripted` or `-mode model`.

`maintain` and `inspect` apply a version cooldown, `-min-age`, 72 hours by
default: a version published less than that long ago, or whose publish time
the module proxy does not give, is not eligible, and its candidate entry
says why. This is a change in behaviour from releases before it; `-min-age
0` turns it off. The fixtures' versions are all from 2023, so no fixture
outcome changes. In vulnerable selection the cooldown is waived for the
version that clears an advisory, and the result and the proposal say so.

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

`-ask-unexercised` (agent modes, off by default) turns a repair that no
test runs from an ended run into a pause for approval, exit 5, decided with
`approve` or `reject` like any other; see
[Repairs no test exercises](#repairs-no-test-exercises).

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
events alongside the tasks, promotions, validation records, vulnerability
scans, and proposals;
`runs/<id>/` holds the scratch clone, snapshots, and staging; `cache/mod/`
holds the module cache per module path; `tools/` holds the scanner built
for each toolchain image, and `vulndb/` the fetched vulnerability database
snapshots, each in a directory named by its content. A run's options and candidate
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

## What vulns does

`vulns` changes nothing. It reports the known vulnerabilities that affect a
repository at HEAD, and stops there. Acting on them is `maintain -select
vulnerable`, below.

1. Snapshots and profiles HEAD exactly as `inspect` does; an unsupported
   repository is refused before any network or container use.
2. Provisions the database: by default it downloads `vulndb.zip` from
   https://vuln.go.dev, validates and extracts it into
   `vulndb/vulndb-<modified>-<hash>/` under the data directory, and reuses
   that directory while the database is unchanged. `-vulndb DIR` uses an
   existing directory instead, which is identified and checked but never
   fetched or written. `fixture vulndb -dest DIR` writes the synthetic
   database for the fixture modules.
3. Starts the sandbox and populates the module cache (acquire profile).
4. Provisions the scanner: govulncheck at the version pinned for the
   image's Go minor, built once per image digest in the tool profile, which
   sees no repository source and none of its caches and always fetches from
   proxy.golang.org with sum.golang.org, even with `-fixture-proxy`. The
   host copies the binary to `tools/govulncheck-<version>-<image digest>/`
   with a record of its sha256 and build information.
5. Immediately before the scan, checks the scanner on the host (sha256 as
   recorded, version, module sum, the image's exact Go version, linux,
   `-trimpath`, no cgo, no replaced modules) and the database (content hash
   and modified time as identified). Any mismatch is an error and nothing
   is scanned or rebuilt; remove the scanner's directory to rebuild it.
6. Runs the scanner in the execute profile, with no network, read-only
   source and module cache, a fresh build cache, and the scanner and the
   database mounted read-only, and parses its output fail-closed: anything
   unexpected makes the scan inconclusive, and an inconclusive scan says
   nothing about vulnerabilities.

The report splits findings into third-party dependencies and the standard
library. Each finding has its advisory id and aliases, summary, module,
found version, fixed version or none, the level the scanner reached
(module, package, or symbol), and for symbol-level findings one call path.
Standard-library findings follow the toolchain image's Go version, not the
repository's dependencies; repo-steward never changes the go directive, so
it cannot fix them today and lists them for information only. The report
also names the scanner version and sha256, the database snapshot id and
modified time, the image, the tree hash, and the Go version.

## What maintain does in baseline mode

This is the pipeline from [How a run goes](#how-a-run-goes) in full, without
the repair step. Gate A and Gate B are the two sets of rules that a change
to the manifests must pass before it is admitted.

1. Refuses a dirty source checkout, then clones it into a scratch workspace
   with a separate Git directory.
2. Profiles and validates the base tree exactly as `inspect` does.
3. Selects the smallest eligible upgrade: patch before minor before major,
   then the first module in alphabetical order that has a target in that
   class, then the highest version of that module in that class. Eligible
   means allowed by the policy and, unless `-min-age 0`, published at least
   `-min-age` ago (72 hours by default) with a known publish time.
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

## What maintain does with -select vulnerable

`-select vulnerable` replaces step 3 of baseline mode and adds a scan after
step 6. Everything else, the gates, validation, readiness, and the frozen
proposal, is the same pipeline. Flags: `-vulndb DIR` uses a database
directory instead of fetching https://vuln.go.dev, and `-scan-timeout`
bounds each scan (10 minutes by default). Baseline mode is never resumed,
so nothing about the selection is persisted for `resume`; the selection is
part of the run's configuration hash, so its evidence cannot be mistaken
for a default run's.

1. Before any container starts, the database is identified or fetched as
   for `vulns`.
2. After the baseline validation is clean and conclusive, the scanner is
   provisioned and the base tree is scanned in the execute profile with a
   fresh build cache, the scanner and the database verified on the host
   first. The scan is stored in the task store with the tree, configuration
   hash, image digest, scanner version and sha256, database snapshot id and
   modified time, Go version, the parsed scan, and the raw output
   compressed with its sha256.
3. An inconclusive scan ends the run `scan_inconclusive` with the reason;
   it is never read as "no vulnerabilities". A conclusive scan with no
   third-party findings ends it `no_vulnerabilities`; standard-library
   findings are listed in the result but not acted on, because
   repo-steward never changes the go directive.
4. Targets are computed from the findings: per module, the needed version
   is the highest fixed version among its findings, and the target is the
   lowest published version at or above it that the policy allows (no
   pre-release or pseudo-version, the patch, minor, and major allowances,
   the deny list, and a `-dependency` pin, which must name that module and
   clear the findings). A finding with no fix leaves the module targetable
   for the others. Modules are ordered by the highest reachability level
   among their fixable findings (symbol, package, module), then direct
   before indirect, then path; the first is applied and the rest are listed
   as remaining. A module the main module requires only indirectly is a
   candidate here, unlike in the default selection. When nothing is
   eligible the run ends `no_fix_available` with a reason per finding.
5. Gate B also checks the build list the toolchain selects after tidy: the
   target at exactly its version, and every module that enters or moves up
   in the target's closure. A tidy that drops an indirect requirement and
   so reselects the vulnerable version is refused.
6. After the post validation is clean, the candidate tree is scanned with
   the same scanner and database and stored the same way.
7. Readiness adds three rules, each with its own failure code: a
   conclusive post scan bound to the candidate tree and to the base scan's
   configuration, image, scanner, database snapshot, and Go version
   (`scan_not_bound`, `scan_inconclusive`); every targeted finding gone at
   every level and found version (`advisory_not_resolved`); and nothing
   introduced or escalated, standard library included
   (`advisory_introduced`). Without a usable post scan all three fail.
8. The proposal body gains a section naming each fixed advisory with its
   aliases, summary, reachability and call path at base, and found and
   fixed versions; whether the dependency is direct or indirect; the
   third-party findings that remain and why; the count of
   standard-library findings and why they are not acted on; the scanner
   version; and the database snapshot and its modified time. Text from the
   database is untrusted: it is sanitized, cut to a fixed length, and kept
   on one line inside a code span, so it cannot pose as a line of the body,
   a commit trailer, or a mention or link in the pull request.

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
| `read_file`, `list_directory`, `search_files`, `git_diff` | read | Working tree only, except that `search_files` can search a dependency's source; symlink components and traversal refused. Files come in numbered windows of whole lines; a missing path is answered with the nearest existing directory's entries. |
| `read_dependency_source` | read | Files of a module version from the module cache. |
| `apply_upgrade` | local | Exact eligible target only, once per run, through manifest staging and Gate A. |
| `write_file` | local | Regular source files only; tests, CI, security, and manifest paths denied; ignored paths refused; scope checked on the projected diff before the write. |
| `edit_file` | local | Replaces one exact occurrence of a text in an existing file. Projected to the whole file and checked and written by the same code as `write_file`, so every rule above applies; zero or several occurrences are refused. |
| `normalize_manifests` | local | `go mod tidy` through staging and Gate B. |
| `run_validation` | read | Build, vet, test on the exact candidate tree; introduced findings relative to the baseline. |
| `prepare_proposal` | local | Readiness, then a frozen proposal commit. Terminal unless publication is enabled. A change to source must be executed by the repository's tests; with `-ask-unexercised` a repair that is not pauses here for an approval bound to its tree. |
| `publish_proposal` | remote, terminal | Only with `-publish`. Always requires a publication approval bound to the proposal's hash; verifies the proposal and the working tree again on resume; pushes the commit and opens the pull request as two journaled operations. |
| `report_blocked` | read, terminal | Ends the run with an explained non-result. |

The policy denies tools outside the current phase, checks eligibility on
the exact module and version, denies protected and ignored writes, aborts
on hard scope limits, asks for one scope expansion approval on soft limits,
aborts when the validation budget or the no-progress budget is spent, and,
with `-ask-unexercised`, asks for an approval before preparing a proposal
whose changed source no test executes.

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

### Repairs no test exercises

A repair can compile, pass `go vet`, and pass every test and still be
wrong, when no test runs the code it changed. On 2026-10-04 that happened:
in a repository with no tests, the local model replaced a map index with
an unchecked type assertion that panics on a call without arguments, and
the run ended with a ready proposal. Readiness now has a rule for it.

When the candidate changes anything beyond `go.mod` and `go.sum`, every
`run_validation` whose checks introduced nothing also runs the
repository's tests with coverage of every package of the module (`go test
-count=1 -covermode=set -coverpkg=./... ./...`), on the same snapshot, in
the execute profile with a build cache of its own and no network. The
profile comes back on standard output with a line count after it, so no
file a container wrote is read from the host, and a profile cut anywhere
is detected. It is stored with that validation, so it is bound to the
tree, the configuration, and the toolchain image exactly as the
validation is, and the model is not shown it there. On the fixtures it
added about 3.3 seconds to a 3.7-second validation.

Readiness then maps every changed line of a non-test `.go` file to the
profile's blocks:

- A line inside a function needs every block that contains its code to
  have run at least once. Braces count only on a line that has nothing
  else, so a changed `if` or `for` header needs the block that evaluates
  it, not the body it may skip. Code no block contains, such as a `case`
  label, needs every block of that function on the line. A changed
  signature needs the function's first block: the function was called.
- Comments, blank lines, the package clause, and ordinary imports need
  nothing; the compiler checks them.
- Not verified, whatever the profile says: a changed or removed `const`,
  `var`, or `type` at package level (its effect is wherever it is used), a
  blank or dot import (it runs package initialization), a compiler or
  `//line` directive, a function or method that no longer exists in its
  package (deleted or renamed), a file that is not Go source, a binary
  change, a file the profile does not mention (excluded by build tags, or
  compiled into no test binary), and a profile that is missing, truncated,
  malformed, from a failed run, or inconsistent with the file.
- Lines removed from a function that still exists need the blocks around
  the place they were removed from. Code moved to another place is judged
  where it now is. Generated files get no exemption. `_test.go` files are
  left to the protected-path rule.

If anything is not verified, the proposal is not ready, with the failure
`repair_not_exercised` listing `file:line` ranges and reasons. The model
sees it as `prepare_proposal`'s error, like any other readiness failure;
it cannot edit tests, so it can try a different repair or report blocked,
and the validation budget (eight cycles), the step limit, and the loop and
consecutive-failure limits bound that. A run that ends without a proposal
after that refusal, with the tree unchanged since, ends
`repair_not_exercised` (exit 4), the runtime's own outcome kept as
`run_outcome` in the detail. A proposal that passes the rule says so in
its body: `Changed source executed by tests: N of N required coverage
blocks`.

With `-ask-unexercised` the same condition pauses `prepare_proposal` for an
`unexercised_repair` approval instead (exit 5). Its capability names the
candidate tree; its presentation lists the ranges and shows the patch of
those files, printed escaped by `approve` and `reject` like every
approval. Approving it lets readiness pass for that tree only, and the
proposal body then opens with a line saying that no test exercises part of
the repair and naming the approval and who gave it. Rejecting it ends the
run with no proposal. A later edit is a new tree that no approval names.

What the rule does not catch: code that the tests run but do not check.
A test that calls the changed function without asserting what it returns
satisfies the rule as fully as a good test. It is a floor under a
proposal, not evidence that the repair is right.

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
sandbox label. The `internal/vulnscan` and `internal/steward` tests also
need the network: they build govulncheck once per test binary through
proxy.golang.org and sum.golang.org, and one of them fetches
https://vuln.go.dev. Without the
network they fail with a message saying so. The fault-injection tests crash a child process at each
journaled point and recover in the parent; they need no engine.

## Smoke scenarios against public modules

`internal/smoke` holds five scenarios that clone a real public repository
at a pinned commit, upgrade one direct dependency to a pinned version
through the public module proxy and checksum database, and check the
outcome: two proposals (a patch upgrade in go-playground/validator, a minor
upgrade in labstack/echo), a pin to an older version that yields no candidate, a
target whose go directive exceeds the repository's pinned toolchain, and a
repository whose test suite reaches the network and therefore fails its
baseline in the sandbox. A sixth, `TestSmokeVulnerable`, runs vulnerable
selection on labstack/echo v4.15.4 against the live database: it must find
GO-2026-5970 in `golang.org/x/text` v0.38.0, an indirect dependency, at
base, and propose v0.39.0, the lowest version that fixes it, with the
advisory gone and nothing introduced. It checks that advisory, not totals,
because the live database changes. All six run the baseline pipeline, with no model,
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

After the 2026-10-04 revision of the prompt and tools, mcp-openweather
ended with a ready proposal whose repair is wrong (an unchecked type
assertion that panics on a call without arguments), dictionary ended after
three malformed edit requests, and awsoremod was not run; see
[the 2026-10-04 measurement](benchmarks/README.md#2026-10-04-one-revision-of-the-prompt-and-tools).

Since 2026-10-05 a repair must be executed by the repository's tests
([Repairs no test exercises](#repairs-no-test-exercises)). None of the
three repositories has a test that runs the lines a repair changes, so a
correct repair of each now ends `repair_not_exercised` by default, and the
declarations say so; a proposal from any of them fails the test.
`TestRepair` reads a model-mode proposal's validation evidence from the
task store, and its oracles fail the known wrong repairs, proven on
hand-made trees by `TestRepairOracles`, which runs with no model and no
network. When a run ends without a proposal, the oracles judge the tree it
left and its diff is kept. Run once each on 2026-10-05: mcp-openweather
made the 2026-10-04 repair again and ended `repair_not_exercised` with no
proposal; dictionary repeated 2026-10-04; awsoremod chose the right repair
and failed to apply it with `edit_file`. See
[the 2026-10-05 measurement](benchmarks/README.md#2026-10-05-the-coverage-rule).

The first measurement, on 2026-10-03 with qwen3:30b-a3b, repaired nothing:
0 of 9 runs, each ending after three consecutive failed steps, with no edit
attempted and the checkout untouched. The results and their caveats are in
[benchmarks/README.md](benchmarks/README.md#repair-on-real-repositories).
Measuring them exposed a sandbox defect: the kill, log, and removal calls
shared one 30-second budget that started when the container was created,
so any command running longer than 30 seconds, such as acquisition over
the network, failed with its output unread. Each call now gets its own
budget, covered by a unit test.
