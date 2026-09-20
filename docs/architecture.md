# Architecture

Repo Steward performs one bounded dependency upgrade on a local Go module:
select a candidate, apply it, validate, repair within limits, and prepare
a proposal that a human approves before anything leaves the machine. It
runs on [agent-runtime](https://github.com/joeylking/agent-runtime), which
owns the decision loop and its controls; this repository owns everything
that knows what a repository is.

## Stages and who decides

| Stage | Owner | Package |
|---|---|---|
| Profile the base tree; refuse unsupported layouts | deterministic | `repo`, `snapshot` |
| Baseline validation in the sandbox | deterministic | `validate`, `sandbox` |
| Discover candidates with per-version eligibility | deterministic facts, operator policy | `deps` |
| Select a candidate | baseline: smallest delta; agent: the model, within eligibility | `steward`, `agent` |
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
Manifest changes are journaled with before and after hashes and recovered
from the journal alone. A proposal is a commit built from a persisted
recipe under its own ref; its record carries a hash over the whole body.

## Sandbox profiles

| Profile | Source | Module cache | Network | Runs repository code |
|---|---|---|---|---|
| acquire | read-only | writable | yes, proxy only by configuration | no |
| mutate | read-only, plus a writable staging copy of the manifests | writable | as acquire | no |
| execute | read-only | read-only | none, enforced by the engine | yes |

Containers are created with an empty environment, an unprivileged user,
dropped capabilities, a read-only root, tmpfs `/tmp`, resource limits, and
an in-container timeout. A probe fails hard when the engine cannot see the
host directories.

## Runs and recovery

A run's state lives in one SQLite file: the runtime's runs, steps,
approvals, model calls, and events, and this repository's tasks,
promotions, validation records, proposals, and publication operations.
Phase is derived from the journals, never stored. A run paused for
approval, or left mid-step by a crash, is continued by `resume`, which
rebuilds the session from the persisted options and facts, reconciles
journaled operations, and hands control back to the runtime.

## Modes

`baseline` runs the pipeline with no model and is the floor the agents are
measured against. `scripted` replays a declared decision list through the
full control path. `model` asks a model for each decision through the
runtime's accounting caller; local models through Ollama are the
development provider. `bench run` executes scenarios in any mode and
scores them against their declarations.
