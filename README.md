# repo-steward

An agentic repository maintenance tool built on
[agent-runtime](https://github.com/joeylking/agent-runtime). It performs one bounded dependency upgrade
on a local Go checkout, validates it in a sandbox, repairs breakage within
explicit scope limits, and prepares a pull request that a human approves before
anything leaves the machine.

## Status

Milestone 0 and batch 1A: fixtures, exact snapshots, container sandbox,
fail-closed validation, candidate discovery, the no-model `inspect` command,
and the deterministic `maintain -mode baseline` pipeline that stages an
upgrade through manifest gates, validates the exact candidate tree, and
freezes a proposal commit. No model calls, no GitHub. See
[docs/status.md](docs/status.md) for what is implemented and which test
verifies it.

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

`maintain` exits 0 when a proposal was prepared, 2 when unsupported, 3 on
baseline problems, and 4 for any other explained non-result such as a
regression introduced by the upgrade. The proposal commit lives under
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

## Integration tests

```sh
go test -tags integration -p 1 ./...
go test -tags faultinject ./internal/manifest/ ./internal/proposal/
```

Integration tests need the engine and the pinned image and never pull.
Packages run serially because `inspect` reaps every container carrying the
sandbox label. The fault-injection tests crash a child process at each
journaled point and recover in the parent; they need no engine.
