# Consolidation Plan

**Status:** complete · **Opened:** 2026-06-21 · **Completed:** 2026-07-11

This document is the recovery plan after a fast, breadth-first development sprint.
It acknowledges where we cut corners, records the consolidated findings from two
independent reviews, and lays out an ordered plan to get back to a stable,
verifiable state before any new milestone work resumes. The consolidation plan
is now complete; remaining work has moved back to the normal roadmap.

---

## 1. Where we actually are

We properly implemented **M1-M3** before the rushed breadth-first sprint. Their
acceptance criteria were verified earlier in the project history and remain
marked complete in the roadmap.

The consolidation work has now landed through **Phase 7**. That means the
dangerous M7 slice was first made safe, then reintroduced behind the approval
layer, the docs were reconciled, and the riskiest consolidation-era packages now
have regression coverage. M4/M5/M6 implementation gaps called out by the audit
were also addressed or explicitly descoped in the roadmap.

This does **not** mean every milestone after M3 is acceptance-complete. Feature
checkboxes now track code that exists; acceptance-test boxes stay unchecked
unless a test was explicitly run and observed passing. **M8 has not started.**

**M9 (Layout V2) and M10 (Pipeline DSL) are frozen.** They exist only as design
docs. No M9/M10 implementation begins until the M3b-M8 line is stabilized and its
acceptance tests pass.

### True milestone status

| Milestone | Current reality |
|---|---|
| M1-M3 | **Done** and previously acceptance-verified |
| M3b Projects | **Mostly implemented**; directory index manifests remain deferred/open and acceptance tests are not fully checked |
| M4 Agent Loop | **Implementation stabilized**; routed through assembler + queue, part/tool events persisted, per-tool toggles added; acceptance tests still open |
| M5 Semantic | **Episode semantic retrieval implemented**; blended retrieval fixed, episode rebuild/status/scores added; attached-directory indexing deferred |
| M6 Promotion | **Implementation complete for scoped M6**; cross-agent prompt injection intentionally descoped beyond M6 |
| M7 Tools+Perms | **Approval-gated core landed**; `file_write`/`shell_exec`, layered approvals, destructive classification, UI approval cards, and audit trail exist; Windows-native shell execution and web search remain in M7; file edit/patch, steering/follow-ups, extension hooks, sub-agents, and tool-history retry UI are deferred |
| M8 Hardening | **Not started** |
| M9 / M10 | **Frozen** (docs only) |

---

## 2. Consolidated findings

The original audit findings are kept here with current disposition so we do not
re-litigate already-fixed problems or lose track of what remains.

| # | Finding | Current disposition |
|---|---|---|
| 1 | Unguarded destructive tools | **Resolved.** Phase 0 removed them from the live M4 surface; Phase 5 reintroduced `file_write` and `shell_exec` behind disabled-by-default config toggles and the M7 approval evaluator. |
| 2 | `/task` bypassed Prompt Assembler and Queue | **Resolved.** Phase 2 routes tasks through the assembler and queue path. |
| 3 | Blended-retrieval truncation kept the lowest-scored episodes | **Resolved.** Phase 3 keeps top-N after descending blended-score sort and uses exponential recency decay. |
| 4 | Non-streaming inference was impossible | **Resolved at inference-client level.** The inference client now handles non-streaming tool-call responses; the agent loop still requests streaming completions. |
| 5 | Custom JS and `node_modules` instead of htmx | **Resolved.** Phase 1 removed the frontend toolchain and moved chat/status/log updates to server-rendered htmx/SSE flows. |
| 6 | Metrics docs overstated implemented metric families | **Resolved in docs.** Architecture now lists only current metric constants as implemented and marks later families aspirational until code exists. Missing runtime metrics remain M8 work. |
| 7 | Undocumented packages (`runtime`, `db`, `index`, `logbuf`, `project`, `reqid`) | **Resolved in Phase 6.** Architecture now documents these packages and their boundaries. |
| 8 | Config doc missed `Agent`, `Log`, `Loop`, cache, retrieval, and summarizer fields | **Resolved in Phase 6.** Architecture config section now mirrors the current config struct at section/field level. |
| 9 | `tools.Context` under-specified | **Resolved.** Phase 2 added session id, caller identity, and cancellation context. |
| 10 | Storage language mixed pre-M9 and post-M9 layouts | **Resolved in docs.** Architecture now frames current single-repo paths versus planned layout-v2 paths explicitly. |
| 11 | Roadmap checkboxes were wrong in both directions | **Resolved in Phase 6.** Roadmap now separates implemented features from unchecked acceptance tests and marks descoped/deferred items explicitly. |
| 12 | Promotion path looked wrong because docs used future layout-v2 path | **Resolved.** Current `global/facts.md` and future `projects/global/facts.md` language is aligned. |
| 13 | `internal/agentloop` and `internal/embedder` had zero tests | **Resolved for consolidation.** Agentloop approval tests landed in Phase 5; Phase 7 adds embedder client tests plus Phase 2-4 runtime/UI regressions. |

---

## 3. The plan

Ordered by risk, then by dependency. Each phase is a separate PR.

### Phase 0 — Drop dangerous exec — **done**

- Removed `file_write` and `shell_exec` from the live M4 default surface so M4 returned to read-only behavior.
- Acceptance target: model attempting a write/shell call gets a "tool not available" result; loop continues; no disk mutation.

### Phase 1 — Kill the custom JS and `node_modules` — **done**

- Removed the frontend lint/build dependency path.
- Vendored htmx/SSE assets and moved status, logs, chat resume, and chat send flows toward server-rendered fragments.
- Removed the JS-only conveniences accepted in the original plan: beacon autosave, pinned-bottom scroll behavior, and styled dialog modals.

### Phase 2 — Fix M4 properly — **done**

- Routed `/task` through the Prompt Assembler and Queue.
- Persisted tool-call/tool-result messages for UI display and session replay.
- Added per-tool enable/disable config and UI controls.
- Added/verified non-streaming tool-call parsing in the inference client; the loop still uses streaming completions.
- Expanded `tools.Context` with session id, caller identity, and cancellation context.

### Phase 3 — Fix M5 — **done for scoped M5**

- Fixed blended retrieval ordering and recency decay.
- Added episode index status, UI-triggered episode index rebuild, and retrieval-score display.
- Explicitly deferred attached-directory indexing until directory-level semantic search becomes user-facing.

### Phase 4 — Fix M6 — **done for scoped M6**

- Aligned current/future facts and notes layout language.
- Implemented the promotion dedup pass.
- Implemented the cross-agent episode browser.
- Descoped cross-agent prompt injection beyond M6.

### Phase 5 — M7 approval-gated destructive tools — **done for approval core**

- Reintroduced `file_write` and `shell_exec` behind disabled-by-default config toggles and the M7 approval flow.
- Added allow once, always allow, and reject decisions.
- Wired layered permissions: builtin defaults -> user config -> session approvals.
- Added destructive-command classification, sandbox working directory validation, timeouts, output truncation, UI approval cards, and session audit trail.
- Windows-native shell execution remains open: the current `shell_exec` path still assumes `sh` is available.
- Web search remains in M7 as an opt-in, clearly disclosed network tool with a per-tool disable toggle.
- Broader M7-adjacent expansion is deferred beyond M7: file edit/patch, steering/follow-ups, extension hooks, sub-agents, tool history, and retry failed tool call.

### Phase 6 — Docs + roadmap reconciliation — **done**

- Documented `internal/runtime`, `db`, `index`, `logbuf`, `project`, `reqid`, and `approvals`.
- Fixed the config section list and current-vs-layout-v2 storage framing.
- Marked missing M2/M4/M5/M6/M7 metric families as aspirational until code exists.
- Rewrote roadmap checkboxes to reflect true implementation state while keeping acceptance boxes unchecked unless observed.

### Phase 7 — Test the new/risky packages — **done**

- Added `internal/embedder` HTTP client tests for request shape, response indexing, empty inputs, status errors, and health checks.
- Added a Phase 2 runtime regression proving task execution goes through the Prompt Assembler and Queue rather than direct inference.
- Added a Phase 4 UI regression proving dedup-blocked fact promotion redirects without writing or committing.
- Existing Phase 3 prompt/index regressions cover blended retrieval ordering, exponential recency decay, and episode index rebuild. Browser-level acceptance tests remain tracked in the roadmap.

---

## 4. What remains frozen

- **M9 — Layout V2:** depends on a stable M3b/M5/M8 base. No work begins until
  the M3b-M8 acceptance tests pass.
- **M10 — Pipeline DSL:** depends on M7 (real permissions) and M9. Design docs may
  evolve; no `internal/dsl` / `internal/pipeline` code begins until M9 ships.

## 5. Next roadmap step

Resume normal milestone work at scoped **M7**: finish Windows-native shell
execution, add opt-in web search, and run the M7 browser-level acceptance tests.
