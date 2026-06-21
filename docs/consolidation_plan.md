# Consolidation Plan

**Status:** active · **Opened:** 2026-06-21

This document is the recovery plan after a fast, breadth-first development sprint.
It acknowledges where we cut corners, records the consolidated findings from two
independent reviews, and lays out an ordered plan to get back to a stable,
verifiable state before any new milestone work resumes.

---

## 1. Where we actually are

We properly implemented **M1–M3** — those milestones are coherent, tested, and
their acceptance criteria broadly hold (note: the M1–M3 acceptance tests were
*not* re-run as part of this audit; the verdict is based on code + history, not a
fresh test pass).

After that we **rushed M3b through M7 breadth-first** — implementing a slice of
each milestone instead of finishing one before starting the next, and without
updating the roadmap or running acceptance tests. The result is real code for
M4/M5/M6 functionality and *premature, unsafe* M7 code, but **no milestone past
M3 is actually complete**. **M8 has not been started.**

**M9 (Layout V2) and M10 (Pipeline DSL) are frozen.** They exist only as design
docs. No M9/M10 implementation begins until the M3b–M8 line is stabilized and its
acceptance tests pass.

### True milestone status

| Milestone | Roadmap claim | Reality |
|---|---|---|
| M1–M3 | done | **Done** (not re-verified this pass) |
| M3b Projects | 2 boxes open | **~90%** — switch/reload works; directory index manifests missing |
| M4 Agent Loop | mixed | **~60%** — loop runs but bypasses assembler + queue; not read-only; no durable parts; no per-tool toggles |
| M5 Semantic | mixed | **~40%** — embedder + episode embedding + flat ANN done; blended retrieval **broken**; no dir indexing/rebuild/UI scores |
| M6 Promotion | 2 boxes ticked | **~30%** — promote-fact / append-note backends only |
| M7 Tools+Perms | all open | **~15%** — destructive tools exist with **no safety layer** (premature) |
| M8 Hardening | open | **Not started** |
| M9 / M10 | open | **Frozen** (docs only) |

---

## 2. Consolidated findings

Cross-checked between two independent reviews; the items below were verified
against the code (file:line evidence inline).

### Critical (correctness / safety)

1. **Unguarded destructive tools.** `RegisterBuiltins` registers `file_write`
   and `shell_exec` into the live tool registry
   ([internal/tools/tools.go:138](../internal/tools/tools.go)). `shell_exec` runs
   `sh -c <model string>` with only a 30s timeout — **no approval, no permission
   layer, no destructive-command classification**
   ([internal/tools/tools.go:281](../internal/tools/tools.go)). A model emitting
   `rm -rf` inside the sandbox executes it silently. M4 was supposed to be
   read-only; this is M7 work shipped without its safety half.

2. **`/task` bypasses the Prompt Assembler and the Queue.**
   `taskRunnerAdapter.RunTask` builds raw `inference.Message` values from the
   conversation and hands the inference client directly to `agentloop.NewEngine`
   ([internal/runtime/adapters.go:517](../internal/runtime/adapters.go),
   [:570](../internal/runtime/adapters.go)). Consequences: agent-loop tasks get
   **no rules/persona/memory/episode injection**, and they **skip queue
   backpressure, the WAL, and single-model serialization**. Contradicts
   [architecture.md](architecture.md) ("Call the Prompt Assembler, submit
   requests through Queue/Inference Client").

3. **Blended-retrieval truncation bug.** `loadEpisodes` sorts episodes by blended
   score *descending*, then truncates with `out[len(out)-n:]` — keeping the
   **lowest**-scored episodes and discarding its own top matches
   ([internal/prompt/prompt.go:418](../internal/prompt/prompt.go),
   [:425](../internal/prompt/prompt.go)). Also "recency_decay" is implemented as
   a linear rank `(i+1)/n`, not a decay.

4. **Non-streaming inference is impossible.** `Complete` hardcodes
   `req.Stream = true` ([internal/inference/inference.go:153](../internal/inference/inference.go)),
   so the "streaming and non-streaming tool-call parsing" requirement cannot be met.

### Architecture / hygiene

5. **Custom JS + `node_modules` instead of htmx.** Zero `hx-` attributes in any
   template, but a 20KB `assets/static/app.js` plus inline `<script>` in
   `task.html`, a `node_modules/` tree, `biome.json`, and a JS
   lint CI step. Directly violates the stated design ("htmx + SSE, no JS
   framework, no build step, no node_modules"). Root cause is shared with #2: the
   chat/task surfaces keep conversation state in the **browser** and stream
   directly from a `fetch`, instead of server-owned state + SSE-driven swaps.

6. **Metrics are mostly aspirational.** M1 metrics (`uptime`, `queue_depth`,
   `process_health`, `restart_count`) and M3 metrics (`session_count`,
   `episode_count`, `git_commit_latency_ms`) are recorded
   ([internal/metrics/recorder.go](../internal/metrics/recorder.go)). M2/M4/M5/M6/M7
   metric names don't exist as constants despite that code shipping.

7. **Undocumented packages.** `internal/runtime` (owns the mutable service graph —
   the doc's "Core" box), `internal/db` (all SQL), `internal/index` (vector
   format), plus `logbuf`, `project`, `reqid` are not described in
   [architecture.md](architecture.md).

8. **Config doc is stale.** Sections `Agent`, `Log`, `Loop` and fields like
   `cache_type_k/v`, `recency_n`, `summarizer_prompt`, `semantic_weight`,
   `recency_weight` exist in the struct + migrations but not in the doc's section
   list.

9. **`tools.Context` under-specified.** Only `ProjectSlug` + `SandboxRoots`
    ([internal/tools/tools.go](../internal/tools/tools.go)); the architecture
    calls for session id, caller identity, and cancellation context.

10. **Storage language mixes pre-M9 and post-M9.** Code uses `harness.db` next to
    the binary; the doc describes `~/.harness/` and `projects/<slug>/` repos as if
    current. Docs need explicit "current vs. after layout-v2" framing.

### Roadmap accuracy

11. **Checkboxes are wrong in both directions.** Done-but-unticked: M4 tool
    registry, M5 embedder sidecar, M5 episode embed-on-commit, M4 loop
    visibility, M3b project switch/reload. Ticked-but-broken: M5 blended
    retrieval. Genuinely missing despite milestone "progress": per-tool toggles,
    attached-directory indexing, index rebuild, retrieval-score UI, cross-agent
    read, dedup, and the entire M7 safety layer.

### Not a bug (recorded to avoid re-litigating)

12. **Promotion path.** `handlePromoteFact` writes `global/facts.md`
    ([internal/ui/promotion.go:50](../internal/ui/promotion.go)) and the assembler
    *reads* the same path (`factsPath = "global/facts.md"`,
    [internal/prompt/prompt.go:44](../internal/prompt/prompt.go)). Promoted facts
    **do** surface — it is a doc/layout naming drift (architecture.md says
    `projects/global/facts.md`, which is the future layout-v2 path), not a runtime
    break.

### Test coverage gaps

13. `internal/agentloop` (the loop engine) and `internal/embedder` have **zero
    tests** — the two newest, riskiest packages are uncovered.

---

## 3. The plan

Ordered by risk, then by dependency. Each phase is a separate PR.

### Phase 0 — Drop dangerous exec (immediate, smallest)

- Remove `file_write` and `shell_exec` from `RegisterBuiltins`
  ([internal/tools/tools.go:138](../internal/tools/tools.go)). M4 returns to
  **read-only** (`file_read`, `file_list` only).
- Keep the tool implementations in the tree but unregistered; they return in M7
  behind approvals.
- Acceptance: model attempting a write/shell call gets a
  "tool not available" result; loop continues; no disk mutation.

### Phase 1 — Kill the custom JS and `node_modules`

Goal: server-side rendering, minimalist client, **no npm**, while keeping a decent
UX (live status, streaming). The only client lib is **vendored htmx** (one file,
embedded via `embed.FS`, no build step, no node_modules).

- Delete `assets/static/app.js` and the inline `<script>` in `task.html`.
- Remove `node_modules/`, `biome.json`, any package manifest/lock files, and
  the frontend (Biome/JS) lint CI step. Add `node_modules/` and `harness.db` to
  `.gitignore`.
- Vendor htmx as a single static asset served from `embed.FS`; load the SSE
  extension the same way.
- Move conversation/transcript state **server-side** (the session manager already
  exists). Chat/task then stream tokens and tool parts via `sse-swap` +
  `hx-swap="beforeend"` instead of browser `fetch` streaming. This also unblocks
  Phase 2 (#2) since turns now flow through the server.
- Status page: push server-rendered fragments over the existing `/events` SSE,
  swapped with htmx, instead of JSON patched by JS. Move uptime formatting
  server-side.
- Accept the loss of three JS-only conveniences for now: `sendBeacon` autosave on
  tab close (use an explicit Save), "scroll only if pinned to bottom" (default
  scroll), and styled `<dialog>` modals (use `hx-confirm`).
- **Decision point:** if even vendored htmx is unwanted, the fallback is pure
  server-rendered forms with full-page reloads — but that loses live status and
  token streaming. Recommendation: keep vendored htmx.

### Phase 2 — Fix M4 properly

- Route `/task` (and `/chat`) through the **Prompt Assembler + Queue** so
  personas/memory apply and requests are serialized/backpressured (resolves #2).
- Persist **part-based messages** (`text`, `tool_call`, `tool_result`) for UI
  display and session replay.
- Add **per-tool enable/disable** config + UI toggles (new migration + config
  fields); the loop filters the registry by it.
- Add **non-streaming** tool-call parsing, or explicitly defer it in the roadmap
  with a reason (resolves #4).
- Expand `tools.Context` with session id, caller identity, and cancellation
  context (#9).

### Phase 3 — Fix M5

- Repair blended-retrieval ordering: truncate to top-N **after** the blended sort,
  not by slice position (#3). Make recency an actual decay, not linear rank.
- Implement attached-directory indexing (`projects/<slug>/index/<dir-slug>/`) **or**
  explicitly defer it in the roadmap with a note (this is the open M3b/M5 item).
- Add the UI-triggered **index rebuild** flow (idempotent, per tree).
- Surface **retrieval scores** in the memory browser.

### Phase 4 — Fix M6

- Decide and align the facts/notes layout naming (`global/` vs `projects/global/`)
  and update docs accordingly (#12).
- Implement the **dedup** pass on promotion, or uncheck "M6 complete."
- Implement **cross-agent read**, or explicitly descope it.

### Phase 5 — M7 (only after a real permission layer exists)

- Reintroduce `file_write` / `shell_exec` **behind** an approval flow
  (once/always/reject), layered permissions (agent default → user config →
  session), destructive-command classification, per-tool toggles, and an audit
  trail in the UI. Destructive tools must not be registered by default until this
  lands.

### Phase 6 — Docs + roadmap reconciliation

- Document `internal/runtime`, `db`, `index`, `logbuf`, `project`, `reqid` (#7).
- Fix the config section list (#8) and add explicit "current vs. post-M9" storage
  framing (#10).
- Add the missing M2/M4/M5/M6 metrics, or mark them aspirational in the doc (#6).
- Rewrite the roadmap checkboxes to reflect true state — **unchecked unless the
  acceptance test was actually run and observed** (#11).

### Phase 7 — Test the new/risky packages

- Add tests for `internal/agentloop` and `internal/embedder` (#13), plus
  regression tests for the bugs fixed in Phases 2–4.

---

## 4. Frozen until the above is done

- **M9 — Layout V2:** depends on a stable M3b/M5/M8 base. No work begins until
  Phases 0–7 land and the M3b–M8 acceptance tests pass.
- **M10 — Pipeline DSL:** depends on M7 (real permissions) and M9. Design docs may
  evolve; no `internal/dsl` / `internal/pipeline` code begins until M9 ships.
