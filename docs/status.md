# Implementation status

Status values: **Required** (planned, not implemented), **Implemented** (code
exists, reference given), **Verified** (a named test exercises it). Tests
marked *integration* run under `-tags integration` against a real engine.

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
| Repository and dependency content returned with an untrusted-data notice | Implemented | `internal/tools` |
| Terminal tools: `prepare_proposal` and `report_blocked`; no remote or destructive tools exist | Verified | `TestSpecs_AreCompleteAndTerminalToolsMarked` |
| Phase derived from the promotion journal; tools gated by phase | Verified | `TestPhaseAndTarget_FromJournal`, `TestPhaseGating` |
| `apply_upgrade` allowed only for the exact eligible module and version, once per run | Verified | `TestApplyUpgrade_ExactEligibility`, `TestPhaseGating` |
| Scope computed on the projected diff before a write: hard limit aborts, soft limit asks once, expansion raises soft to hard, a second ask aborts, other approval kinds do not count | Verified | `TestWrite_ScopeBeforeApply`, `TestWrite_SingleExpansion`, `TestProjectedScope_CountsProposedWrite` |
| Repair budgets: validation cycles and no-progress detection on introduced findings | Verified | `TestValidate_Budgets` |
| Validation evidence produced by a tool is admissible only when its runtime step is done | Verified (integration) | `proposal.Inputs.StepDone`, `verifyProposal` in `scripted_integration_test.go` |
| S1 patch upgrade through the agent path | Verified (integration) | `TestScripted_S1_PatchUpgrade` |
| S2 breaking minor repaired in one source file; protected test unchanged | Verified (integration) | `TestScripted_S2_BreakingMinorRepaired` |
| S3 moved package repaired by import fix | Verified (integration) | `TestScripted_S3_MovedPackageRepaired` |
| S8 protected change: write denied by policy, run reports blocked, no proposal | Verified (integration) | `TestScripted_S8_ProtectedChangeBlocked` |
| Interrupted run reconciliation hook runs promotion and freeze recovery | Implemented | `steward.runAgent` `Reconcile` |

## Milestone 1, batch 1C

| Control or capability | Status | Reference |
|---|---|---|
| Run options and candidate facts persisted at start; resume reconstructs the session under the same configuration hash | Verified (integration) | `task.SetTaskContext`, `steward.Resume`, `TestCLI_ScopeExpansionAcrossProcesses` |
| Resume refuses a run started with a fixture proxy unless the proxy is supplied again, and refuses a finished run | Verified (integration) | `TestCLI_ScopeExpansionAcrossProcesses`, `TestCLI_RejectCancels` |
| `approve` and `reject` decide the single pending approval from a separate process; a second decision is refused | Verified (integration) | `agentrt.Approve`, `agentrt.Reject`, `TestCLI_ScopeExpansionAcrossProcesses` |
| Soft scope limit pauses the run with a `scope_expansion` approval whose presentation names the write; after approval and resume the write lands and the proposal includes it | Verified (integration) | `TestCLI_ScopeExpansionAcrossProcesses` |
| Hard scope limit ends the run without asking | Verified (integration) | `TestCLI_HardScopeLimitAborts` |
| Reject cancels the run, records the outcome, and the pending write never lands | Verified (integration) | `TestCLI_RejectCancels` |
| A process killed inside a tool leaves the run RUNNING with an executing step; resume records the step as interrupted, reconciles, and continues to a proposal | Verified (integration, fault-injected binary) | `TestCLI_InterruptedRunResumes` |
| `runs list` and `runs show` expose task, run, steps, approvals, proposals, and promotions | Verified (integration) | `TestCLI_*` |
| Paths are never deleted and recreated within a run; probe markers carry a nonce | Verified (integration) | `workspace.Materialize`, `sandbox.Probe`, `TestCLI_ScopeExpansionAcrossProcesses` |

Milestone 1 is complete.

## Milestone 2, batch 2A

| Control or capability | Status | Reference |
|---|---|---|
| Ollama adapter over the native chat API: system, tool_use, and tool_result mapping, tool_calls parsed, usage reported, 5xx and connection failures transient, 4xx and body errors plain | Verified | `internal/model/ollama`, `TestGenerate_MapsRequestAndToolCalls`, `TestGenerate_ErrorsClassified` |
| Model agent: fixed system prompt with no repository content; steps rendered as tool_use and tool_result pairs; older results elided; first tool use mapped to a decision; one nudge on text-only replies then a fail decision; limit errors propagated | Verified | `internal/agent`, `TestRender_StepsBecomeToolUseAndResultPairs`, `TestDecide_MapsFirstToolUse`, `TestDecide_NudgesOnceThenFails`, `TestDecide_PropagatesLimitErrors` |
| `-mode model` with persisted model spec so `resume` rebuilds the same agent; `-record` and `-replay` through the runtime's replay package | Implemented | `internal/steward/model.go` |
| Live local-model runs: patch-safe selects v1.2.4 and freezes a manifest-only proposal; breaking-minor reads the new signature, rewrites the call site, and freezes a proposal with main.go | Observed on 2026-09-17 with qwen3:30b-a3b, not a gated test | see README |

Required for batch 2B: replayable model-mode tests, which need
deterministic tool outputs (validation record ids and timings currently
vary between runs); a proof-of-capability harness that runs S1 through S8
against a chosen model under a call cap and records outcomes as data; a
second local model for comparison.

## Not claimed

The sandbox reduces risk from untrusted build behaviour on operator-selected
repositories. It does not claim container-escape resistance, OS-enforced
egress control during acquisition against a real proxy, or protection of the
per-run build cache from code under test. Credential handling, approvals,
mutation tools, and publication are Required and arrive in later milestones
with their own tests.

## Known limitations

- The Go module cache under the data directory is created read-only by the
  toolchain. A `cache clean` command is Required.
- Integration test packages must run serially against one engine.
- Model-mode runs are not replayable yet because tool results embed
  run-specific identifiers; recordings from one run do not match the next.
- VM-backed engines cache path lookups. The code never deletes and
  recreates a directory at the same path during a run and probe markers
  are unique, but any new mount path added later must follow the same rule.
