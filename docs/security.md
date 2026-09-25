# Security

Repo Steward operates on source repositories, which are untrusted input,
and drives a model, whose output is untrusted input. This document lists
what is protected, where trust changes hands, which controls exist, and
which risks are accepted. Every control carries a status: **Required**
(planned, not implemented), **Implemented** (code exists), or **Verified**
(a named test exercises it). Nothing below claims more than its status.

## Assets, in priority order

1. The operator's credentials and host: model API keys, the GitHub token,
   SSH keys, the shell environment, and the container engine socket.
2. The destination repository: its branches and the truthfulness of what
   a pull request claims.
3. The operator's source checkout, which is never written.
4. Validation evidence: the module cache, snapshots, and proposal records.
5. Model spend.

## Trust boundaries

| Boundary | Trusted | Untrusted | What crosses |
|---|---|---|---|
| Operator to CLI | both | | flags, configuration, approvals |
| Host process to repository content | host code | every file | parsed manifests, file contents delivered to the model as data |
| Host to `execute` containers | host | repository and dependency code | read-only snapshot and cache in; exit code and output out |
| Host to `acquire` and `mutate` containers | host and the Go toolchain | manifests and the network | manifests in; cache and staging files out |
| Host to model | transport | model output | prompt with repository excerpts out; decisions in, validated and policed |
| Host to GitHub | transport and service | content | release notes and pull request bodies in as data; push and pull request out under approval |
| Host to data directory | both | | trusted as the operator's filesystem |

## Attacker-controlled inputs

Repository files including README, comments, test data, ignore rules, and
manifests; dependency source and its manifests; compiler and test output,
which quotes repository strings; model output; pull request bodies and
branch names read during reconciliation.

## Controls

| Threat | Control | Status | Reference |
|---|---|---|---|
| Injected instructions request exfiltration or remote action | The agent has no shell, no network tool, no file access outside the scratch clone; remote tools require a purpose-specific approval; policy is evaluated by the runtime with no model input | Verified | `policy` tests; scenario S7 in `benchmarks/results` |
| Injected instructions steer edits | Scope on the projected diff before a write, protected paths, budgets, and human review of the diff | Verified | `TestWrite_ScopeBeforeApply`, `TestWrite_ProtectedDenied` |
| Malicious test or build code executes | `execute` profile: no network, read-only snapshot and module cache, read-only root filesystem, empty environment, unprivileged user, capabilities dropped, cpu, memory, pids limits, in-container timeout | Verified (integration) | `TestExecute_NoNetwork`, `TestExecute_SourceAndCacheReadOnly`, `TestEnvironmentIsBuiltFromScratch`, `TestTimeoutKillsAndRemoves` |
| Container silently runs against nothing | Mount probe with unique markers fails hard when the engine cannot see host directories | Verified (integration) | `TestProbe_DetectsUnsharedDirectory` |
| Evidence drift | Validation runs on a tree materialized from raw objects and verified file by file, never on the working tree | Verified | `TestMaterialize_*`, `TestCandidateTree_IgnoredFileCannotAffectSnapshot` |
| Evidence poisoning through the build cache | None within a run; the cache is per run and discarded | Accepted | |
| Container escape | Nothing beyond dropped capabilities and no-new-privileges | Accepted | |
| Egress during acquisition | `GOPROXY` without direct fallback, `GOSUMDB` on, `GOPRIVATE` empty; no repository code runs in those profiles. Not OS-enforced | Implemented as configuration | `sandbox.Config.Validate`, `TestConfigValidate` |
| Manifest-driven execution or redirection | Replace, exclude, retract, go, and toolchain directives byte-identical to base at every gate; `GOTOOLCHAIN=local`; cgo, vendoring, workspaces, submodules, local replaces refused | Verified | `TestVerifyAdmission_Rules`, `TestInspect_Refusals` |
| Unauthorized dependency changes | Exact eligible target only, once per run; closure-bounded transitive increases; no reversions; direct dependencies preserved; tidy idempotence; `go mod verify`; target resolution | Verified | `TestVerifyAdmission_*`, `TestVerifyNormalized_*`, readiness in `TestBaseline_PatchSafeProducesVerifiedProposal` |
| Protected path written through an alias | Symlink components refused on every file tool, then lexical protected-path match | Verified | `TestWriteFile_Rules`, `TestReadTools_Containment` |
| Escape from the scratch clone | `os.Root` plus explicit traversal and `.git` refusal | Verified | `TestReadTools_Containment` |
| Git-level execution on the host | Separate git dir, hooks path isolated, global and system config disabled, submodule recursion off, fixed argv, no shell | Verified | `TestEnv_IgnoresUserConfiguration`, `TestHooksDoNotRun` |
| Silent modification of the operator's checkout | Read once at clone; dirty checkouts refused; every benchmark run asserts an unchanged status | Verified | `TestCreate_RefusesDirtySource`, `bench` side-effect check |
| False success claim | Readiness is code: bound validation, conclusive checks, introduced findings, manifest rules, protected paths, scope; evidence admissible only from completed steps; a frozen proposal verifies by hash, ref, tree, and parent | Verified | `TestReadiness_*` via integration, `TestVerify_DetectsTamperedBody`, `TestFreeze_Interruptions` |
| Proposal altered between review and publish | Publication approval bound to the proposal id and hash; on resume the proposal is re-verified and the working tree must still be the proposal tree | Verified (integration) | `TestPublish_RequiresProposalBoundApproval`, `TestCLI_PublicationAcrossProcesses` |
| Credential leakage | Containers get an empty environment; the push token lives only in a process-scoped variable read by a credential helper and never in arguments; the API token is a bearer header; nothing writes tokens to the store | Verified | `TestPushCommand_TokenOnlyInEnvironment`, `TestClient_TokenSentAsBearer`, `TestEnvironmentIsBuiltFromScratch` |
| Repository content sent to the model | Local models during development; the paid provider is constructed by no test or CI path, refuses to run without a key or a known price, and is used only for explicit budgeted measurements under a cost cap | Verified | `TestModelSpec_PaidProviderRefusals`, `TestModelSpec_LocalModelIsNamedAndFree`, `maintain` and `bench` refuse a paid provider without a cap; the adapters' own request and usage tests live in `agent-runtime/providers/anthropic` |
| A reply that asks for nothing executable | Recorded as an invalid decision with an `invalid_decision` observation and nudged on the next step, so it counts against the step and consecutive-failure limits and is visible in the audit log; a partial tool call from a truncated reply is never executed | Verified | `TestDecide_NoToolCallIsRecordedNotRetried`, `TestRender_InvalidStepIsNudged`, runtime `render.Decide` |
| Duplicate or wrong remote actions on recovery | Operations journaled before dispatch with markers; reconciliation reads remote state across ref and pull request states and never recreates a marked pull request | Verified | `TestReconcile_*`, `TestCLI_PublicationInterrupted*` |
| Concurrent writers | Exclusive OS lock per run and per data directory; no takeover, no heartbeat | Verified | `TestLock_SuspendedHolderBlocksSecondProcess`, `TestLock_KilledHolderReleases` |
| Resource exhaustion and spend | Container limits, output caps, timeouts, step, failure, loop, call, token, cost, and time limits, repair budgets | Verified | runtime limit tests, `TestValidate_Budgets` |
| Malicious dependency version | Checksum database verifies integrity, not intent; code runs only in `execute`; the human reviews the pull request | Accepted beyond sandboxing | |
| Local data directory tampering | Hashes catch corruption and code bugs only | Accepted | |
| Container engine socket access | None; the operator already holds it | Accepted | |

## What this project does not claim

Isolation sufficient for deliberately hostile repositories; OS-enforced
egress control during dependency acquisition; protection against a
compromised container runtime or kernel; protection of evidence from a
local user with write access to the data directory; confidentiality of
repository content from whichever model provider is configured.

## Operational notes

- VM-backed engines cache path lookups. No directory is deleted and
  recreated at the same path within a run, and probe markers are unique.
  Any new mount path must follow the same rule.
- The Go module cache under the data directory is created read-only by
  the toolchain; removing a data directory needs the permissions restored
  first.
- Model recordings are tied to the exact prompt and tool shapes. The
  runtime's renderer reproduces the previous renderer byte for byte, so the
  recordings committed before it replay unchanged.
- The Claude SDK reaches this module's dependency graph through
  `providers/anthropic`, which `internal/steward` imports for every mode.
  No development, test, or CI path constructs the adapter.
