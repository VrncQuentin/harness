# Full Project Review

**Date:** 2026-07-07
**Reviewer:** Claude (Fable 5)
**Scope:** entire repo at `main` (7041856) — architecture docs, roadmap, and all non-test Go code (~15,200 lines production, ~13,800 lines tests).
**Out of scope:** security. Nothing in this document is a security assessment; findings that touch the sandbox or permission layers are evaluated strictly for functional correctness and design adherence.

**Method:** read `docs/architecture.md` and `docs/roadmap.md`, then every production `.go` file package by package, cross-checking behavior against the docs and the roadmap checkboxes. `go build ./...`, `go vet ./...`, and `go test ./... -count=1` all pass cleanly (23 packages, 0 failures).

---

## Verdict in one paragraph

This is an unusually well-built hobby-scale codebase: consistent error wrapping, real table-driven tests almost everywhere, clean package boundaries, honest comments, and a design doc that the code mostly follows. The architecture itself is sound for what it wants to be. The problems are concentrated in three places: (1) a handful of genuine correctness bugs, two of which break headline features (project switching kills the request queue permanently; the M5 blended retrieval trims the *most relevant* episodes first); (2) the M7 tool/approval layer was written for Unix while the product targets Windows first, so `shell_exec` and the destructive-command classifier do not work on the primary platform; and (3) milestone discipline has slipped — M7 features shipped while M4, M5, and M3b acceptance tests remain unchecked, which is exactly what the project's own rules say not to do.

---

## 1. Do the architecture, design, and plans make sense?

### What is genuinely good

- **The staging order is right.** Inference core → prompt assembly → memory → projects → agent loop → semantic retrieval → promotion → destructive tools → hardening → layout-v2 → pipeline DSL. Each milestone builds on the previous; destructive tools deliberately come after read-only loop MVP; the DSL is deliberately last. The rationale paragraphs in `architecture.md` ("Key Design Decisions") are real reasons, not slogans.
- **UI-server-first startup** (UI binds before anything else can fail, all errors render in the browser) is the correct design for a no-terminal desktop app, and `cmd/harness/main.go` implements it faithfully.
- **One SQLite handle, one `db` package owning all SQL** with embedded golang-migrate migrations is clean and correctly executed ([internal/db/db.go](../internal/db/db.go)).
- **htmx + SSE + `html/template`, no build step** — implemented as designed, and the multiplexed single `/events` stream (documented in [sse.go:29](../internal/ui/sse.go) with the browser-connection-limit rationale) is a nice touch.
- **The process manager** ([internal/proc/proc.go](../internal/proc/proc.go)) is the strongest package in the repo: circuit breaker, 503-means-loading handling, user-initiated-restart vs. crash accounting, hex exit codes for Windows crash triage. All of it justified in comments and covered by tests.

### Design decisions that don't hold up

**D1. The queue WAL is designed for a problem this product doesn't have.**
The architecture mandates a WAL so queued requests survive a crash. But queued items are *interactive chat completions*: after a crash the browser/API client that owned the response channel is gone. The implementation is honest about this — replayed requests get a response channel drained by `for range resp {}` ([queue.go:272](../internal/queue/queue.go)) — which means crash recovery consists of re-running inference (minutes of GPU time on local hardware) and throwing away the output; its only observable effect is marking WAL entries done. Meanwhile every enqueue pays a serialized `fsync` ([queue.go:446](../internal/queue/queue.go)), and since the agent loop routes *every inner turn* through the queue via `queuedInferClient`, a 10-turn task fsyncs its entire growing conversation 10 times. And after all that, `config.Defaults()` leaves `Queue.WALPath` empty, so the WAL is **off by default** and nothing resolves it to the `projects/<active>/queue.wal` location the architecture specifies. Recommendation: either drop the WAL requirement for M-series entirely (a crashed chat is simply lost, which is what already effectively happens), or redefine it as a journal for *session recording* rather than request replay.

**D2. M7 (shell + destructive classification) was designed Unix-first for a Windows-first product.**
`shell_exec` runs `sh -c` ([tools.go:313](../internal/tools/tools.go)) — not present on a stock Windows machine, so the tool always fails on the primary target OS. `ClassifyShellCmd` ([approvals.go:183](../internal/approvals/approvals.go)) matches `rm `, `sudo `, `mkfs`, `iptables`, fork bombs, `> /etc` — and nothing for `del`, `rd /s`, `Remove-Item`, `format`, `reg delete`. The design (prefix classification of destructive commands) is inherently brittle, but even accepting it, it classifies the wrong shell's commands. This needs a platform decision (`cmd /c`? PowerShell? bundled busybox?) before M7 can be called done on Windows.

**D3. Lossy delivery of semantically critical loop events.**
The agent loop emits *all* events — streamed text, but also `approval_needed`, `done`, `limit`, `doom_loop` — through a non-blocking send that silently drops on a full buffer ([loop.go:430](../internal/agentloop/loop.go)), into a 64-slot channel, then through a second 64-slot channel, then broadcast to browser SSE subscribers with 8-slot drop-on-full channels ([task.go:178](../internal/ui/task.go)). Dropping a text delta is cosmetic. Dropping `approval_needed` means the approval card never renders while the loop blocks forever waiting for a decision (see C5/C6 below). Dropping `done` means the UI never re-enables input. A design that distinguishes "droppable" (text deltas) from "must-deliver" (state transitions, approvals) events is needed; the current uniform best-effort channel is the wrong tool.

**D4. Broadcast SSE assumes exactly one browser tab.**
Chat tokens and task events are broadcast to *every* subscriber of `/chat/events` and `/task/events` with no session routing ([chat.go:444](../internal/ui/chat.go), [task.go:178](../internal/ui/task.go)). Two tabs (or the same page open on a second machine) interleave each other's streams into their transcripts. A single-user local tool can legitimately assume one *user*, but one *tab* is not a safe assumption for a browser app. Frames should carry the session id and clients should filter, or subscribers should register per session.

**D5. Metrics retention is designed but not implemented — and the design is silently violated.**
`architecture.md` promises "raw rows kept for 30 days, downsampled hourly aggregates kept indefinitely." There is no pruning and no downsampling anywhere in the codebase (no `DELETE FROM metrics` exists). `metrics.retention_days` is validated, stored, editable in the UI — and dead. At the current recording rate (6 rows/10 s from [events.go](../internal/runtime/events.go) plus per-save session metrics) `harness.db` grows by ~50 k rows/day, forever.

**D6. Documentation drift, both directions.**
- `architecture.md` contains the **Metrics Store section twice** (lines ~241 and ~272) with slightly different content.
- The embedder is described as "nomic-embed-text… ships as a self-contained binary (no Python dependency)" but is actually llama-server run with `--embedding` ([proc.go:599](../internal/proc/proc.go)). Fine choice — better than a second stack — but the doc describes a different design.
- Roadmap checkboxes are stale in both directions: M4 marks "Loop engine" done but leaves "OpenAI-style tool-call parsing", "tool registry and schema contract", "read-only file tools", and "config toggles" unchecked — all of which are implemented. Conversely M2 checks "Qwen3 prompt template formatting" which is explicitly *not* done in the assembler (delegated to llama-server, per the package comment in [prompt.go:6](../internal/prompt/prompt.go)). M7 has zero checkboxes ticked while commits #159–#170 implemented most of it.

**D7. Milestone discipline broke down after M4.**
The project's own rules (CLAUDE.md: "Work one milestone at a time… Do not advance until all acceptance tests pass") are violated by the current state: **all eight M4 acceptance tests are unchecked, all nine M5 acceptance tests are unchecked, two M3b acceptance tests are unchecked** — and yet M6 is done and M7 is substantially implemented. The unchecked M3b reload test is not a formality: the feature is actually broken (C1 below). The rule existed precisely to catch this.

---

## 2. Are the architecture and rules respected?

### Respected (spot-checked, no violations found)

- Startup sequence matches the documented 10 steps exactly; UI truly starts first and every later failure lands in `AddStartupError`.
- UI and API servers on separate ports, API opt-in and off by default.
- go-git only; no git binary; memory repo never auto-created (scaffolding only fills in missing layout items and refuses to touch wrong-kind entries — [layout.go](../internal/memory/layout.go) is careful, good work).
- No CLI, no subcommands, no `init()` functions anywhere, systray owns the main goroutine, single-instance via named mutex ([tray_windows.go](../internal/tray/tray_windows.go)).
- `sessions.jsonl` is genuinely append-only with fsync per record and garbled-line-tolerant reading ([session/log.go](../internal/session/log.go)) — the M3 corruption acceptance test is honored.
- Error wrapping style (`pkg: context: %w`) is applied consistently across every package.
- Stdlib-first: dependencies are limited to go-git, fsnotify, systray, modernc sqlite, golang-migrate, x/sys. All defensible.
- Every package has tests except `embedder`, `reqid`, and `pkg/httpclient` (the first has real logic — response-index mapping — and deserves a test file).

### Deviations

**A1. `panic` in library code, twice.**
`tools.Registry.Register` panics on duplicate id ([tools.go:74](../internal/tools/tools.go)); `session.NewManager` panics on nil deps ([session.go:193](../internal/session/session.go)). CLAUDE.md: "no `panic` in library code." The session one is at least documented as a programming-error guard; the registry one can fire from a future plugin/extension path and should return an error.

**A2. M4 sandbox scope silently widens to the memory repo.**
The roadmap says file tools are "scoped to active project directories." When the active project has no directories configured, `RunTask` falls back to using the *memory repo* as the sandbox root ([adapters.go:649-656](../internal/runtime/adapters.go)). The memory repo is not a project directory — it holds rules, personas, facts, episodes, and session sidecars. Functionally this means an agent's `file_read`/`file_write` surface defaults to the harness's own memory store, which no document describes. Should be: no roots → all file tools rejected (which is what `validatePath` already does with an empty root list).

**A3. Task cancellation is implemented in the engine but unreachable by the user.**
M4 checks "[x] Cancellation and abort propagation through the loop." The engine honors ctx cancellation, but there is no `/task/cancel` route ([ui.go:472-508](../internal/ui/ui.go) route table), and tasks run on the *server's root context* ([task.go:99](../internal/ui/task.go)), so closing the tab, navigating away, or anything short of quitting the harness does not stop a runaway loop. The corresponding acceptance test ("Click cancel mid-task…") is honestly unchecked, but the feature checkbox is misleading.

**A4. "The git log is the index" — except it isn't.**
The commit-tag design (`[agent:x] [type:episode]`, `QueryLog`, `BlobByRef`) is fully implemented in [internal/git](../internal/git/git.go) and [message.go](../internal/git/message.go)… and never called from production code. Episode discovery, the memory browser, retrieval, and the index rebuilder all walk the filesystem. Either wire the UI's episode browsing to the log as designed, or delete `Log`/`BlobByRef` and update the doc.

**A5. Status page queue depth is dead.**
`Server.SetQueueDepth` has **no production caller** (only a test calls it), so the queue card renders 0/0 forever regardless of load. M1's "shows queue depth" line and the acceptance test are marked done; the metric goes to SQLite but the live UI element was never wired. One-line fix in the `recordMetrics`/`ForwardEvents` loop.

**A6. API sessions are started and never ended.**
The M3 session contract is "on end → summarize → episode → commit." The API server mints a fresh session per request and appends turns ([api.go:296-311](../internal/api/api.go)), but nothing ever calls `Save` or `End` for them — they sit in the manager's map, holding full conversations in RAM, until harness shutdown flushes everything at once. Consequences: (a) unbounded memory growth proportional to API traffic, (b) episodes for API usage only ever land in git if the harness exits cleanly. Either save-on-completion or drop the per-request session minting until the M4 mapping arrives.

**A7. "Session approvals" are process-global, not session-scoped.**
The M7 spec layers "agent defaults → user config → *session* approvals." The evaluator holding the session layer is built once in `startMemoryAndAPI` and shared by every engine ([memory_api.go:168-174](../internal/runtime/memory_api.go)), so an "Always allow" clicked in one task session silently applies to all future sessions (and all agents) until restart or the next service rebuild. That is a meaningfully broader grant than the UI's "Always Allow (this session)" wording implies.

**A8. Loop/approval config changes don't propagate.**
`ApplyConfig` rebuilds the memory/API/task stack only when `Memory`, `Prompt`, `API`, `Agent.Active`, or the project slug change ([config.go:98-102](../internal/runtime/config.go)) — `cfg.Loop` is not in the list. The engine reads `Loop` live per task, but the **approvals evaluator's user layer is stale**: enable `shell_exec` in `/config` and the evaluator keeps its "user: shell_exec disabled in config" `Denied` rule until a restart or an unrelated config change forces a rebuild. Net effect: toggling destructive tools on via the UI doesn't work as advertised.

---

## 3. Critical bugs

Ordered by severity. C1–C5 are the ones I'd fix before anything else.

**C1. Project switch with `llama_on_switch = "reload"` (the default) permanently kills the request queue.**
`handleProjectSwitch` calls `rt.reqQueue.Stop()` then `rt.reqQueue.Start(ctx)` on the *same* Queue ([project_switch.go:76-98](../internal/runtime/project_switch.go)). But `Queue.Stop` is one-way: `stopOnce` fires, `stopped` is set true forever, and the intake channel is `close()`d ([queue.go:129-142](../internal/queue/queue.go)). `Start` resets none of that — the new worker goroutine reads the closed channel and exits immediately, and every subsequent `Enqueue` returns `ErrStopped`. After the first project switch, **all chat, task, and API requests fail with "queue is shutting down" until the harness is restarted**. (The M3b acceptance test covering exactly this is unchecked — correctly.) Fix: give Queue a `Reset`/`Restart` that reconstructs channel + flags, or make the runtime build a fresh Queue on switch (and rewire the adapters that captured the old pointer).

**C2. Budget trimming evicts the *most relevant* episodes after blended retrieval.**
`loadEpisodes` sorts episodes **best-score-first** when blended retrieval is active ([prompt.go:423-427](../internal/prompt/prompt.go)). `trim` then enforces the memory budget by repeatedly dropping `lay.episodes[0]` on the assumption the slice is oldest-first ([prompt.go:521-548](../internal/prompt/prompt.go)). With semantic retrieval enabled, index 0 is the *highest-scoring* episode — so whenever trimming kicks in, the assembler throws away the best matches and keeps the worst. This silently inverts the entire point of M5. Fix: trim from the tail after blended sort (or sort ascending and keep the existing drop-head logic).

**C3. An episode's semantic score is its *worst* chunk's score.**
`Index.Search` returns one result per chunk, sorted descending. `loadEpisodes` folds them with `scores[r.SHA] = float64(r.Score)` ([prompt.go:409-412](../internal/prompt/prompt.go)) — each later (lower-scored) chunk of the same episode overwrites the earlier (higher) one, so a multi-paragraph episode is ranked by its least relevant paragraph. The UI's `indexScorer` does it correctly (`if !ok || > existing` keep-max, [memory_api.go:464-467](../internal/runtime/memory_api.go)), so the memory browser shows different scores than the assembler actually uses. Fix: keep the max in `loadEpisodes` too.

**C4. `shell_exec` cannot work on Windows.**
`exec.CommandContext(ctx, "sh", "-c", cmdStr)` ([tools.go:313](../internal/tools/tools.go)) — no `sh` on stock Windows; the M7 flagship tool fails 100% of the time on the primary target platform, and the destructive classifier guards Unix commands only (see D2). The M7 acceptance tests would have caught this on first run.

**C5. Dropped `approval_needed` events can hang a task forever.**
`Engine.emit` drops on a full channel ([loop.go:430-435](../internal/agentloop/loop.go)). If the `approval_needed` event is dropped (event channel congested by streamed text — plausible, since each task event is template-rendered and fanned out per subscriber), `checkApproval` still blocks on `<-ch` with **no timeout** — despite `ErrApprovalTimeout` being declared and the comment saying "Wait for user decision with timeout" ([loop.go:396-399](../internal/agentloop/loop.go)). No card is ever shown, no decision can arrive, the task is stuck until harness shutdown (no user cancel exists — A3). Two independent fixes needed: make state-transition events blocking-or-fail, and actually implement the timeout.

**C6. Chat SSE framing breaks on multi-line tokens.**
The chat path writes raw model content into the frame: `fmt.Fprintf(w, "event: chat-token\ndata: %s\n\n", tok.Content)` ([chat.go:391](../internal/ui/chat.go), same in `writeChatSSEContent`). A token containing `\n` (models emit these constantly — every paragraph break) terminates the SSE data payload early; the remainder of the token is either lost or misparsed as protocol lines. Every other SSE writer in the codebase escapes via `sseData()`; the chat token path forgot. (Task events don't have this bug — they go through `sseData`.) The same path also inserts content without HTML escaping, unlike the template-rendered task path, so literal `<`/`&` in model output renders incorrectly.

**C7. Explicit "deny" on a destructive command is downgraded to Ask.**
In `Evaluator.Evaluate`, when `ClassifyShellCmd` is true, the code only honors an exact-match session rule whose decision is `Allowed`; any other outcome — including a matched `Denied` rule from the user layer or session layer — falls through to `return Ask` ([approvals.go:141-160](../internal/approvals/approvals.go)). A rule that says "never run this" produces a fresh approval prompt every time instead of a denial. Currently latent (the UI never persists deny rules, and the engine's enable-toggle gate usually fires first), but it's a logic inversion in the permission core.

**C8. The permission evaluator fails open for unknown tools.**
`Evaluate` initializes `best := Allowed` ([approvals.go:123](../internal/approvals/approvals.go)); a tool id matching no rule in any layer is allowed without interaction. The four builtins are covered by `DefaultLayer`, but the moment M7's extension hooks or a fifth tool registers, it bypasses approvals entirely (`isToolEnabled` also defaults unknown ids to `true`, [loop.go:437-450](../internal/agentloop/loop.go) — the two defaults compound). Default should be `Ask`.

**C9. Metrics grow without bound** — retention knob dead (see D5). Slow-burn disk/scan-time failure measured in months.

**C10. API session memory leak** — sessions minted per API request are never ended or saved (see A6). Slow-burn RAM failure proportional to API traffic.

**C11. `Index.Add` can corrupt the manifest on partial failure.**
The chunk entry is appended to the in-memory manifest *before* `appendVectors` writes the file ([index.go:105-115](../internal/index/index.go)); if the vector write fails, the method returns error but the phantom entry stays in `idx.manifest.Chunks`, and the *next* successful `Add` persists a manifest pointing at offsets containing the wrong vectors. Also, the doc comment says "Returns error if the SHA already exists" while the code deliberately returns `nil`.

**C12. Windows path-comparison in the sandbox validator is case-sensitive.**
`validatePath` compares with `strings.HasPrefix` on raw paths ([tools.go:113-141](../internal/tools/tools.go)). On Windows, `c:\proj` vs `C:\proj` (drive-letter case varies by source: config form input vs `os.Executable` vs project store) makes a legitimately in-sandbox path fail validation. Functional consequence: spurious "path is outside sandbox roots" errors that depend on how the user typed the directory into the projects form.

**C13. Task transcript recording is all-or-nothing at the end of the run.**
`recordTaskEvents` runs only after the event channel closes, and the forwarding goroutine returns early *without recording anything* if the context is cancelled mid-stream ([adapters.go:681-696](../internal/runtime/adapters.go)). A task interrupted by shutdown leaves zero trace in the session, including tool calls that already executed — which undermines the M7 audit-trail intent (approval decisions are persisted only via this same path).

**Minor (noted, not expanded):** `resolveSlot` grows the tool-call slot slice to any index the model emits — a malformed `index: 10⁹` delta allocates unboundedly ([loop.go:458](../internal/agentloop/loop.go)); approval audit trail numbers `approval_needed` and `approval` with one shared counter so the pair gets different numbers ([adapters.go:785-810](../internal/runtime/adapters.go)); `embedder.Embed` can return nil rows if the server omits indices and only the first row is length-checked by callers; `file_write` doesn't create parent directories, unlike `memory.WriteFile`, so models must mkdir-by-luck.

---

## 4. Over-engineered bits

**O1. The queue WAL + replay machinery.** ~150 lines (`walRecord`/`walPayload` durable schema, `recoverWAL` dedup/ordering, `replayPending`, `retryReplay` with backoff, `enqueueReplay` spin-wait, `clearWALIfDrained`) plus fsync-per-enqueue — all to replay requests whose outputs are discarded (D1), and disabled by default anyway. This is the single largest mismatch between engineering effort and delivered value in the repo. Delete or repurpose.

**O2. The fsnotify hot-reload watcher.** 272 lines ([prompt/hotreload.go](../internal/prompt/hotreload.go)) — watcher lifecycle, parent-dir watching for atomic-rename saves, per-path debounce timers, agent/project rewiring — whose entire observable effect is a `slog.Info("prompt: file changed")` line. The actual hot-reload behavior comes from `DiskAssembler` re-reading every file on every `Assemble` call. The package comment admits this. The M2 acceptance tests pass *because of the re-read*, with or without the watcher. Either make the watcher do something (invalidate a cache, push an SSE toast) or delete it.

**O3. Dead git query surface.** `Repo.Log` tag filtering, `ParseMessage`/`BuildMessage` round-tripping, and `BlobByRef` with its deterministic-diff-sort — implemented, tested, and unused by any production caller (A4). The commit-tag *writing* is used; the reading half is speculative.

**O4. Two parallel chat streaming stacks.** `/chat/stream` (fetch-based, per-request SSE, proper request-context cancellation, correct backpressure) coexists with `/chat/send` + `/chat/events` (htmx broadcast, root-context, drop-on-full). The template only uses the broadcast pair — the *better-engineered* path is the dead one. Ironically, `/chat/stream`'s design (per-request stream, request ctx) is exactly what would fix D4/C6's structural problems; consolidate on it.

**O5. `ui.Server`'s fifteen mutex-guarded setter/getter pairs.** `retryMu`, `storeMu`, `agentRegMu`, `binDirMu`, `memRepoMu`, `memStoreMu`, `committerMu`, `dedupMu`, `promotionDedupThresholdMu`, `scorerMu`, `rebuilderMu`, `chatRunnerMu`, `taskRunnerMu`, `sessionStoreMu`, `projectStoreMu` ([ui.go:117-181](../internal/ui/ui.go)) — each protecting a single pointer that is swapped wholesale during rebuilds. One `atomic.Pointer[deps]` holding an immutable struct would replace ~200 lines of boilerplate and make rebuild atomicity *better* (today a request can observe a half-swapped mix of old and new adapters mid-`stopMemoryAndAPI`/`startMemoryAndAPI`).

**O6. Per-request-shaped micro-interfaces are (mostly) fine — with one exception.** The repo's many small interfaces (`Committer`, `FileWriter`, `Enqueuer`, `MetricsRecorder`…) are pulling their weight in tests. The exception is the `memory.Reader` + four optional capability interfaces (`DirLister`/`DirCreator`/`FileWriter`/`DirRemover`/`Walker`) discovered via type assertions at every call site, with five dedicated `errNoDirX` sentinels for fakes that forget a method — for a codebase with exactly one production implementation that has all of them. A single `memory.Repo` interface would delete the assertion plumbing; the capability split is abstraction for its own sake, which CLAUDE.md warns against.

**Not over-engineered (deliberately noted):** the proc manager's circuit breaker (justified by real llama-server crash loops), the layered approvals model itself (matches the M7 spec; the bugs are in the implementation, not the layering), the per-page template sets (the comment explains the name-collision reason), and the `logbuf` ring (small, tested, used by three consumers).

---

## 5. Recommended order of work

1. **Fix C1** (queue death on project switch) — it breaks the default configuration of a shipped, checked-off feature.
2. **Fix C2 + C3 together** (trim direction, max-vs-last chunk score) — small diffs, they restore M5's actual value; add an acceptance test that trims under blended retrieval.
3. **Decide the Windows shell story** (C4/D2) before touching anything else in M7 — everything in the approval layer is calibrated to a shell the product doesn't run on.
4. **Make loop state-transition events reliable and add the approval timeout** (C5, D3), and wire a `/task/cancel` route (A3).
5. **Sweep the small correctness fixes**: C6 (`sseData` on chat tokens), A5 (queue depth card), A8 (evaluator rebuild on Loop change), C7/C8 (evaluator defaults), C10/A6 (API session lifecycle).
6. **Then go back and run the M3b/M4/M5 acceptance checklists honestly** before writing another M7 line — the unchecked boxes predicted two of the three worst bugs in this review.
7. Housekeeping: deduplicate the Metrics Store section in `architecture.md`, reconcile roadmap checkboxes with reality, delete or wire the dead code from O1–O4.

---

# Addendum — Follow-up Review

**Date:** 2026-07-15
**Reviewer:** Claude (Fable 5)
**Scope:** delta since the review above (`7041856` → `fda394d`, ~20 commits: M8 observability hardening and M9 layout-v2), plus a simplification/refactoring-focused pass over the whole codebase (~17,000 lines production Go). Prior findings were re-verified against today's code rather than repeated; this addendum records their status and adds new findings, labelled **S** (simplification) and **N** (new code-level items).

---

## 6. Status of the original findings

**Fixed since 2026-07-07:**

- **C4 / D2 (Windows shell + classifier):** `shell_exec` now uses `cmd.exe /d /s /c` on Windows ([tools.go:330-335](../internal/tools/tools.go)), and `ClassifyShellCmd` covers `del`, `erase`, `rd /s`, `rmdir /s` alongside the Unix set ([approvals.go:193](../internal/approvals/approvals.go)).
- Roadmap/doc reconciliation landed for the M7 scope docs, and consolidation phases 6–7 addressed several checklist items.

**Still open (re-verified at `fda394d`):**

- **C1 — project switch still kills the queue.** `handleProjectSwitch` still calls `Stop()` then `Start()` on the same one-shot `Queue` ([project_switch.go:76-97](../internal/runtime/project_switch.go)); `stopOnce`/`stopped`/`close(ch)` are never reset ([queue.go:138-151](../internal/queue/queue.go)). First switch under the default `llama_on_switch=reload` still bricks all inference until restart.
- **C2 — trim still evicts the best episodes.** `trim` drops `lay.episodes[0]` while blended retrieval sorts best-first ([prompt.go:521-527](../internal/prompt/prompt.go)).
- **C3 — assembler still ranks an episode by its worst chunk** ([prompt.go:409-412](../internal/prompt/prompt.go)) while the UI scorer keeps the max — the browser still shows scores the assembler doesn't use.
- **C6 — chat SSE framing still breaks on multi-line tokens.** `tok.Content` is still written raw, without `sseData()`, in both `streamChatTokens` and `writeChatSSEContent` ([chat.go:264, 391](../internal/ui/chat.go)).
- **A5 — `SetQueueDepth` still has no production caller**; the queue card still renders 0/0.
- **O1 — the queue WAL got worse, not better:** M9 now defaults the WAL path to `<active repo>/queue.wal` ([lifecycle.go:170-177](../internal/runtime/lifecycle.go)), so the fsync-per-enqueue machinery whose replayed output is discarded is now **on by default**.
- **O2 — the fsnotify hot-reload watcher** was rewritten for layout-v2 ([hotreload.go](../internal/prompt/hotreload.go), 250 lines) instead of deleted; its only effect is still a log line.
- **O3/A4 — `Repo.Log`/`BlobByRef`** still have no production caller.
- **O4 — `/chat/stream`** is still a complete second chat stack referenced by no template.
- **O5 — `ui.Server`'s mutex-per-dependency design** is still present and grew two more fields (`memRepo`, `promotionDedupThreshold`) since the original review.

The pattern flagged as D7 (milestone discipline) repeated: the top-priority fixes from section 5 were left open while M8 and M9 shipped on top of the affected files.

---

## 7. New architecture-level findings (simplification)

**S1. The logical-path translation layer (`LayoutV2Reader`) preserves a layout that no longer exists — delete it.**
M9 moved to one git repo per project, but instead of updating callers, [layout_v2.go](../internal/memory/layout_v2.go) (~300 lines) rewrites logical pre-M9 paths (`global/rules.md`, `projects/<slug>/episodes/...`) onto two physical repos via `mapPath`, plus `LayoutV2Committer` for commits and `globalLogicalPath` to un-map walks. Meanwhile the session manager already works the simple way — repo root + plain relative paths ([session.go:160-186](../internal/session/session.go)). The codebase therefore runs **two path conventions simultaneously**: prompt assembly, the agent registry, the memory UI, and the dedup checker speak "logical"; sessions, the episode index, and the WAL speak "physical". Every future feature must pick a dialect, and `mapPath`'s special cases (`projects/global` aliasing to the global repo, `agents/` at the global root) are where edge cases will accumulate. Recommendation: give consumers two plain `DirReader`s (`global`, `active`) and make each caller say which repo it means; delete `mapPath`, `LayoutV2Committer`, `globalLogicalPath`, `joinLogical`, `walkAll`, and the slug-prefixed path formatting in [prompt.go](../internal/prompt/prompt.go) and [ui/memory.go](../internal/ui/memory.go). This is the single largest simplification available, and it is cheapest now — before M10 pipelines build on the logical dialect.

**S2. `internal/runtime` has become a god package — move domain logic out, leave wiring.**
[adapters.go](../internal/runtime/adapters.go) (885 lines) + [memory_api.go](../internal/runtime/memory_api.go) (802) + lifecycle/config/switch ≈ 2,700 lines. The architecture doc scopes runtime to "the mutable service graph" — wiring — but it also *implements* the semantic-memory domain: `indexRebuilder`, `indexScorer`, `afterSaveEmbed`, `dedupChecker`, `cosineSimilarity`, `chunkSummary`, `retrievalDecay`. Move these next to [internal/index](../internal/index/index.go) (or a new `internal/retrieval`); runtime shrinks to what its doc claims and S3 becomes impossible to sustain.

**S3. Blended retrieval is implemented twice, divergently — unify it.**
`DiskAssembler.loadEpisodes` ([prompt.go:373-439](../internal/prompt/prompt.go)) and `indexScorer.ScoreEpisodes` ([memory_api.go:498-547](../internal/runtime/memory_api.go)) both do embed-query → ANN search → blend with exponential recency decay, with duplicated helpers (`expDecay`/`retrievalDecay`, `extractID`/`episodeID`). They disagree on chunk aggregation (max vs. last-wins — that divergence *is* C3). One shared `BlendScores` function fixes the duplication and the bug in the same diff.

**S4. `internal/ui/projects_page.go` violates a core design decision and hosts misplaced plumbing.**
- `handleProjectBackup` ([projects_page.go:293-347](../internal/ui/projects_page.go)) shells out to `git` and `gh` binaries — directly contradicting the "go-git over git binary" decision and the no-terminal philosophy (it errors with "requires the GitHub CLI to be installed and logged in"). Do it through go-git push + a token flow, or cut the feature; it is currently the only PATH-dependent code in the binary.
- ~130 lines of repo-lifecycle plumbing (`copyTreeWithoutGit`, `copyFile`, `listRepoFiles`, `copyProjectMemoryRepo`) live in the HTTP handler package; they belong in `internal/memory` next to `CreateMissingProjectRepo`, where `prepareProjectMemoryRepo` — currently duplicated between this file and [runtime/setup.go:36-60](../internal/runtime/setup.go) — can live once.
- `/projects?activate=`, `?hide=`, `?unhide=` are **state-changing GETs** ([projects_page.go:81-107](../internal/ui/projects_page.go)); `activate` triggers a full config re-apply and llama-server reload from a link a browser may prefetch, and unlike hide/unhide it does not redirect, so refresh re-activates. Make them POSTs.

**S5. `ui.Server` internals (extends O5).**
One `atomic.Pointer[uiDeps]` snapshot, swapped whole, would replace the ~17 mutex/setter/getter triples ([ui.go:118-186](../internal/ui/ui.go)), delete ~250 lines, and fix the torn-swap window where a request mid-rebuild observes a mix of old and new adapters. The eleven near-identical `template.ParseFS` blocks ([ui.go:196-249](../internal/ui/ui.go)) should be a loop over page names into a map.

**S6. `internal/memory`'s capability-interface lattice (extends O6).**
`Reader` + five optional interfaces, discovered by type assertion at call sites ([adapters.go:47](../internal/runtime/adapters.go), the anonymous `interface{ Reader; Walker }` at [memory_api.go:130](../internal/runtime/memory_api.go)), for two production implementations that both implement everything. A single `memory.Repo` interface also deletes downstream contortions like the 30-line glob-based `countEpisodeFiles` fallback ([session.go:556-590](../internal/session/session.go)) that exists only for stub readers in tests.

---

## 8. New code-level findings

- **N1.** `taskRunnerAdapter.RunTask` special-cases `len(msgs) == 1` with an unconditional `Append` ([adapters.go:613-620](../internal/runtime/adapters.go)); `appendUserSide` already handles that case, and the special branch can double-append the user turn onto a resumed session. Delete the branch.
- **N2.** Hand-rolled bubble sort in `sortChildren` ([ui/memory.go:801-816](../internal/ui/memory.go)) with a comment arguing `sort.Slice` is overkill — the justification is longer than the `sort.SliceStable` call it avoids, in a file that already uses `sort.Slice` three times.
- **N3.** Client construction duplicated: `embedder.NewClient(...)` built twice per rebuild ([memory_api.go:71, 288](../internal/runtime/memory_api.go)); `inference.NewClient(...)` in three places across lifecycle/memory_api/config. Build once per apply, pass down.
- **N4.** `tools.Registry.Schemas()` ([tools.go:101-115](../internal/tools/tools.go)) has no production caller — the loop engine rebuilds its own schema slice **every turn** ([loop.go:171-184](../internal/agentloop/loop.go)). Delete `Schemas` or use it, and hoist the per-turn rebuild out of the loop.
- **N5.** `web_search` dead type-assertion: tool args come from `json.Unmarshal` into `map[string]any`, so `max_results` is always `float64`; the `int` branch ([tools.go:372](../internal/tools/tools.go)) is dead, and the clamps collapse to `min(max(n,1),5)`.
- **N6.** `newBasePage` performs a projects `List()` plus a full config `Load()` (two SQLite round-trips) on **every page render** just for the nav header ([ui.go:574-607](../internal/ui/ui.go)); the state snapshot already carries `ProjectSlug`, and the names list changes only on project CRUD.
- **N7.** `os.Stdout = os.Stderr` in [main.go:47](../cmd/harness/main.go) is an uncommented global mutation — future readers will assume it's a bug. Comment the intent or route the offending writer properly.
- **N8.** `slugFromName`'s `invariant` variable ([projects_page.go:563-582](../internal/ui/projects_page.go)) means "last rune was alphanumeric" but reads as a maths term; rename (`lastWasDash`) or reuse a shared kebab-case helper.
- **N9.** `queue.Enqueue` holds the exclusive `enqMu` across a WAL fsync ([queue.go:154-169](../internal/queue/queue.go)); `RLock` suffices (as `enqueueReplay` already demonstrates) since `walMu` serializes the file. Moot if O1's WAL is removed.
- **N10.** Session-manager path helpers ([session.go:160-186](../internal/session/session.go)) are `fmt.Sprintf` wrappers that no longer vary post-M9 (`sessionsLogRelPath` returns the literal `"sessions.jsonl"`); fold into constants. The M3-era `session.Project` compatibility constant ([session.go:39](../internal/session/session.go)) is likely deletable.
- **N11.** `proc.Manager.Reconfigure` duplicates `Restart`'s body ([proc.go:183-213](../internal/proc/proc.go)); implement as "store new args, then `Restart()`".
- **N12.** `LayoutV2Reader.listRootDirs` sorts an already-sorted literal ([layout_v2.go:147-151](../internal/memory/layout_v2.go)). Moot under S1.

---

## 9. Revised order of work

1. **Clear the standing bugs first — C1, C2+C3, C6, A5.** Three of them live in exactly the files the refactors below touch; refactoring around a known bug bakes it in. (C1 in particular sits on every refactoring path through `runtime` and `queue`.)
2. **Dead-code deletion pass** (~800 lines, no behavior change): `/chat/stream` stack (O4), `Repo.Log`/`BlobByRef` (O3), `Registry.Schemas` (N4), the hot-reload watcher (O2), and — after an honest decision on D1 — the WAL/replay machinery (O1, now on by default).
3. **`ui.Server` deps snapshot + template loop** (S5) — mechanical, test-covered, fixes the torn-swap window as a side effect.
4. **Extract `internal/retrieval`** (S2 + S3) — unifying the two blend implementations fixes C3 by construction.
5. **Retire the logical-path layer** (S1) — the biggest item; do it before M10 pipelines are built on the logical dialect.
6. **Projects-page hygiene** (S4): POST-ify mutating actions, move repo copy/scaffold code into `internal/memory`, decide the fate of the `gh`-CLI backup feature.
7. Fold in the N-series items opportunistically as their files are touched.
