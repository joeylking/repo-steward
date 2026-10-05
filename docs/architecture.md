# Architecture

Repo Steward performs one bounded dependency upgrade on a local Go module:
select a candidate, apply it, validate, repair within limits, and prepare
a proposal that a human approves before anything leaves the machine. It
runs on [agent-runtime](https://github.com/joeylking/agent-runtime), which
owns the decision loop and its controls; this repository owns everything
that knows what a repository is.

The runtime also supplies the parts that are not about repositories: the
provider adapters (`providers/ollama` and `providers/anthropic`, nested
modules, so the Claude SDK is a dependency of the adapter and not of the
core), the shared provider helpers (transport classification, price lookup
that refuses an unpriced paid model, dollar parsing), the step-log to
message renderer and response mapping (`render`), the stderr trace observer
(`trace`), and the operator read models over `runs.db` (`view`: run and
step summaries and the pending-approval selection rule). Those were written
here first and are now deleted; what stayed is the domain text and the
joins with this repository's own task, proposal, and promotion tables.

Three more come from the runtime since v0.3.1. `approver` is the approval
channel `approve`, `reject`, and `cancel` go through: the approval is shown
in the sanitised text form of `approver/webhook` and decided bound to the
hash of what was shown. `bench`, a nested module with no dependencies, is
the result format and summarizer for the benchmark: repo-steward declares
its six scores as an outcome set and keeps the classifier and the scenarios.
`testkit` is what the policy and tool tests are written over: a conformance
table and never-assertions for the policy, and schema fuzzing for the tools.

## Stages and who decides

| Stage | Owner | Package |
|---|---|---|
| Profile the base tree; refuse unsupported layouts | deterministic | `repo`, `snapshot` |
| Baseline validation in the sandbox | deterministic | `validate`, `sandbox` |
| Discover candidates with per-version eligibility | deterministic facts, operator policy | `deps` |
| Select a candidate | baseline: smallest delta, or with `-select vulnerable` the lowest eligible upgrade that clears a scanned module's advisories; agent: the model, within eligibility | `steward`, `agent`, `deps` |
| Apply the upgrade through manifest staging and Gate A | deterministic | `manifest` |
| Repair source within scope | agent, policed | `tools`, `policy` |
| Normalize manifests through Gate B | deterministic | `manifest` |
| Validate the exact candidate tree | deterministic | `validate` |
| Readiness and a frozen proposal commit | deterministic | `proposal` |
| Publication approval, push, pull request, reconciliation | human, then deterministic | `publish`, `github` |

## Evidence

Every claim is bound to data the code can check. A candidate tree is
written from a temporary index seeded from the base tree and materialized
from raw objects with every blob verified. Validation records carry the
tree hash, a configuration hash, and the toolchain image digest, and are
admissible only when the runtime step that produced them completed.
Vulnerability scans are stored the same way, with the scanner's sha256 and
version, the database snapshot and its modified time, and the raw output,
and readiness admits a post scan only when it is bound to the candidate
tree and to the base scan's scanner and database.
Manifest changes are journaled with before and after hashes and recovered
from the journal alone. A proposal is a commit built from a persisted
recipe under its own ref; its record carries a hash over the whole body.

## Sandbox profiles

| Profile | Source | Module cache | Network | Runs repository code |
|---|---|---|---|---|
| acquire | read-only | writable | yes, proxy only by configuration | no |
| mutate | read-only, plus a writable staging copy of the manifests | writable | as acquire | no |
| execute | read-only | read-only | none, enforced by the engine | yes |
| tool | none | its own, fresh per build | yes, proxy.golang.org and sum.golang.org only by configuration | no; builds a pinned tool such as the scanner |

When a scan is configured, execute also mounts the scanner directory at
`/tools` and the vulnerability database at `/vulndb`, both read-only. No
container writes either: the host copies what the tool profile built.

Containers are created with an empty environment, an unprivileged user,
dropped capabilities, a read-only root, tmpfs `/tmp`, resource limits, and
an in-container timeout. A probe fails hard when the engine cannot see the
host directories.

## Vulnerability report

`vulns` (`inspect.Vulns`, with provisioning and the scan in `vulnscan`)
snapshots and profiles HEAD as `inspect` does, provisions the database and
the pinned scanner, verifies both on the host, scans in execute, and reports
findings split into third-party and standard library. It only reports;
`internal/vuln` holds the pins, the parser, and the database handling and
runs nothing.

`maintain -mode baseline -select vulnerable` acts on the same scan. The
baseline pipeline scans the base tree after a clean baseline, computes
targets with `deps.PlanVulnFixes` (a pure function of the findings, the
published versions, and the policy), and applies the first through the
unchanged gates, with Gate B also comparing the build lists the toolchain
selects (`manifest.VerifyBuildList`). After the post validation the
candidate is scanned with the same scanner and database, and readiness adds
`proposal.CheckScans`. The version cooldown (`Policy.MinAge`) is applied in
discovery from the toolchain's publish times and waived in this selection
for the version that clears an advisory. The agent modes do not select by
vulnerability.

## Runs and recovery

A run's state lives in one SQLite file: the runtime's runs, steps,
approvals, model calls, and events, and this repository's tasks,
promotions, validation records, proposals, and publication operations.
Phase is derived from the journals, never stored. A run paused for
approval, or left mid-step by a crash, is continued by `resume`, which
rebuilds the session from the persisted options and facts, reconciles
journaled operations, and hands control back to the runtime. The runtime
leases a RUNNING run to the process executing it; `resume` against a run
whose lease is still live, because that process is still working or
because it crashed and the lease has not yet expired (30 seconds by
default), is refused and reports who holds it and until when, rather than
racing it. Because repo-steward supplies `Config.Reconcile`, an
interrupted tool call is never replayed blindly: the runtime hands the
crash to the same journal-based reconciliation (manifest promotions,
proposals, publication operations) resume always used, not to its own
generic approval for an unreconciled interruption.

## Modes

`baseline` runs the pipeline with no model and is the floor the agents are
measured against. `scripted` replays a declared decision list through the
full control path. `model` asks a model for each decision through the
runtime's accounting caller, with the runtime's renderer building the
request and mapping the reply; local models through Ollama are the
development provider. A reply with no executable tool call is recorded as
an invalid decision and nudged on the next step, so it costs a step and
appears in the audit log. `bench run` executes scenarios in any mode and
scores them against their declarations, into result files in the format of
agent-runtime's `bench` module.
