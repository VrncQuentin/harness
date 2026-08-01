# PR #403 — Findings Inventory

Each PR #403 finding is listed below with its planned replacement PR and the
regression test or eliminated mechanism. PR #403 stays open as a reference
until the sequence below is merged and the final audit (PR 12) passes.

## Findings mapped to replacement PRs

### PR 2a — Anchor primitive

| # | Finding | Test |
|---|---------|------|
| 2.1 | Construction through a stable symlink alias must bind the physical target | `TestAnchor_ConstructionThroughSymlinkSucceeds` |
| 2.2 | Construction through a stable Windows junction must bind the physical target | `TestAnchor_ConstructionThroughJunctionSucceeds` |
| 2.3 | Re-pointing a previously-stable alias root fails closed | `TestAnchor_RePointedAliasFailsClosed`, `TestAnchor_RePointedJunctionFailsClosed` |
| 2.4 | Same-name directory replacement fails closed | `TestAnchor_SameNameReplacementFailsClosed` |
| 2.5 | Original directory renamed aside — silent switch | `TestAnchor_RenameAsideFailsClosed` |
| 2.6 | Windows filesystem identity not established | `TestAnchor_WindowsIdentity` |
| 2.7 | Identity failure not propagated | `TestAnchor_IdentityFailureFailsClosed` |

### PR 2b — Anchor comparison primitive

| # | Finding | Test |
|---|---------|------|
| 2.8a | `SameAnchor` compares pinned handles, not pathname re-resolution | `TestAnchor_SameAnchor_*` (5 tests) |

### PR 2c — DirReader identity, lifetime, and transactional reload

| # | Finding | Test |
|---|---------|------|
| 2.8b | Git and memory readers compare identities from later `pathid` resolutions, not from the objects each component actually opened | PR 5b4: `gitw.Repo` retains the pinned repository boundary (`rootfs.NewAnchorFromRoot`) and compares it against the reader's retained anchor with `os.SameFile` (`DirReader.SameRepo`), so a same-name replacement at the pathname is detected (`TestRepo_SameAnchorDetectsSameNameReplacement`). Runtime construction compares every independently opened reader (`activeMem`, `gitRepo`, `sessionStore`, and `globalMem` when it shares the active path) and fails closed on mismatch (`TestCandidateIdentityMismatchFailsClosed`). `SameDirReader` remains the reader-to-reader handle comparison; the git-to-memory comparison uses `SameRepo`. |

### PR 3a — Atomic publication and governor B3

| # | Finding | Test |
|---|---------|------|
| 3.1 | B3 spill writes through pre-existing hard-linked entry | `TestB3_ReplacesHardLinkedSpillEntry` (governor) |
| 3.2 | B3 spill written with `os.WriteFile` — no rename, partial reads possible | migrated: governor uses Anchor + WriteStreamAtomic |
| 3.7 | `WriteStreamAtomic` does not fsync before rename — crash-unsafe | `TestWriteStreamAtomic_SyncBeforeRename` (sync failure blocks publication) |
| 3.8 | `WriteStreamAtomic` cleanup deletes by name after rename has consumed it | `TestWriteStreamAtomic_DoesNotCleanUpTempOnFailure` |
| 3.9 | `WriteStreamAtomic` must preserve a stranger's substituted entry | `TestWriteStreamAtomic_DetectsSubstitutedTemp` (Linux CI, asserts impostor survives) |
| 3.10 | `WriteStreamAtomic` must pin the destination directory once | `TestWriteStreamAtomic_PinSurvivesIntermediateSwap` (Linux CI) |
| 3.11 | A failed write may leave its own partial temp entry | `TestWriteStreamAtomic_DoesNotCleanUpTempOnFailure` |

### PR 3b — git_push HEAD migration

| # | Finding | Test |
|---|---------|------|
| 3.5 | `git_push` reads `.git/HEAD` by path — fails on linked worktrees | `TestCurrentBranch_LinkedWorktreeLayout` (git) |

### PR 3c — Remaining standalone consumers

| # | Finding | Test |
|---|---------|------|
| 3.3 | Retrieval trace sink opens spill directory by pathname | `TestNDJSONSink_TraceDirectoryIsPinned` |
| 3.4 | Retrieval trace deletes by pathname, could delete stranger's file | `TestNDJSONSink_RetentionDeletesOnlyOwnFiles` |
| 3.6 | `eval-retrieval` uses `filepath.Glob` + `filepath.Rel` on operator root | `TestEvalRetrieval_PinnedRepo` |
| 3.12 | `Set.Open` resolves the target before pinning each candidate root | `TestSet_OpenResolvesAlongsideEachPin` |
| 3.13 | `Root.Open` exposes `*os.File.Name()`, enabling a read to become a pathname reopen | `TestRoot_OpenDoesNotExposePathname` |

### PR 4 — Rooted traversal and memory read path

| # | Finding | Test |
|---|---------|------|
| 4.1 | `DirReader.Read` re-joins `r.root + rel` — every read is a fresh TOCTOU | `TestDirReader_ReadDoesNotFollowLink` |
| 4.2 | `DirReader.Walk` navigates by pathname, not pinned child | `TestDirReader_WalkKeepsDescendingInsidePinnedTree` |
| 4.3 | Symlink/junction escape during walk not prevented | `TestOpenChildNoFollow_DetectsSubstitution` (rootfs, mid-open substitution); `TestDirReader_WalkDoesNotEnterSymlink` (static behavior) |
| 4.4 | Directory cycle detected by depth only | Eliminated: no-follow component traversal makes filesystem cycles impossible within the stated threat model (bind mounts out of scope) |
| 4.5 | `DirReader.Glob` reads parent directory by pathname | `TestDirReader_GlobDoesNotFollowLinkOutOfRoot` |
| 4.6 | `DirReader.ListDirs` reads by pathname | `TestDirReader_ListDirsDoesNotFollowLinkOutOfRoot` |

### PR 5a — Memory writes migration

| # | Finding | Test |
|---|---------|------|
| 5.1 | `DirReader.WriteFile` writes by pathname, not through a pinned handle | `TestDirReader_WriteFileReplacesHardLinkedLeaf` |

### PR 5b1 — Vector copy-on-write

| # | Finding | Test |
|---|---------|------|
| 5.5 | Index append→truncate rollback propagates through hard links; must assemble replacement separately and publish by rename | `TestIndex_UpsertReplacesViaRename`, `TestIndex_UpsertManifestFailurePreservesOldIndex`, `TestIndex_UpsertFailurePreservesOtherEntries` |

### PR 5b2 — Index rooted identity

| # | Finding | Test |
|---|---------|------|
| 5.2 | Index located by absolute pathname (`<repo>/index/_episodes`) | `TestEpisodeIndex_LinkedIndexDirectoryCannotEscapeTheRepo` |
| 5.3 | Index re-pointed after pin — manifests a different repo | `TestEpisodeIndex_RepointedAfterPinFailsClosed` |

### PR 5b3 — Manifest publication and serialization

| # | Finding | Test |
|---|---------|------|
| 5.4 | Post-rename `os.Remove` + retry fallback deletes a stranger's replacement | `TestIndex_WriteManifestDoesNotRemoveStranger` |
| 5.6 | Manifest publication not fsynced before rename | `TestIndex_WriteManifestFsyncsBeforeRename` |
| 5.7 | Temp file cleanup deletes by name after the rename may have consumed it | `TestIndex_WriteManifestCleansUpOwnTemp` |
| 5.8 | Scattered lock maps — no unified per-repo coordinator | PR 5b3 narrowed this to one index-directory coordinator keyed by physical identity and bound to the pinned root (`TestIndex_TwoHandlesShareCoordinator`, `TestIndex_ColdStartTwoHandles`, `TestIndex_ColdStartAdoptsExisting`, `TestIndex_SpellingsShareCoordinator`, `TestIndex_RepointedAliasRefused`). PR 5b4 replaced both remaining lock maps (`repoMutationLocks`, `indexMutationLocks`) with one repository-wide coordinator in `internal/coord`, keyed by verified repository identity and shared by git mutations and index publication. Index publication and the following git commit run inside one repository transaction (`Repo.WithMutation` / `Index.UpsertRootedUnder`), with the lock order fixed as repository gate then per-handle mutex (`TestEpisodeIndex_MixedUpsertLockOrder`); discriminating tests: `TestRepoAndIndex_ShareRepositoryTransaction`, `TestRepoTransaction_CommitInsideSessionDoesNotReacquire`, `TestRepoTransaction_FailedMutationReleasesCoordinator`. |
| 5.9 | Compare-then-rollback against unlocked concurrent writer | `TestIndex_TwoHandlesShareCoordinator` (each write re-reads committed state under the coordinator) |

### PR 6 — Project repository workflow

| # | Finding | Test |
|---|---------|------|
| 6.1 | Source and destination not pinned for complete copy | `TestMoveProjectRepo_SourceAndDestinationPinned` |
| 6.2 | Containment checked by name, not handle-bound identity | `TestMoveProjectRepo_ContainmentCheckByNameIsInsufficient` |
| 6.3 | Trees not checked for disjointness in both directions | `TestMoveProjectRepo_OverlappingTreesRejected` |
| 6.4 | Traversal descends by name after inspecting by handle | `TestMoveProjectRepo_DescendsThroughPinnedChild` |
| 6.5 | `.gitkeep` created by joining layout directory path | `TestCreateMissing_LinkedLayoutDirectoryDoesNotPlaceGitkeepOutside` |
| 6.6 | Same-repo aliases (symlink, junction, hard link) not detected | `TestMoveProjectRepo_SameRepoAlias` |
| 6.7 | Nested destination / reverse overlap not caught | `TestMoveProjectRepo_NestedDestination` |
| 6.8 | Renamed child during copy escapes containment | `TestMoveProjectRepo_RenamedChildMidCopy` |
| 6.9 | Recursive self-copy not rejected | `TestMoveProjectRepo_RecursiveSelfCopy` |

### PR 7 — Session log and append migration

| # | Finding | Test |
|---|---------|------|
| 7.1 | `session.ReadAll` reads sessions.jsonl by pathname | `TestSessionLog_ReadsThroughPinnedRoot` |
| 7.2 | `session.AppendRecord` opens sessions.jsonl by pathname | `TestSessionLog_AppendsThroughPinnedRoot` |
| 7.3 | Append uses `os.OpenFile` — no identity check | `TestSessionLog_AppendDoesNotFollowLinkOutOfRoot` |
| 7.4 | Sidecar publication uses pathname | `TestSessionLog_SidecarPublishedThroughPinnedRoot` |
| 7.5 | Non-`ErrNotExist` error on read interpreted as "no sessions" | `TestSessionLog_ReadAllOnlyErrNotExistMeansNoSessions` |
| 7.6 | Hard-link append limitation documented | No test — inherent limitation. Documented. |

### PR 8 — Immutable UI dependency snapshots

| # | Finding | Test |
|---|---------|------|
| 8.1 | Repeated getter calls can combine store/committer/runner from different publications | `TestSnapshot_RequestUsesConsistentDependencies` |
| 8.2 | Detached goroutines hold references across reload without generation lease | `TestSnapshot_DetachedGoroutineCapturesSnapshotBeforeStart` |
| 8.3 | Old snapshot references remain valid after reload | `TestSnapshot_OldReferencesRemainValidAfterReload` — deferred to PR 8 |
| 8.4 | `memoryHandles` / `genGate` / route-by-route drain NOT introduced | Eliminated mechanism — route-by-route genGate is rejected. Snapshot-scoped leasing (if any) is deferred to PR 8. |
| 8.5 | `memoryAPISnapshot` NOT introduced | Eliminated mechanism — snapshot pattern replaces it |

### PR 9 — Explicit applied runtime state

| # | Finding | Test |
|---|---------|------|
| 9.1 | "Old model" reconstructed from mutable project store | `TestAppliedState_OldModelNotReconstructedFromStore` |
| 9.2 | `ApplyConfig` not serialized end-to-end | `TestAppliedState_SerializedApplyConfig` |
| 9.3 | Candidate built then published (not published piecemeal) | `TestAppliedState_PrepareQuiesceCommitRetire` |
| 9.4 | Failed candidate restores ten individual fields | `TestAppliedState_FailedCandidateDiscarded` |
| 9.5 | Process rollback uses re-derived config instead of recorded applied state | `TestAppliedState_RollbackUsesRecordedState` |
| 9.6 | `LiveApplied` does not reflect final live state | `TestAppliedState_LiveAppliedReflectsFinalState` |
| 9.7 | Timed-out API shutdown loses ownership of still-running server | `TestAppliedState_TimeoutShutdownRetainsOwnership` |
| 9.8 | Project override deletion/edit lost on reload | `TestAppliedState_ProjectOverrideDeletion` |
| 9.9 | Global port changes not reflected until restart | `TestAppliedState_GlobalPortChanges` |
| 9.10 | `llama_on_switch=keep` violated on config apply | `TestAppliedState_LlamaOnSwitchKeep` |

### PR 10 — Project edits and shutdown lifecycle

| # | Finding | Test |
|---|---------|------|
| 10.1 | Active project moved while runtime still targets it | `TestProjectEdit_ActiveRepoNotMovable` |
| 10.2 | Project edits silently update store, runtime deps on old repo | `TestProjectEdit_UpdateRoutesThroughTransaction` |
| 10.3 | Retry compares against newly-read store values, not recorded applied state | `TestProjectEdit_RetryComparesAgainstAppliedState` |
| 10.4 | New admissions accepted during shutdown | `TestShutdown_StopsNewAdmissionsFirst` |
| 10.5 | Root/task contexts not cancelled before waiting on components | `TestShutdown_CancelsBeforeWaiting` |
| 10.6 | Every wait is bounded or context-aware | `TestShutdown_BoundedWait` |
| 10.7 | Timed-out drain closes resources still in use | `TestShutdown_DrainTimeoutDoesNotCloseInUse` |
| 10.8 | Unbounded queue stop called after bounded drain already failed | `TestShutdown_NoUnboundedStopAfterDrainFailure` |
| 10.9 | API ownership released before termination known | `TestShutdown_APIOwnershipPreservedToTermination` |

### PR 11 — Explicit session recovery state

| # | Finding | Test |
|---|---------|------|
| 11.1 | Session state inferred from timestamp, empty path, or physical order | `TestSessionRecovery_ExplicitRecordState` |
| 11.2 | No `pending` state between raw sidecar durable and summary publication | `TestSessionRecovery_PendingAfterSidecarDurable` |
| 11.3 | No `complete` state after summary publication/commit | `TestSessionRecovery_CompleteAfterCommit` |
| 11.4 | No monotonically allocated save sequence or attempt ID | `TestSessionRecovery_MonotonicSaveSequence` |
| 11.5 | Same-attempt `complete` does not deterministically supersede `pending` | `TestSessionRecovery_CompleteSupersedesPending` |
| 11.6 | Wall-clock ordering used for correctness | `TestSessionRecovery_NoWallClockOrdering` |
| 11.7 | Backward-compatible interpretation for existing records | `TestSessionRecovery_BackwardCompatible` |
| 11.8 | Summarizer failure during first save makes session undiscoverable | `TestSessionRecovery_FirstSaveDiscoverable` |

### PR 12 — Final repository migration audit

| # | Finding | Test |
|---|---------|------|
| 12.1 | Migration allowlist entries still present | `fsaudit` reports zero migration entries |
| 12.2 | Configured-tree pathname operations outside rootfs | `fsaudit` reports only permanent exceptions |
| 12.3 | Production `os.OpenRoot` exists outside internal/rootfs | `fsaudit` reports os.OpenRoot only in rootfs.go |
| 12.4 | Compatibility wrappers or unused APIs remain | Manual review — removed |
| 12.5 | `rootfs.go` is monolithic | Split into identity, read, write, walk, set/target files |
| 12.6 | Test setup repetitive | Consolidated without removing discriminating cases |
| 12.7 | Architecture documentation verbose | Reduced to invariants, ownership, threat boundaries, exception ledger |
| 12.8 | All PR #403 findings mapped or eliminated | This checklist verified |

## Eliminated mechanisms (no test needed)

These PR #403 approaches are rejected entirely. The replacement architecture
makes them unnecessary.

| Mechanism | Why eliminated |
|-----------|---------------|
| `memoryHandles` — runtime holds root handles and closes on shutdown | Replaced by generation lease model: the `Runtime` tracks generations through `generation.leases` and closes old-generation readers when the last in-flight operation releases its lease. |
| `genGate` — route-by-route drain wrapping | Eliminated. Route-by-route generation gates are rejected. Deferred snapshot-scoped leasing (if any) is designed in PR 8. |
| `memoryAPISnapshot` — snapshot of memory API per generation | Replaced by immutable UI snapshots in PR 8 |
| `snapshot.closeReplaced()` — close handles when replacement starts | Deferred to PR 8: Anchor ownership and lifetime design there |
| `stopMemoryAndAPI` — drops references, does not close | Replaced by Anchor ownership in PR 2a: consumers own and close their Anchors; no runtime stop/drop cycle |
| `ResolveAbsRepoPath` — session manager method | Replaced by rooted capabilities in PR 7: session manager receives a handle, not a pathname factory |
| Package-global test hooks for identity verification | Replaced by function-parameter hooks in PR 2a: no global state |
| `.gitkeep` created by joining layout directory absolute path | Replaced by repo-relative addressing through pinned root in PR 6 |
