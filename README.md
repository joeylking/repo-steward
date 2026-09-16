# repo-steward

An agentic repository maintenance tool built on
[agent-runtime](https://github.com/joeylking/agent-runtime). It performs one bounded dependency upgrade
on a local Go checkout, validates it in a sandbox, repairs breakage within
explicit scope limits, and prepares a pull request that a human approves before
anything leaves the machine.

## Status

Milestone 0 (0A and 0B): fixtures, exact snapshots, container sandbox,
fail-closed validation, candidate discovery, and the no-model `inspect`
command. No model calls, no GitHub. See [docs/status.md](docs/status.md) for
what is implemented and which test verifies it.

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
inconclusive. Against a real repository omit `-fixture-proxy`; dependency
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

## Integration tests

```sh
go test -tags integration -p 1 ./...
```

They need the engine and the pinned image and never pull. Packages run
serially because `inspect` reaps every container carrying the sandbox label.
