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
| Host to the `tool` container | host, the Go toolchain, proxy.golang.org with sum.golang.org | | nothing from the repository in; the built scanner binary out, copied by the host |
| Vulnerability scanner and database to the host | the pinned scanner release and the snapshot as identified | the binary and database files on disk between provisioning and use, and the scanner's output | binary and database mounted read-only into `execute`; JSON findings out, parsed fail-closed |
| Host to model | transport | model output | prompt with repository excerpts out; decisions in, validated and policed |
| Host to GitHub | transport and service | content | release notes and pull request bodies in as data; push and pull request out under approval |
| Host to data directory | both | | trusted as the operator's filesystem |

## Attacker-controlled inputs

Repository files including README, comments, test data, ignore rules, and
manifests; dependency source and its manifests; compiler and test output,
which quotes repository strings; the vulnerability scanner's output, which
quotes repository symbols and file names; the vulnerability database's
text (identifiers, aliases, summaries), which anyone who can get an entry
published or tamper with a fetched snapshot before it is identified
controls; module publish times, which the module proxy reports; model output; pull request bodies and
branch names read during reconciliation.

## Controls

| Threat | Control | Status | Reference |
|---|---|---|---|
| Injected instructions request exfiltration or remote action | The agent has no shell, no network tool, no file access outside the scratch clone; remote tools require a purpose-specific approval; policy is evaluated by the runtime with no model input | Verified | `policy` tests, written as agent-runtime `testkit` tables over the real tool specs, with `testkit.Never` asserting that publishing is never allowed without its approval and a destructive tool never allowed in any phase; scenario S7 in `benchmarks/results` |
| Injected instructions steer edits | Scope on the projected diff before a write, protected paths, budgets, and human review of the diff. Both write tools, `write_file` and `edit_file`, are projected to the file's full content by one function before the policy sizes the diff, and written by one function | Verified | `TestWrite_ScopeBeforeApply`, `TestWrite_SingleExpansion`, `TestWrite_ProtectedDenied`, `TestEdit_ExactlyOneOccurrence`, `TestEdit_ScopeSeesProjectedContent`, `TestNever_WritesPastTheRules` |
| Malicious test or build code executes | `execute` profile: no network, read-only snapshot and module cache, read-only root filesystem, empty environment, unprivileged user, capabilities dropped, cpu, memory, pids limits, in-container timeout | Verified (integration) | `TestExecute_NoNetwork`, `TestExecute_SourceAndCacheReadOnly`, `TestEnvironmentIsBuiltFromScratch`, `TestTimeoutKillsAndRemoves` |
| Container silently runs against nothing | Mount probe with unique markers fails hard when the engine cannot see host directories | Verified (integration) | `TestProbe_DetectsUnsharedDirectory` |
| Evidence drift | Validation runs on a tree materialized from raw objects and verified file by file, never on the working tree | Verified | `TestMaterialize_*`, `TestCandidateTree_IgnoredFileCannotAffectSnapshot` |
| Evidence poisoning through the build cache | Every validation (baseline, post, each `run_validation`, `inspect`) and every readiness or tool command in the `execute` profile mounts a new, empty build cache with a random name; the `execute` profile refuses to run without one; `acquire` and `mutate` mount a separate cache that no `execute` container sees; all are removed at the end of the run. Within one validation build, vet, and test share a cache, so code that validation executes can still affect that validation's own result | Verified (integration) | `TestFreshBuildCache_IsolatesValidations`, `TestExecute_BuildCacheFreshPerValidation`, `TestBaseline_BuildCacheFreshPerValidation` and `TestScripted_BuildCacheFreshPerValidation` (a test that fails when its cache holds what an earlier validation wrote; both fail on the code before the change) |
| Container escape | Nothing beyond dropped capabilities and no-new-privileges | Accepted | |
| Egress during acquisition | `GOPROXY` without direct fallback, `GOSUMDB` on, `GOPRIVATE` empty; no repository code runs in those profiles. Not OS-enforced | Implemented as configuration | `sandbox.Config.Validate`, `TestConfigValidate` |
| Manifest-driven execution or redirection | Replace, exclude, retract, go, and toolchain directives byte-identical to base at every gate; `GOTOOLCHAIN=local`; cgo, vendoring, workspaces, submodules, local replaces refused | Verified | `TestVerifyAdmission_Rules`, `TestInspect_Refusals` |
| Unauthorized dependency changes | Exact eligible target only, once per run; closure-bounded transitive increases; no reversions; direct dependencies preserved; tidy idempotence; `go mod verify`; target resolution | Verified | `TestVerifyAdmission_*`, `TestVerifyNormalized_*`, readiness in `TestBaseline_PatchSafeProducesVerifiedProposal` |
| Protected path written through an alias | Symlink components refused on every file tool, then lexical protected-path match | Verified | `TestWriteFile_Rules`, `TestEditFile_Rules`, `TestReadTools_Containment` |
| Escape from the scratch clone | `os.Root` plus explicit traversal and `.git` refusal | Verified | `TestReadTools_Containment`, `TestEditFile_Rules` |
| Git-level execution on the host | Separate git dir, hooks path isolated, global and system config disabled, submodule recursion off, fixed argv, no shell | Verified | `TestEnv_IgnoresUserConfiguration`, `TestHooksDoNotRun` |
| Silent modification of the operator's checkout | Read once at clone; dirty checkouts refused; every benchmark run asserts an unchanged status | Verified | `TestCreate_RefusesDirtySource`, `bench` side-effect check |
| False success claim | Readiness is code: bound validation, conclusive checks, introduced findings, manifest rules, protected paths, scope; evidence admissible only from completed steps; a frozen proposal verifies by hash, ref, tree, and parent | Verified | `TestReadiness_*` via integration, `TestVerify_DetectsTamperedBody`, `TestFreeze_Interruptions` |
| A repair that builds, vets, and passes validation but that no test executes is presented as ready | Coverage rule in readiness: when the candidate changes anything beyond go.mod and go.sum, the post validation readiness binds (same tree, configuration hash, toolchain digest, completed step) must carry a coverage run of that snapshot (`go test -coverpkg=./... -covermode=set` in the execute profile, a fresh build cache, no network), and every coverage block that holds code on a changed line of a non-test `.go` file must have been executed. Changed package-level declarations, blank and dot imports, compiler and `//line` directives, removed or renamed functions, non-Go files, binary changes, files the profile does not mention, and any truncated, malformed, failed, or absent profile are not verified. Otherwise the proposal is not ready (`repair_not_exercised`, listing file:line ranges) and the run ends `repair_not_exercised`, exit 4. With `maintain -ask-unexercised` the same condition pauses `prepare_proposal` for an `unexercised_repair` approval whose capability names the tree and whose presentation shows the ranges and the patch, printed escaped; readiness accepts only the approval granted for that step and that tree, and the body then opens with a statement naming the approval. Manifest-only proposals are unaffected | Verified | `TestCheck_*`, `TestParseProfile`, `TestParseHunks` (rule and fail-closed parsing), `TestPrepare_UnexercisedRepair`, `TestNever_FreezesAnUnexercisedRepair` (the policy never allows `prepare_proposal` on such a tree, under any arguments or grants), `TestNotExercised`; `TestScripted_S2U_UnexercisedRepairIsNotReady`, `TestScripted_S2_BreakingMinorRepaired`, `TestScripted_S1_PatchUpgrade`, `TestCLI_UnexercisedRepairEndsTheRun`, `TestCLI_UnexercisedApprovalAcrossProcesses`, `TestCLI_UnexercisedRejected`, `TestCLI_UnexercisedApprovalBoundToTree` (integration) |
| Operator approves something other than what was shown, or is misled by model-chosen text in the approval | `approve` and `reject` go through the runtime's `approver`: the approval is printed with `webhook.Text`, which escapes model-chosen text and ends with a line it writes itself, and the decision carries the hash of what was printed and is refused if the stored approval differs | Implemented; the path is exercised (integration) | `runDecide` in `cmd/repo-steward`, `TestCLI_ScopeExpansionAcrossProcesses`, `TestCLI_RejectCancels`, CI's approval across processes; the hash refusal is tested upstream in `agent-runtime/approver` |
| Proposal altered between review and publish | Publication approval bound to the proposal id and hash; on resume the proposal is re-verified and the working tree must still be the proposal tree | Verified (integration) | `TestPublish_RequiresProposalBoundApproval`, `TestCLI_PublicationAcrossProcesses` |
| Credential leakage | Containers get an empty environment; the push token lives only in a process-scoped variable read by a credential helper and never in arguments; the API token is a bearer header; nothing writes tokens to the store | Verified | `TestPushCommand_TokenOnlyInEnvironment`, `TestClient_TokenSentAsBearer`, `TestEnvironmentIsBuiltFromScratch` |
| Repository content sent to the model | Local models during development; the paid provider is constructed by no test or CI path, refuses to run without a key or a known price, and is used only for explicit budgeted measurements under a cost cap | Verified | `TestModelSpec_PaidProviderRefusals`, `TestModelSpec_LocalModelIsNamedAndFree`, `maintain` and `bench` refuse a paid provider without a cap; the adapters' own request and usage tests live in `agent-runtime/providers/anthropic` |
| A reply that asks for nothing executable | Recorded as an invalid decision with an `invalid_decision` observation and nudged on the next step, so it counts against the step and consecutive-failure limits and is visible in the audit log; a partial tool call from a truncated reply is never executed | Verified | `TestDecide_NoToolCallIsRecordedNotRetried`, `TestRender_InvalidStepIsNudged`, runtime `render.Decide` |
| Duplicate or wrong remote actions on recovery | Operations journaled before dispatch with markers; reconciliation reads remote state across ref and pull request states and never recreates a marked pull request | Verified | `TestReconcile_*`, `TestCLI_PublicationInterrupted*` |
| Concurrent writers | Exclusive OS lock per run and per data directory; no takeover, no heartbeat. The runtime database adds its own lease on a RUNNING run, released by the kernel's flock on exit or not: a resume after a crash still waits out that lease (agentrt.DefaultLeaseTTL, 30 seconds by default) before the agent continues, which `repo-steward resume` reports plainly | Verified | `TestLock_SuspendedHolderBlocksSecondProcess`, `TestLock_KilledHolderReleases`, `TestCLI_InterruptedRunResumes`, `TestCLI_PublicationInterruptedIsReconciled` |
| Resource exhaustion and spend | Container limits, output caps, timeouts, step, failure, loop, call, token, cost, and time limits, repair budgets | Verified | runtime limit tests, `TestValidate_Budgets` |
| Malicious dependency version | Checksum database verifies integrity, not intent; code runs only in `execute`; the human reviews the pull request | Accepted beyond sandboxing | |
| Another local account writes the world-writable caches or staging directories | The module cache, build caches, and staging directories are mode 0777 for the container user, so the data directory above them is kept owner-only: created 0700, tightened to 0700 when group or others can only read or enter it, and refused when they can write it or another account owns it (`lock.ErrInsecureDir`, one sentence naming the path, its mode, and the fix); the build cache and staging roots, and newly created parents of the module cache, are 0700 as well. The engine resolves bind mounts itself, so the container user still reaches the caches below an owner-only parent | Verified for the modes; the mount below an 0700 parent verified (integration) on colima and on native Linux Docker | `TestPrivate_CreatesTightensOrRefuses`, `TestFreshBuildCache_IsolatesValidations`; the data directory's mode after a run in `TestBaseline_BuildCacheFreshPerValidation` and `TestScripted_BuildCacheFreshPerValidation`; the mount in `TestExecute_BuildCacheFreshPerValidation` (integration). On native Linux Docker the daemon resolves the path as root and the container user needs access only to the mounted directory itself; CI's integration job is the proof there |
| Local data directory tampering | Hashes catch corruption and code bugs only | Accepted | |
| `steward.db` readable or writable by another account | Created owner-only (0600) by whichever of the runtime store or repo-steward's own task store opens it first; an existing database or WAL sidecar that group or others can write, or a WAL file owned by another account, is refused until fixed (`agentrt.ErrInsecureMode`), reported as one sentence naming the file, its mode, and the fix | Verified | upstream `agent-runtime` (`store_test.go`); `task.Open`'s `createOwnerOnly` |
| Container engine socket access | None; the operator already holds it | Accepted | |
| A scanner binary other than the pinned release is used | Built per image digest and pinned version in the `tool` profile from proxy.golang.org, checked against sum.golang.org; the host copies it into the data directory with its sha256 and build information recorded; before every scan the binary's sha256 must equal the record and its build information must match the pin (version, module sum, the image's exact Go version, linux, `-trimpath`, CGO_ENABLED=0, nothing replaced); a mismatch is an error, nothing is scanned or rebuilt | Verified | `TestEnsureScanner_MismatchIsRefusedNotRebuilt`, `TestEnsureScanner_BadBuildPersistsNothing`, `TestScan_TamperedInputsRefusedBeforeScan`, `TestVulns_TamperedScannerRefused` (integration), `TestCheck_Mismatches` |
| A database other than the identified snapshot is read | Fetched archives refused on any entry outside the fixed layout, links, oversize entries, or inconsistent indexes; snapshots content-addressed and reused only when they verify; a `-vulndb` directory is identified and checked the same way and never written; the snapshot's content hash and modified time are verified immediately before every scan, and the scanner's own report of the database and its modified time must match | Verified | `TestFetch_Refusals`, `TestFetch_RefusesTamperedExistingSnapshot`, `TestIdentify_Refusals`, `TestVerify_DetectsChanges`, `TestLocalDatabase`, `TestScan_TamperedInputsRefusedBeforeScan`, `TestVulns_MalformedDatabaseRefused` (integration), `TestVulns_PublicDatabase` (integration, network) |
| Repository or dependency code changes the scanner or the database | Only `execute` mounts them, read-only; `acquire` and `mutate` do not mount them; the persistent copies are written by the host only | Verified (integration) | `TestScanMounts_ExecuteOnlyAndReadOnly`, `TestExecute_ToolsAndVulnDBReadOnly`, `TestScan_ExecuteCannotWriteScannerOrDatabase` |
| The scanner build is steered by the repository or the run's configuration | The `tool` profile mounts no source, not the repository's module cache, no configured tools or database directory, and fresh module, build, and output directories of its own; it always uses proxy.golang.org and sum.golang.org, including when the run uses a fixture proxy; same read-only root, dropped capabilities, unprivileged user, limits, and environment built from scratch as every profile. Network egress is not OS-enforced, as for `acquire` | Verified | `TestToolProfile_IsolatedFromTheRepository`, `TestTool_IsolatedProfile` (integration) |
| Advisory text steers or forges the proposal | In vulnerable selection the proposal body, which a person approves and which becomes the pull request body, quotes database identifiers, aliases, and summaries and scanned call paths only through one function: control and invisible characters escaped, newlines and whitespace runs collapsed to one space, backticks replaced, each value cut to a fixed length (64 runes for identifiers, at most five aliases, 200 for a summary, 300 for a call path) and placed inside a code span after a fixed label, so it cannot start a line, pose as one of the body's own lines or a commit trailer, or render as a mention, link, or HTML. A finding readiness did not see resolved is never listed as fixed. The model never sees this text | Verified | `TestAdvisorySection`, `TestUntrusted`; the rendered section in `TestVulnerable_PatchSafeFixesAdvisory` (integration) |
| A hijacked or rushed release is adopted before anyone notices | Version cooldown: an otherwise eligible version published less than `-min-age` ago (72 hours by default on `maintain` and `inspect`), or with no publish time in the toolchain's module metadata, is ineligible with the reason; 0 disables it. Vulnerable selection waives it for the version that clears an advisory, because waiting leaves a known hole open, and the result (`cooldown_waived`) and the proposal say so. The publish time is what the module proxy reports; a proxy that lies about it defeats the control | Verified | `TestCooldownReason`, `TestDiscover_Cooldown`, `TestDiscover_CooldownLeavesOldVersionsByteIdentical`; `TestCooldown_WaivedOnlyForTheAdvisoryFix` (integration, injected clock) |
| A proposal claims an advisory fixed on evidence that does not show it | Each scan is stored with its tree, configuration hash, image digest, scanner version and sha256, database snapshot and modified time, Go version, parsed result, and compressed raw output with its digest, checked on read. Readiness requires a conclusive post scan of the candidate tree bound to the base scan's scanner, database, Go version, configuration, and image; every targeted finding absent at every level and found version; and nothing introduced or escalated, standard library included. An inconclusive or unbound post scan fails all three rules | Verified | `TestCheckScans`, `TestScanRecordRoundTrip`; `TestVulnerable_PatchSafeFixesAdvisory`, `TestVulnerable_IntroducedAdvisoryIsNotReady` (integration) |
| An indirect upgrade is undone by tidy and the vulnerable version silently reselected | In vulnerable selection Gate B compares the build lists the toolchain selects before and after: the target must be selected at exactly its version and every other module that enters or moves up must be in the target's closure | Verified | `TestVerifyBuildList`; `TestBuildListGate_TidyDroppingAnIndirectTargetIsRefused`, `TestVulnerable_IndirectTarget` (integration) |
| A scan that did not complete is read as "no vulnerabilities" | The output is parsed fail-closed: timeout, truncation (the cap is raised to 64 MB per stream), non-zero exit, malformed or unknown messages, a config that does not name the expected scanner version, database, database modified time, and Go version, or no sign the analysis finished, make the scan inconclusive, which `vulns` reports with the reason and exit 3, and which ends `maintain -select vulnerable` as `scan_inconclusive`. One gap remains: a stream cut exactly at a message boundary after the analysis reported finishing cannot be detected, and can only drop findings, never add them | Verified, with the gap | `TestParse_ProcessFailures`, `TestParse_MalformedStreams`, `TestParse_EveryTruncation`, `TestParse_ExpectMismatches`, `TestVulnReport_InconclusiveExits3`, `TestVulns_InconclusiveExits3`, `TestVulnerable_BaseScanInconclusive` (integration) |

## What this project does not claim

That a proposal is a correct repair. Readiness proves the candidate tree
builds, vets, and passes the repository's own tests with nothing
introduced, and, for a source change, that those tests execute the changed
code; it does not prove that they check it. Covered code whose tests do
not tell a right result from a wrong one passes the coverage rule as
readily as a right repair, so the rule is a floor, not correctness. The
evidence comes from running the repository's own test code, which can
forge it as it can forge test results. An operator who approves an
unexercised repair with `-ask-unexercised` takes on what the tests did not
show. The human review of the diff remains the control for whether a
repair is right. On 2026-10-04 a model-mode run prepared a ready proposal
whose repair adds a panic the original code did not have, in a repository
with no tests; that outcome is now refused by the coverage rule
(`benchmarks/README.md`, "Repair on real repositories").

That a proposal from vulnerable selection fixes every vulnerability: it
clears the findings the scanner reports in one module, against the
database snapshot it names, and a module none of whose packages the
scanner loads is not reported at all. That the database is correct or
complete. That a standard-library finding is fixed: none is. Isolation
sufficient for deliberately hostile repositories; OS-enforced
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
- The scanner and database directories under the data directory are named
  by content (image digest and version; database modified time and hash)
  and are never deleted and recreated in a run. A scanner directory that
  fails verification is left for the operator to remove.
- Model recordings are tied to the exact prompt, tool shapes, and
  rendering. The 2026-10-04 revision changed all three, and the committed
  recordings were re-recorded with it.
- The Claude SDK reaches this module's dependency graph through
  `providers/anthropic`, which `internal/steward` imports for every mode.
  No development, test, or CI path constructs the adapter.
