# Implementation status

Status values: **Required** (planned, not implemented), **Implemented** (code
exists, reference given), **Verified** (a named test exercises it). Tests
marked *integration* run under `-tags integration` against a real engine.
References to `agent-runtime/...` are to the released modules this
repository pins: `agent-runtime v0.3.2` and its nested `bench/v0.1.1`, with
the provider adapters at their nested tags `providers/ollama/v0.2.0` and
`providers/anthropic/v0.2.0`.

## Milestone 0A

| Control or capability | Status | Reference |
|---|---|---|
| Git runs with fixed argv, no shell, empty environment except PATH, global and system config disabled, hooks path isolated, submodule recursion off | Verified | `internal/gitx`, `TestEnv_IgnoresUserConfiguration`, `TestHooksDoNotRun` |
| Commit identity from an explicit recipe: tree, parents, author, committer, dates, exact message bytes | Verified | `gitx.CommitRecipe`, `TestCommitTree_DeterministicAndByteExact`, `TestSetup_IgnoresAmbientGitIdentity` |
| Fixtures embedded, generated into temporary Git repositories, ids pinned, drift detected | Verified | `internal/fixture`, `TestSetup_PinnedIDsAreReproducible`, `TestSetup_DriftIsDetected` |
| Candidate index seeded from the base tree before staging the working tree | Verified | `snapshot.BuildCandidateTree`, `TestCandidateTree_IgnoreRules` |
| Ignored files cannot enter or influence a candidate tree | Verified | `TestCandidateTree_IgnoredFileCannotAffectSnapshot` |
| NUL-delimited tree listing with sizes; size-aware raw blob parsing | Verified | `TestLsTreeAndCatFileBatch_SizeAware` |
| Materialization reproduces path, content, and mode from raw objects; no archive export | Verified | `TestMaterialize_ExportIgnoreIncluded`, `TestMaterialize_UnusualNamesModesAndContents` |
| Symlink and submodule entries refused | Verified | `TestMaterialize_RefusesSymlinkAndSubmodule`, `TestRun_TrackedSymlinkIsUnsupported` |
| Path rules, limits before any write, verification detects tampering, round trip to the same tree id | Verified | `TestCheck_PathRules`, `TestMaterialize_LimitsEnforcedBeforeWriting`, `TestVerify_DetectsTampering`, `TestMaterialize_FixtureRoundTrip` |

## Milestone 0B

| Control or capability | Status | Reference |
|---|---|---|
| File-based module proxy generated from embedded module sources; sums stable; fixture go.sum pinned and drift-checked | Verified | `internal/modproxy`, `TestBuild_LayoutAndDeterminism`, `TestFixtureGoSumsMatchProxy` |
| Exclusive OS lock: held by a suspended process, released by a killed one, no takeover | Verified | `internal/lock`, `TestLock_SuspendedHolderBlocksSecondProcess`, `TestLock_KilledHolderReleases` |
| One executing inspection per data directory (executor lock) | Verified | `TestRun_ExecutorLockIsExclusive` |
| Toolchain image pinned by digest, selected from the go directive | Verified | `internal/toolchain`, `TestForGoDirective`, `TestInspect_PatchSafeColdCache` (integration) |
| Sandbox configuration refuses direct proxy fallback and a disabled checksum database outside fixture mode | Verified | `TestConfigValidate` |
| Execute profile: no network interface, GOPROXY off | Verified (integration) | `TestExecute_NoNetwork` |
| Execute profile: source and module cache read-only, root filesystem read-only, tmp and build cache writable | Verified (integration) | `TestExecute_SourceAndCacheReadOnly` |
| Acquire profile: module cache writable, source read-only, fixture proxy mounted, no network in fixture mode | Verified (integration) | `TestAcquire_CacheWritableAndProxyMounted` |
| Container environment built from scratch; host variables absent; unprivileged user | Verified (integration) | `TestEnvironmentIsBuiltFromScratch` |
| Timeout kills and removes the container; output capped with truncation flags | Verified (integration) | `TestTimeoutKillsAndRemoves`, `TestOutputCapMarksTruncation` |
| Orphaned labelled containers reaped | Verified (integration) | `TestReapOrphans` |
| Mount probe fails hard when the engine cannot see host directories | Verified (integration) | `TestProbe_DetectsUnsharedDirectory` |
| Repository profile: module facts, refusals for workspace, submodules, vendoring, nested modules, cgo, local replace, unsupported go directive | Verified | `internal/repo`, `TestInspect_Refusals` |
| Validation fail-closed: timeout, truncated output, unparsed nonzero exit, exit-zero diagnostics, missing package terminal, exit mismatch, non-JSON | Verified | `internal/validate`, `TestBaseline_FailClosed` |
| Build, vet, and test findings with stable keys; manifest findings classified | Verified | `TestBaseline_BuildFailureParsed`, `TestBaseline_TestFailuresKeyed`, `TestBaseline_MissingGoSumIsManifestFinding` |
| Candidate discovery in the acquire profile; per-version eligibility from policy; pre-release and pseudo-versions ineligible; named dependency | Verified | `internal/deps`, `TestDiscover`, `TestTargets_PolicyAndNamedDependency` |
| Cold-cache inspect on fixtures: discovery, baseline clean and conclusive, evidence bound to tree and toolchain | Verified (integration) | `TestInspect_PatchSafeColdCache` |
| export-ignore test file executes in validation (S11) | Verified (integration) | `TestInspect_ExportIgnoreTestRuns` |
| Ignore rules honoured in the validated snapshot (S12) | Verified (integration) | `TestInspect_IgnoreRules` |
| Refusals decided without an engine | Verified | `TestRun_ProfileRefusalStopsBeforeSandbox` |

## Milestone 1, batch 1A

| Control or capability | Status | Reference |
|---|---|---|
| Scratch workspace: dirty source refused, separate Git directory, source never written, candidate tree and diff | Verified | `internal/workspace`, `TestCreate_SeparateGitDirAndBase`, `TestCreate_RefusesDirtySource`, `TestBaseline_*` (integration) |
| Workspace reads refuse symlink components and traversal | Verified | `TestReadFile_RefusesSymlinkComponents` |
| Protected-path matching | Verified | `repo.IsProtected`, `TestIsProtected` |
| Mutate profile: staging mounted read-write, source read-only, no network in fixture mode | Verified | `TestConfigValidate`, `TestBaseline_PatchSafeProducesVerifiedProposal` (integration) |
| Manifests changed only by the toolchain through `-modfile` staging | Verified (integration) | `manifest.Stage`, `TestBaseline_PatchSafeProducesVerifiedProposal` |
| Gate A rules: exact target, no reversion, closure-bounded increases, no direct add or remove, replace, exclude, retract, go, toolchain, module path unchanged | Verified | `TestVerifyAdmission_Rules`, `TestVerifyAdmission_PermittedTransitives`, `TestClosureFromGraph` |
| Gate B: Gate A plus direct dependency preserved, tidy idempotence, cache verification, target resolution | Verified | `TestVerifyNormalized_DirectBecameIndirect`, `proposal.Evaluate` via `TestBaseline_PatchSafeProducesVerifiedProposal` (integration) |
| Promotion journal with before and after hashes; recovery finishes, aborts, or conflicts from journal and files alone; staging not required | Verified | `TestPromotion_Interruptions` (faultinject, five points), `TestPromotion_UnexpectedStateConflicts` |
| Selection closed once an upgrade promotion is in flight | Verified | `TestPromotion_InFlightClosesSelection` |
| Validation evidence bound to tree, configuration hash, and toolchain digest; only accepted records count | Verified | `proposal.Evaluate`, `TestBaseline_PatchSafeProducesVerifiedProposal` (integration) |
| Regression refused: introduced findings block the proposal | Verified (integration) | `TestBaseline_BreakingMinorIsRegressedNotProposed` |
| Readiness evaluates every check and lists all failures | Implemented | `proposal.Evaluate` |
| Proposal commit from a persisted recipe: deterministic id, ambient Git identity ignored, ref updated, row frozen with hash; HEAD never moved | Verified | `TestFreeze_Interruptions` (faultinject, four points), `TestRecover_RefMovedIsInvalidated`, `TestVerify_DetectsTamperedBody` |
| Baseline selection order | Verified | `TestSelect` |
| Policy restricts selection; no mutation without a candidate | Verified (integration) | `TestBaseline_PolicyRestrictsSelection` |
| `requires_newer_toolchain` detection from toolchain output | Verified | `TestOpError_RequiresNewerToolchain` |
| Task, promotion, validation, and proposal persistence | Implemented | `internal/task` |

## Milestone 1, batch 1B

| Control or capability | Status | Reference |
|---|---|---|
| Runtime run keyed by the task id; steps, approvals, and events line up with the task tables | Verified (integration) | `steward.RunScripted`, `TestScripted_S1_PatchUpgrade` |
| Read tools contained: traversal, absolute paths, `.git`, and symlink components refused | Verified | `TestReadTools_Containment` |
| `write_file` denies protected, ignored, symlinked, traversing, and non-regular targets; writes atomically | Verified | `TestWriteFile_Rules` |
| `edit_file` (added 2026-10-04) replaces one exact occurrence of a text in an existing file. It is projected to the file's full content by the code `write_file` uses (`tools.ProjectWrite`) and written by the same function, so it is the same class, gated by the same phase, and refused for the same protected, ignored, symlinked, traversing, `.git`, root, and non-regular targets; an absent text or one occurring more than once is refused with the lines found, and nothing is written | Verified | `TestEditFile_Rules`, `TestEditFile_ReplacesExactlyOnce`, `TestWrite_ProtectedDenied`, `TestEdit_ExactlyOneOccurrence`, `TestPhaseGating` |
| Repository and dependency content returned with an untrusted-data notice | Implemented | `internal/tools` |
| Terminal tools: `prepare_proposal` and `report_blocked`; no remote or destructive tools exist | Verified | `TestSpecs_AreCompleteAndTerminalToolsMarked` |
| Phase derived from the promotion journal; tools gated by phase | Verified | `TestPhaseAndTarget_FromJournal`, `TestPhaseGating` |
| `apply_upgrade` allowed only for the exact eligible module and version, once per run | Verified | `TestApplyUpgrade_ExactEligibility`, `TestPhaseGating` |
| Scope computed on the projected diff before a write: hard limit aborts, soft limit asks once, expansion raises soft to hard, a second ask aborts, other approval kinds do not count. The same rows run for `write_file` and `edit_file`, and an edit is sized by the whole file it would leave | Verified | `TestWrite_ScopeBeforeApply`, `TestWrite_SingleExpansion`, `TestEdit_ScopeSeesProjectedContent`, `TestProjectedScope_CountsProposedWrite`, `TestProjectedScope_CountsProposedEdit` |
| Repair budgets: validation cycles and no-progress detection on introduced findings | Verified | `TestValidate_Budgets` |
| Policy tests are agent-runtime `testkit.CheckPolicy` tables over the real tool specs, so a request goes through the runtime's argument validation first; `testkit.Never`: in repair nothing read-only is denied, aborted, or held for approval, and in every phase and under every grant publishing is never allowed without approval and a destructive tool never allowed; outside repair neither write tool is ever allowed or held for approval, and in repair neither is when every path is protected or every projection is past the hard scope limit, under every grant | Verified | `internal/policy`, `TestNever`, `TestNever_WritesPastTheRules` |
| Tool schemas fuzzed: generated arguments that satisfy a schema reach the tool without a panic, near misses are refused before it (every tool but `run_validation`, which needs the engine) | Verified | `TestSpecs_FuzzedArguments` |
| Validation evidence produced by a tool is admissible only when its runtime step is done | Verified (integration) | `proposal.Inputs.StepDone`, `verifyProposal` in `scripted_integration_test.go` |
| S1 patch upgrade through the agent path | Verified (integration) | `TestScripted_S1_PatchUpgrade` |
| S2 breaking minor repaired in one source file; protected test unchanged | Verified (integration) | `TestScripted_S2_BreakingMinorRepaired` |
| S3 moved package repaired by import fix | Verified (integration) | `TestScripted_S3_MovedPackageRepaired` |
| S8 protected change: write denied by policy, run reports blocked, no proposal | Verified (integration) | `TestScripted_S8_ProtectedChangeBlocked` |
| S2E, outside the scored set: S2 repaired with two `edit_file` calls after a protected edit and an ambiguous one are each denied by policy and change nothing | Verified (integration) | `TestScripted_S2E_EditFileRepaired` |
| Interrupted run reconciliation hook runs promotion and freeze recovery | Implemented | `steward.runAgent` `Reconcile` |

## Milestone 1, batch 1C

| Control or capability | Status | Reference |
|---|---|---|
| Run options and candidate facts persisted at start; resume reconstructs the session under the same configuration hash | Verified (integration) | `task.SetTaskContext`, `steward.Resume`, `TestCLI_ScopeExpansionAcrossProcesses` |
| Resume refuses a run started with a fixture proxy unless the proxy is supplied again, and refuses a finished run | Verified (integration) | `TestCLI_ScopeExpansionAcrossProcesses`, `TestCLI_RejectCancels` |
| `approve` and `reject` decide the single pending approval from a separate process, through `approver.Open` on `steward.db`; each prints the approval with `webhook.Text` before deciding and is bound to the hash it printed; a second decision is refused | Verified (integration) | `runDecide`, `approver.Local.Approve` and `Reject`, `TestCLI_ScopeExpansionAcrossProcesses`, `TestCLI_RejectCancels` |
| Soft scope limit pauses the run with a `scope_expansion` approval whose presentation names the write; after approval and resume the write lands and the proposal includes it | Verified (integration) | `TestCLI_ScopeExpansionAcrossProcesses` |
| Hard scope limit ends the run without asking | Verified (integration) | `TestCLI_HardScopeLimitAborts` |
| Reject cancels the run, records the outcome, and the pending write never lands | Verified (integration) | `TestCLI_RejectCancels` |
| A process killed inside a tool leaves the run RUNNING with an executing step; resume records the step as interrupted, reconciles, and continues to a proposal | Verified (integration, fault-injected binary) | `TestCLI_InterruptedRunResumes` |
| `runs list` and `runs show` expose task, run, steps, approvals, proposals, and promotions | Verified (integration) | `TestCLI_*`; the runtime rows come from `agent-runtime/view` (`Summary`, `Steps`), the task, proposal, and promotion joins from this repository |
| Paths are never deleted and recreated within a run; probe markers carry a nonce | Verified (integration) | `workspace.Materialize`, `sandbox.Probe`, `TestCLI_ScopeExpansionAcrossProcesses` |

Milestone 1 is complete.

## Milestone 2, batch 2A

| Control or capability | Status | Reference |
|---|---|---|
| Ollama adapter over the native chat API: system, tool_use, and tool_result mapping, tool_calls parsed, usage reported, 5xx and connection failures transient, 4xx and body errors plain | Verified in the runtime | `agent-runtime/providers/ollama` (this repository's copy was deleted on 2026-09-25, when the runtime shipped it); the spec's name and free price here: `TestModelSpec_LocalModelIsNamedAndFree` |
| Model agent: fixed system prompt with no repository content; steps rendered as tool_use and tool_result pairs; older results elided; first tool use mapped to a decision; a reply with no executable tool call recorded as an invalid decision and nudged on the next step; limit errors propagated | Verified | `internal/agent` over `agent-runtime/render`, `TestRender_StepsBecomeToolUseAndResultPairs`, `TestDecide_MapsFirstToolUse`, `TestDecide_NoToolCallIsRecordedNotRetried`, `TestRender_InvalidStepIsNudged`, `TestDecide_PropagatesLimitErrors` |
| `-mode model` with persisted model spec so `resume` rebuilds the same agent; `-record` and `-replay` through the runtime's replay package | Implemented | `internal/steward/model.go` |
| Live local-model runs: patch-safe selects v1.2.4 and freezes a manifest-only proposal; breaking-minor reads the new signature, rewrites the call site, and freezes a proposal with main.go | Observed on 2026-09-17 with qwen3:30b-a3b, not a gated test | see README |

## Milestone 2, batch 2B

| Control or capability | Status | Reference |
|---|---|---|
| Tool results carry nothing run-specific, so a recorded model run replays | Verified (integration) | `internal/tools` `run_validation`, `TestModelMode_ReplaysRecordedRuns` |
| Model mode replays recorded responses with the model server unreachable, in tests and in CI | Verified (integration) | `internal/steward/testdata/recordings`, CI step "model mode from recordings" |
| Benchmark harness: fresh fixture per run, scores with explicit denominators, completions and refusals never combined, safe non-results distinct from incorrect refusals, side effects and policy stops counted | Verified | `internal/bench`, `TestScore_ClassesAreSeparate`, `TestSummary_DenominatorsAreExplicit`, `TestTaxonomy_IsValid` |
| Results in agent-runtime's `bench` format: the six scores declared as a `bench.Taxonomy` with false_success unsafe, files that refuse summaries not following from their trials, and `bench summarize` refusing results from different commits unless given `-mixed-commits` | Verified | `bench.Taxonomy` in `internal/bench`, `TestWrite_RoundTripsAndNamesByMode`, `TestComparison_RefusesMixedCommitsUnlessAllowed` |
| Results committed as data with the commit they were produced at; the files made before 2026-10-02 converted mechanically that day to the `bench` format, numbers unchanged | Implemented | `benchmarks/results/`, `benchmarks/README.md` |

Recordings are keyed by the exact request. Changing the system prompt, a
tool description or schema, or the shape of a tool result invalidates them;
re-record with `maintain -mode model -record` and commit the new files.
Moving onto the runtime's `render` package on 2026-09-25 did not invalidate
them: it reproduces the previous renderer byte for byte, and the recorded
qwen3 runs replay unchanged.

## Milestone 3

| Control or capability | Status | Reference |
|---|---|---|
| Minimal GitHub client: repository, branch head, ref head, commit existence, pull request create and list; token as a bearer header | Verified | `internal/github`, `TestClient_TokenSentAsBearer` |
| Fake GitHub server for every test, with failure injection and refs read from a bare repository | Implemented | `internal/github/fake` |
| Destination parsed from the remote, verified before any credential use and before the run directory exists: supported host, repository and base branch exist, base head equals the local base commit; detached HEAD refused | Verified | `publish.Verify`, `TestVerify`, `TestCLI_PublicationRefusedWhenBaseMismatch` (integration) |
| Push token only in a process-scoped environment variable read by a credential helper; never in arguments, files, or the store | Verified | `publish.PushCommand`, `TestPushCommand_TokenOnlyInEnvironment` |
| Publication approval bound to the frozen proposal's id and hash; a different or changed proposal cannot reuse it | Verified | `policy.evaluatePublish`, `TestPublish_RequiresProposalBoundApproval`, `TestCLI_PublicationAcrossProcesses` (integration) |
| Nothing reaches the destination before approval; rejection leaves the remote untouched | Verified (integration) | `TestCLI_PublicationAcrossProcesses`, `TestCLI_PublicationRejected` |
| On resume, the proposal is verified against the repository and the working tree must still be the proposal tree, else the run ends `proposal_invalidated` | Implemented | `tools.publishTool.Call` |
| Push and pull request as two journaled operations with markers, recorded before dispatch; base movement recorded and stated in the pull request body, never rebased | Verified | `publish.Publish`, `TestPublish_HappyPath`, `TestPublish_BaseMovedProceedsUnchanged` |
| Reconciliation from remote state: absent ref pushed again, ref at the commit accepted, different commit is a conflict; pull request found by marker across open, closed, and merged states and never recreated; foreign pull request is a conflict | Verified | `TestReconcile_*` |
| Interrupted publication resumed by reconciliation: after the pull request was created but unrecorded, and after the push but before the pull request; the run completes without another agent decision | Verified (integration, fault-injected binary) | `TestCLI_PublicationInterruptedIsReconciled`, `TestCLI_PublicationInterruptedBeforePRIsFinished` |
| Runtime reconciliation outcomes: continue, completed, waiting, conflict | Verified | agent-runtime `TestResume_ReconcileOutcomes` |
| `cancel` closes a run whose approval was granted but never resumed; both locks taken; resume then refused; nothing reaches the destination | Verified (integration) | `TestCLI_PublicationCancelledAfterApproval` |

Run against github.com on 2026-09-22: a model-mode run on a private Go
repository of the author's (qwen3:30b-a3b, pgx v5.7.4 to v5.7.5, go.mod and
go.sum only) paused for approval and on resume pushed the branch and opened
pull request #1 with the marker and validated base commit; the base branch
was untouched. The first attempt exposed that a destination parsed from the
origin remote was not persisted, so a resume in a new process could not
register the publish tool; fixed and covered by
`TestCLI_PublicationDestinationFromOriginAcrossProcesses`. The approval was
granted by the operator between two processes, as designed.

## Milestone 4

| Control or capability | Status | Reference |
|---|---|---|
| Scenario declarations carry acceptable outcomes, allowed and required files, hidden oracles, forbidden proposal text, scope and policy overrides, and a model-call bound; the harness scores from them | Verified | `internal/scenario`, `internal/bench`, `TestScore_ClassesAreSeparate` |
| Hidden oracles run against the proposal tree and are never visible to the agent; an oracle failure is a false success | Verified | `bench.oracles`, `TestScore_ClassesAreSeparate` |
| Candidate discovery from module version lists, so incompatible majors appear as ineligible candidates | Verified | `deps.Discover`, `TestDiscover` |
| Proxy generator serves +incompatible releases without a go.mod | Implemented | `internal/modproxy` |
| S4 major ineligible by default; S4M major allowed but the 22-file rename exceeds the hard scope limit | Scripted, scored | scenarios `S4`, `S4M`, fixture `major-v2` |
| S5 failing baseline: no mutation, no model call | Scripted, scored | scenario `S5`, fixture `baseline-failing` |
| S6 closure-driven break in an unrelated file repaired within scope | Scripted, scored | scenario `S6`, fixtures `closure-regression`, modules `core` and `util` |
| S7 injected instructions in README, comment, and test output; forbidden text checked in the proposal | Scripted, scored | scenario `S7`, fixture `injected` |
| S9 newer toolchain required; apply_upgrade reports it and the run is blocked | Scripted, scored | scenario `S9`, fixture `needs-toolchain`, module `needsgo` |
| S10H hard scope limit ends the run without an approval | Scripted, scored | scenario `S10H` |

Results for every mode over all eleven scenarios are committed under
`benchmarks/results/` and summarized in `benchmarks/README.md`. Not done:
pinned real-module smoke scenarios, which need network acquisition and are
reported separately when they exist.

## Smoke scenarios against public modules

| Control or capability | Status | Reference |
|---|---|---|
| Exact-version pin: `-dependency module@version` leaves only that version eligible; an older pin yields no candidate | Verified | `TestTargets_PolicyAndNamedDependency`, smoke `echo-pin-older` |
| Library-only modules build: `-o` is passed only when the listing contains a main package | Verified | `TestBaseline_BuildOutputOnlyForMainPackages`, smoke `echo-minor` |
| A `go.mod` under a directory the go tool ignores (`_x`, `.x`, `testdata`) is not a nested module | Verified | `TestInspect_IgnoredDirectoriesAreNotNestedModules`, smoke `validator-patch` |
| Patch and minor upgrades on real public modules through the public proxy and checksum database end in a frozen proposal changing go.mod and go.sum only, with the proposal commit on the pinned base and its tree equal to the validated tree | Verified (smoke, network) | `TestSmoke/validator-patch`, `TestSmoke/echo-minor` |
| A target declaring a newer go directive than the pinned image stops before admission as `requires_newer_toolchain` | Verified (smoke, network) | `TestSmoke/echo-requires-newer-toolchain` |
| A real test suite that reaches the network fails its baseline in the sandbox and the run stops before selection or mutation | Verified (smoke, network) | `TestSmoke/progressbar-baseline-failing` |

Pinned on 2026-09-24: labstack/echo v4.15.4, go-playground/validator
v10.30.5, schollz/progressbar v3.19.1. A scenario fails if its tag moves
off the pinned commit. The workflow is `.github/workflows/smoke.yml`,
on demand and weekly.

## Repair scenarios on real repositories

| Control or capability | Status | Reference |
|---|---|---|
| A sandbox command that runs longer than 30 seconds still has its output collected and its container removed: kill, log, and removal each get their own budget instead of one started at container creation | Verified | `sandbox.Docker.Run`, `TestRun_LongCommandStillCollectsLogsAndRemoves` |
| Repair scenarios declared like the smoke scenarios, with allowed and required files, hidden oracles, and a model-call cap; the break is proven by a baseline run ending `regressed` before the model runs | Implemented | `internal/smoke/testdata/repair`, `TestRepair` (smoke tag, skipped unless `REPO_STEWARD_SMOKE_MODEL` names an `ollama:` model; not in CI) |
| A proposal from a repair scenario is held to its file set, its oracles, clean conclusive post-validation bound to the proposal tree, the pinned parent, and an untouched checkout | Implemented, not yet exercised | `checkRepair` in `internal/smoke/repair_test.go`; no run has produced a proposal to check |
| Breaks confirmed on real code with the baseline pipeline: mcp-go v0.29.0 in mschneider82/mcp-openweather and awsoremod/mcp, ollama v0.5.0 in allof-dev/dictionary, each `regressed` after a clean no-network baseline | Observed on 2026-10-03 | `TestRepair` baseline step, `maintain -mode baseline` |
| Model-driven repair of those breaks by qwen3:30b-a3b | Not achieved: 0 of 9 runs (3 per scenario) repaired; all ended `limit_exhausted` after three consecutive failed steps, no edit attempted, checkout untouched | `benchmarks/README.md` "Repair on real repositories", `benchmarks/real-repair/2026-10-03/` |
| Recorded runs of these scenarios replay with the model server unreachable and network acquisition | Observed once each for two runs, not a gated test; recordings not committed | `benchmarks/real-repair/2026-10-03/*-replay-of-cli-1.json` |

## Before automatic upgrades

| Control or capability | Status | Reference |
|---|---|---|
| Every validation (baseline, post, each `run_validation`, `inspect`) and every readiness or tool command in the execute profile runs against a new, empty, randomly named build cache; the execute profile refuses to run without one; acquire and mutate use a separate cache execute never mounts; end-of-run cleanup removes them all | Verified (integration) | `sandbox.WithFreshBuildCache`, `TestFreshBuildCache_IsolatesValidations`, `TestExecute_BuildCacheFreshPerValidation`, `TestBaseline_BuildCacheFreshPerValidation`, `TestScripted_BuildCacheFreshPerValidation` |
| Data directory created 0700; one that group or others can only read or enter is tightened to 0700; one they can write, or that another account owns, is refused naming the path, its mode, and the fix; build cache and staging roots, and new parents of the module cache, created owner-only | Verified | `lock.Private`, `TestPrivate_CreatesTightensOrRefuses`, `TestFreshBuildCache_IsolatesValidations`; mode after a run in `TestBaseline_BuildCacheFreshPerValidation` (integration) |
| Containers still write world-writable caches below an owner-only parent | Verified (integration) on colima with virtiofs and on native Linux Docker by CI's integration job | `TestExecute_BuildCacheFreshPerValidation` |
| A `go get` or `go mod tidy` that fails because the module proxy or checksum database is unreachable (DNS, connection, timeout, TLS) or answers 5xx ends `acquisition_failed` with the toolchain's message, not `admission_refused` or `normalization_refused`; `apply_upgrade` aborts the run with the same outcome; `maintain` exits 6; anything not matched keeps its outcome | Verified | `OpError.AcquisitionFailed`, `TestOpError_AcquisitionFailed` (stderr captured from go 1.22 in the toolchain image), `TestStageOutcome_AcquisitionFailureIsNotARefusal`; not exercised by a full run against an unreachable proxy |

The fresh build caches cost time, because a later validation no longer
reuses what an earlier one compiled. Measured on 2026-10-03 on colima,
interleaving test binaries built before and after the change,
`TestBaseline_PatchSafeProducesVerifiedProposal` took 5.8 to 7.1 seconds
before (nine runs, median 6.0) and 9.2 to 9.8 seconds after (six runs,
median 9.3).

## Vulnerability report (VA-2)

This batch wires the vulnerability groundwork into the sandbox and adds a
report. The `vulns` command only reports; selecting and fixing an upgrade
by advisory, stored scan evidence, and the advisory text in a proposal came
with the next batch, below.

| Control or capability | Status | Reference |
|---|---|---|
| Execute profile mounts a tools directory at `/tools` and a database at `/vulndb`, both read-only, only when configured; every existing run mounts exactly what it did before; no other profile mounts them | Verified | `TestScanMounts_ExecuteOnlyAndReadOnly`, `TestConfigValidate`; `TestExecute_ToolsAndVulnDBReadOnly` (integration) |
| Tool profile for trusted builds: no source, not the repository's module cache, fresh module, build, and output directories under the run's build cache root, `/tools` writable as GOBIN, network on, proxy.golang.org and sum.golang.org always (fixture proxy ignored), same hardening as every profile | Verified | `TestToolProfile_IsolatedFromTheRepository`; `TestTool_IsolatedProfile` (integration) |
| Scanner built once per image digest and pinned version, copied out by the host into `tools/govulncheck-<version>-<digest>/` (binary 0555) with its identity recorded; reused afterwards without building | Verified | `TestEnsureScanner_BuildsOnceThenReuses`; `TestVulns_FixtureAdvisoryThenUpgradeRemovesIt` (integration, `built_this_run` false on the second run) |
| Before every scan the scanner binary is checked on the host against its recorded sha256 and the pin with the image's exact Go version; a mismatch is a hard error, never a rebuild | Verified | `TestEnsureScanner_MismatchIsRefusedNotRebuilt`, `TestEnsureScanner_BadBuildPersistsNothing`; `TestScan_TamperedInputsRefusedBeforeScan`, `TestVulns_TamperedScannerRefused` (integration) |
| Database fetched from https://vuln.go.dev into a content-addressed directory and reused while unchanged; `-vulndb DIR` identified and checked, never fetched; the snapshot verified immediately before every scan | Verified | `TestLocalDatabase`; `TestVulns_PublicDatabase` (integration, network), `TestVulns_MalformedDatabaseRefused`, `TestScan_TamperedInputsRefusedBeforeScan` (integration) |
| Scan in the execute profile with a fresh build cache, no network, a 64 MB output cap, a timeout, and every field of `Expect` filled (scanner version, `file:///vulndb`, database modified time, the Go version read in the same sandbox); the scanner and database must be visible in the container or the run fails | Verified (integration) | `vulnscan.Scan`, `TestScan_FixtureAdvisoryThroughMounts` |
| `vulns` reports GO-TEST-0001 at symbol level with its call path for patch-safe, and the standard-library advisory separately; after the baseline pipeline upgrades lib to v1.2.4 it is gone | Verified (integration) | `TestVulns_FixtureAdvisoryThenUpgradeRemovesIt`; CI step "vulnerability report on fixtures" |
| Report shape: findings split into third-party and standard library, sorted by module then id, aliases listed once; scanner version and sha256, database snapshot id and modified time, image, tree hash, Go version, conclusive and reason verbatim; a summary on stderr that says standard-library findings cannot be fixed today | Verified | `TestVulnReport_SplitsStdlibAndExits4`, `TestVulnReport_StdlibOnlyIsClean`, `TestVulnReport_AliasesListedOnce` |
| Exit codes: 0 conclusive with no third-party findings, 2 unsupported, 3 inconclusive, 4 third-party findings, 1 errors | Verified | `TestVulnReport_ExitCodes`, `TestVulnReport_InconclusiveExits3`, `TestVulns_UnsupportedStopsBeforeDatabaseAndSandbox`; `TestVulns_InconclusiveExits3` (integration) |
| An unsupported repository or a malformed `-vulndb` is refused before any container or download | Verified | `TestVulns_UnsupportedStopsBeforeDatabaseAndSandbox`, `TestVulns_BadDatabaseRefusedBeforeSandbox` |
| `fixture vulndb -dest DIR` writes the synthetic database | Implemented, run in CI | `cmd/repo-steward`, CI "quick start smoke" |
| On native Linux Docker the binary `go install` writes as the container user is readable by the host, so it can be inspected and copied; files the tool build leaves in its caches are removed through a container | Verified (integration) on colima and on native Linux Docker by CI's integration job | `TestVulns_FixtureAdvisoryThenUpgradeRemovesIt`, `TestTool_IsolatedProfile` (integration) |

Measured on 2026-10-04 on colima: a cold `vulns` run on patch-safe with the
fixture database took 11.9 seconds, 10.6 of them building the scanner; a
warm run took 1.0 second. Fetching the public database added about 1.2
seconds per run; on the public database of 2026-10-01, patch-safe has no
third-party findings and 52 standard-library findings for go1.22.12.

## Vulnerable selection (VB)

`maintain -mode baseline -select vulnerable` fixes a known vulnerability
end to end in the deterministic pipeline. Only baseline mode does this: the
scripted and model modes refuse the flag, and nothing the model sees has
changed. Standard-library findings are reported and never fixed.

| Control or capability | Status | Reference |
|---|---|---|
| `-select smallest` (default) and `vulnerable`; scripted and model modes refuse `vulnerable` with one line before creating anything; an unknown value is an error | Verified | `TestSelectVulnerable_RefusedInAgentModes`; `runMaintain` refuses it on the command line before resolving the author |
| The default selection's configuration hash, result JSON, readiness, and proposal body are what they were: every new field is omitted when empty and the selection enters the hash only when it is `vulnerable` | Verified | `TestConfigHash_SelectOnlyWhenVulnerable`; the existing baseline, scripted, replay, and CLI integration tests pass unchanged |
| Base scan after a clean, conclusive baseline, with a fresh build cache and the scanner and database verified first; inconclusive ends `scan_inconclusive` with the reason, never "no vulnerabilities" | Verified (integration) | `TestVulnerable_BaseScanInconclusive` |
| No third-party findings end `no_vulnerabilities`, standard-library findings listed with the reason they are not acted on | Verified (integration) | `TestVulnerable_NoVulnerabilities` |
| Target computation: highest fixed version per module, lowest eligible published version at or above it, policy (pre-release, pseudo-version, patch/minor/major, deny list, `-dependency` pin naming the module and clearing the findings), findings with no fix leaving the module targetable for the others, order by level then direct then path, indirect modules included, a reason per finding when nothing is eligible | Verified | `TestPlanVulnFixes`, `TestPlanVulnFixes_TargetDetail`, `TestPublishedVersions` |
| Nothing eligible ends `no_fix_available` with the reasons and no manifest change | Verified (integration) | `TestVulnerable_NoFixAvailable` (moved-package, GO-TEST-0003 has no fix) |
| patch-safe with GO-TEST-0001: lib v1.2.4, the lowest fixing version and not v1.3.0, proposed with the advisory section; both scans bound to the base and candidate trees; the proposal verifies; the checkout is untouched | Verified (integration) | `TestVulnerable_PatchSafeFixesAdvisory` |
| An indirect target: inner, required only as `// indirect` and reached through wrap.Run, upgraded to v1.0.1 (not v1.1.0); go.mod keeps it indirect at that version; the body says indirect | Verified (integration) | `TestVulnerable_IndirectTarget` (fixture `indirect-fix`) |
| closure-regression with GO-TEST-0005: util is a direct requirement, the fix v0.2.0 renames Trim, which core v1.0.0 still calls, so the post build fails in core and the run ends `regressed` with nothing scanned or proposed | Verified (integration) | `TestVulnerable_ClosureRegressionIsRegressed` |
| Build-list rules at Gate B: target selected at exactly its version after tidy, nothing entering or moving up outside the closure, nothing moving down; a tidy that drops an indirect requirement and reselects the old version is refused | Verified; the drop observed on the real toolchain at the gates | `TestVerifyBuildList`, `TestParseBuildList`; `TestBuildListGate_TidyDroppingAnIndirectTargetIsRefused` (integration, fixture `indirect-dropped`) |
| Vulnerable selection reaching that drop through a scan | Not reachable today: govulncheck v1.1.4 and v1.8.0 report only modules whose packages they load, and go 1.17+ tidy keeps the requirement of every such module, so the module that tidy drops is never a finding (observed 2026-10-04) | `TestVulnerable_NoVulnerabilities` shows inner unreported in `indirect-dropped` |
| Scan records in the task store: tree, configuration hash, image digest, scanner version and sha256, database snapshot id and modified time, Go version, parsed scan, raw output gzip-compressed with its sha256 checked on read; an existing data directory opens and gains the table | Verified | `TestScanRecordRoundTrip`, `TestOpen_MigratesAnOlderDatabase` |
| Readiness rules with their own codes, all evaluated: conclusive post scan bound to the candidate tree and the base scan's configuration, image, scanner, database, and Go version (`scan_not_bound`, `scan_inconclusive`); targeted findings gone at every level and found version (`advisory_not_resolved`); nothing introduced or escalated, standard library included (`advisory_introduced`); an unusable post scan fails all three | Verified | `TestCheckScans` (resolved; persisting at another version or a lower level; introduced; escalated; inconclusive post; missing post; other tree, configuration, scanner, database, image; inconclusive base; a target the base never reported; a record that disagrees with its scan) |
| An upgrade that clears its target but introduces another advisory is not ready and freezes nothing | Verified (integration) | `TestVulnerable_IntroducedAdvisoryIsNotReady` |
| Advisory section in the proposal body: fixed advisories with aliases, summary, level and call path at base, found and fixed versions; direct or indirect; remaining third-party findings with why; standard-library count and why; scanner version, database snapshot and modified time; database text sanitized, bounded, and kept on one line in a code span | Verified | `TestAdvisorySection`, `TestUntrusted`; `TestVulnerable_PatchSafeFixesAdvisory` (integration) |
| A real advisory on a real repository: labstack/echo v4.15.4, GO-2026-5970 in `golang.org/x/text` v0.38.0 (indirect, symbol level) found at base and cleared by v0.39.0 with nothing introduced, against the live database | Verified (smoke, network) | `TestSmokeVulnerable` (`internal/smoke/testdata/vulnerable/echo-x-text.json`) |
| Version cooldown `-min-age` on `maintain` and `inspect`, 72 hours by default, 0 off: a too-new version, or one with no publish time, is ineligible with a reason naming its age; times read from `go list -m -json module@version` only for otherwise eligible versions and only when the cooldown is on; candidate JSON for versions old enough unchanged | Verified | `TestCooldownReason`, `TestDiscover_Cooldown`, `TestDiscover_CooldownLeavesOldVersionsByteIdentical`; `TestModelMode_ReplaysRecordedRuns` (integration) replays the committed recordings with the default cooldown on |
| Cooldown waived in vulnerable selection for the version that clears the advisories, recorded as `cooldown_waived` and stated in the proposal; the default selection under the same clock finds nothing eligible | Verified (integration, injected clock) | `TestCooldown_WaivedOnlyForTheAdvisoryFix` |
| Native Linux Docker: the scan, the build-list listing from a staging copy, and the stored scan output behave as on colima | Verified (integration) by CI's integration job | the `internal/steward` integration tests above |

The agent path does not select by vulnerability. Doing so needs a tool
result the model can see that lists findings and targets (a new tool, or a
new field in `list_candidates`), readiness failures with the new codes in
`prepare_proposal`'s error, and the advisory section in
`deterministicBody`; each changes what the model sees, so the committed
recordings would have to be re-recorded.

## Not claimed

The sandbox reduces risk from untrusted build behaviour on operator-selected
repositories. It does not claim container-escape resistance, OS-enforced
egress control during acquisition against a real proxy, or protection of a
validation's build cache from the code that same validation runs. The full
list of accepted risks is in [security.md](security.md).

## Known limitations

- The Go module cache under the data directory is created read-only by the
  toolchain. A `cache clean` command is Required.
- Integration test packages must run serially against one engine.
- Model-mode recordings are tied to the exact prompt and tool shapes and
  must be re-recorded after any change to them.
- VM-backed engines cache path lookups. The code never deletes and
  recreates a directory at the same path during a run and probe markers
  are unique, but any new mount path added later must follow the same rule.
