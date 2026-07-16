# Roadmap

Each milestone ends with a usable, stable state. Don't start the next until all acceptance tests pass.

Implementation checkboxes track code that has landed. Acceptance-test checkboxes stay unchecked unless the test was explicitly run and observed passing; when package-level automated tests cover the acceptance behavior, the checkbox may be checked and annotated with the test scope. Browser, OS, hardware, and live-model checks stay unchecked until exercised end-to-end.

Windows native and Linux are equal first-class targets. CI must run the Go test suite on both OSes for every PR, and platform-specific milestone acceptance tests must be verified on the OS they exercise.

---

## M1 — Inference Core

**Goal:** model runs, requests go through, harness owns the process.

- [x] Config store (SQLite, single-row typed table in `harness.db`): model path, ctx size, GPU layers, etc., edited via the `/config` page
- [x] Process Manager: spawn llama-server, health check loop, restart with backoff
- [x] Inference Client: OpenAI-compatible HTTP, streaming, cancellation
- [x] Queue: bounded in-process channel with backpressure; interactive requests are not crash-replayed
- [x] Process Manager: same pattern for Embedder sidecar (stub for now)
- [x] Minimal UI server: single status page showing model health and queue depth
- [x] System tray: single-instance lock, tray icon with Open UI + Quit, graceful shutdown on Quit
- [x] On first launch: browser opens automatically to UI; on subsequent double-click: do nothing
- [x] All startup errors (missing config, missing model file) surface in browser UI, not terminal

**Acceptance tests:**
- [x] Start harness with valid config → llama-server process appears in OS process list, tray icon visible
- [x] Double-click binary while already running → second instance exits silently, first instance unaffected
- [x] Kill llama-server manually → harness detects death within health check interval and restarts it
- [x] Send a streaming completion request → tokens arrive incrementally, request completes successfully
- [x] Fill the queue to max depth → next request returns a clear backpressure error, not a hang
- [x] Cancel a mid-flight request → llama-server is not left in a broken state, next request succeeds
- [x] Start harness with missing model file → browser status page shows error, binary does not crash
- [x] Start harness on first run (no saved config) → status page shows "Set up your harness" CTA linking to /config
- [x] Click Quit in system tray → in-flight requests drain, child processes terminate, binary exits cleanly
- [x] Open browser status page → shows llama-server as healthy and queue depth as 0
- [x] Kill llama-server → status page updates to unhealthy within one health check interval

---

## M2 — Prompt Assembly

**Goal:** layered system prompt, agent personas, hot-reload.

- [x] Agent registry: named agents with their own persona file path
- [x] Prompt Assembler: layer ordering (`rules → user → persona → facts → notes → episodes → conversation`)
- [x] Total memory cap + conversation reserve enforcement, episode trim oldest-first
- [x] Chat template delegation: assembler returns OpenAI-style chat messages; llama-server applies the model-specific template
- [x] Hot-reload: rules and persona files are re-read without restart
- [x] API Server: OpenAI-compatible `/v1/chat/completions` (streaming), disabled by default
- [x] UI: agents page — switch active agent, view active persona

**Acceptance tests:**
- [x] Send a request with agent `coder` → response reflects coder persona, not default
- [x] Switch active agent to `reviewer` via UI → next request uses reviewer persona
- [x] Edit `global/rules.md` on disk → next request without restarting reflects the change
- [x] Edit `agents/coder/persona.md` on disk → next request without restarting reflects the change
- [x] Construct a context that would exceed ctx_size → episodes are trimmed oldest-first until it fits, rules and persona are untouched
- [x] Set `memory_token_budget` to a small value → assembler respects it, does not overflow
- [x] Enable API server → `curl /v1/chat/completions` returns a valid streaming response
- [x] Disable API server in config → port is not open, connection refused

---

## M3 — Memory Foundation

**Goal:** sessions are summarized and committed. Recency retrieval works.

M3 stages the project-scoped layout that M3b later formalizes: sessions, episodes, and indexes live under `projects/global/` from day one (`global` is a hardcoded slug at this milestone; the `projects` table and multi-project plumbing are introduced in M3b). Top-level `agents/<n>/` holds definition only — episodes live under the project.

- [x] Git Backend: go-git wrapper, commit, log query, blob fetch
- [x] Session lifecycle: on-end → summarize via Qwen → write episode file → commit
- [x] `projects/global/sessions.jsonl`: append-only session log
- [x] Recency retrieval: inject last N episodes on session start
- [x] UI: memory browser page — episode list by agent/date, view episode content

**Acceptance tests:**
- [x] Complete a session → episode file appears at `projects/global/episodes/<agent>/<timestamp>.md`, committed to git
- [x] Episode commit message matches format `[agent:x] [type:episode] ...`
- [x] Start a new session → previous episode content appears in the assembled prompt
- [x] Complete 10 sessions → all 10 episode files present in git log, `projects/global/sessions.jsonl` has 10 entries
- [x] Corrupt `projects/global/sessions.jsonl` by appending garbage → harness starts without crashing, logs a warning
- [x] UI memory browser → lists episodes for active agent, click one to view content

---

## M3b — Projects

**Goal:** introduce project-scoped rules, agents, sessions, directories, and optional model overrides. The previous "no project" baseline is implemented as the always-present `global` project.

Depends on M2 (agent registry, layered prompt) and M3 (memory repo, sessions, git backend). M3b records attached directories and validates them on activation; attached-directory indexing is deferred until directory-level semantic search becomes user-facing.

**Scoped contract:**

- A project is identified by an immutable lowercase-dashed `slug` and editable display name. Filesystem paths and DB keys always use the slug.
- The `global` project is seeded on first run, is always active by default, and cannot be hidden, deleted, or renamed by slug.
- M3b originally placed project memory under the then-current single memory repo at `projects/<slug>/{rules.md, agents/, sessions.jsonl, episodes/<agent-name>/, index/}`. M9 layout-v2 keeps the same logical project boundaries but stores each project as its own physical git repo under `~/.harness/projects/<slug>/`.
- The `global` project is the default active project, not an always-injected base layer. Its `agents/<name>/persona.md` and `agents/<name>/rules.md` files are the fallback agent-definition library for other projects; rules/user/facts/notes/episodes are otherwise project-local.
- Prompt assembly reads `rules.md`, `user.md`, `facts.md`, notes, and episodes from the active project repo. Agent `persona.md` and `rules.md` resolve per file from the active project repo, falling back to the global agent-definition library; `notes.md` does not fall back.
- Sessions are bound immutably to their project, append to that project's `sessions.jsonl`, and write episodes to that project's `episodes/<agent-name>/` directory.
- `project_llama_on_switch = reload` drains the queue and restarts llama-server with the destination project's effective model unless the effective config is unchanged. `keep` leaves llama-server running and surfaces any running/preferred model mismatch.
- Project directories are absolute paths to git repos. Activation warns and continues when a directory is missing or invalid; attached-directory semantic indexes are deferred.
- Per-project model overrides cover `model_binary`, `model_path`, `model_ctx_size`, `model_gpu_layers`, and `model_n_parallel`; null values inherit from global config.

- [x] `projects` and `project_directories` tables; system `global` row seeded on first run (cannot be hidden, deleted, or renamed)
- [x] `config` additions: `active_project_slug` (NOT NULL, default `'global'`) and `project_llama_on_switch` (`'keep' | 'reload'`, default `'reload'`)
- [x] Memory repo layout: top-level `runtime/` and `index/` fold into `projects/global/`; episodes move out of `agents/<name>/episodes/` into `projects/<slug>/episodes/<agent-name>/` (agent dirs become definition-only)
- [x] Prompt assembler: project-scoped `rules.md`, `user.md`, `facts.md`, notes, and episodes with global fallback only for agent persona/rules
- [x] Agent resolution: per-file override of the global agents library by `projects/<slug>/agents/<name>/`
- [x] Activation: eager git-repo check on configured directories (warn-and-continue), fresh session, conditional llama-server swap based on `llama_on_switch`
- [x] UI: `/projects` page (CRUD + hide), topbar switcher with `Global` always present, project-aware `/agents`, mismatch indicator on status page when `keep` causes a model/preference divergence

**Acceptance tests:**

- [x] First run seeds the `global` project; `global` cannot be hidden, deleted, or renamed
- [ ] Create a project via UI -> row appears in `projects` table, project memory repo is available, slug is auto-generated from display name and editable on create
- [ ] Edit a project -> display name and overrides change, slug is read-only
- [ ] Activate a project -> `active_project_slug` updates, a fresh session opens in that project repo's `sessions.jsonl`, and the session record includes `project: <slug>`
- [ ] Switching projects with `llama_on_switch = reload` drains the queue and reloads llama-server with the destination's effective model; identical effective configs are a no-op regardless of mode
- [ ] Switch with `llama_on_switch = keep` -> llama-server keeps running and the status page shows model mismatch when applicable
- [ ] Switch to the global project from a user project -> subsequent sessions write to the global repo's `sessions.jsonl` with `project: global`, and only the global agents library is visible
- [x] Project agent overrides resolve per-file (project `persona.md` + global `rules.md` works)
- [x] Activating a project with a missing directory succeeds and surfaces a "directory missing" badge
- [x] Project-only agent in the active project repo is visible only inside that project (automated: `TestAssemble_ProjectOnlyAgentSuccess`, `TestAssemble_ProjectOnlyAgentNotLeaking`)
- [x] Project prompt files present -> active repo `rules.md`, `user.md`, and `facts.md` are injected without leaking global project files (automated: `TestAssemble_ProjectScopedLayersUseActiveProject`)
- [x] Complete a session in a project repo -> episode file appears at `episodes/<agent>/<timestamp>.md`, not under `agents/<agent>/episodes/` (automated runtime wiring: `TestBuildSessionManagerUsesPhysicalProjectRepoPaths`)
- [x] Complete a session in the global project repo -> episode file appears at `episodes/<agent>/<timestamp>.md` (automated runtime wiring: `TestBuildSessionManagerUsesPhysicalProjectRepoPaths`)
- [x] Hide a non-global project -> it disappears from the topbar switcher and default `/projects` list, while data remains on disk (automated DB/UI handler coverage: `TestProjectStore_HideAndUnhide`, `TestHandleProjectVisibilityPOST`)
- [x] Unhide a project -> it reappears in pickers with data intact (automated DB/UI handler coverage: `TestProjectStore_HideAndUnhide`, `TestHandleProjectVisibilityPOST`)
- [ ] Restart harness -> `active_project_slug` is honored on startup, activation runs, eager directory checks execute, and the status page reflects the active project

Attached-directory index manifests are not part of scoped M3b; they remain deferred with attached-directory semantic indexing.

---

## M4 — Native Agent Loop MVP

**Goal:** the harness owns agentic tool execution through a first-party loop engine and browser task surface.

Depends on M3b (projects, active project directories, and sandbox roots).

Design references: opencode (part-based messages, step counter, doom-loop detection, compaction-first loops, tool id/schema/execute/context contract, layered permissions and once/always/reject approvals, abort propagation) and Pi (minimal loop, minimal built-in tools, steering/follow-up queues, tree sessions, small prompts, extension hooks). These are references only; neither is a runtime dependency or integration milestone.

- [x] Chat/task surface: first-party browser UI for task input and conversation display; no external chat client needed
- [x] Loop engine: send conversation to the model, parse the response, dispatch tool calls, inject results, and repeat until stop/limit/cancel
- [x] `/task` route uses the Prompt Assembler and Queue, so personas/memory apply and model calls are serialized/backpressured
- [x] Inference client parses OpenAI-style tool calls for streaming and non-streaming chat completion responses; the agent loop currently requests streaming completions
- [x] Part-based message model: text, tool_call, and tool_result parts are represented for UI display and session replay
- [x] Tool registry and schema contract: tools declare id, JSON Schema parameters, execute function, and context (active project, sandbox roots, session id, caller identity, cancellation context)
- [x] Read-only file tools first: `file_read` and `file_list` are enabled by default; destructive tools are disabled by default and gated by M7 approvals
- [x] Sandbox rooting: all file operations scoped to active project directories; paths outside those roots are rejected
- [x] Step limit and doom-loop detection for repeated identical tool calls or response patterns
- [x] Cancellation and abort propagation through the loop, current tool call, and in-flight model request
- [x] Visibility: task stream/session history records loop turns, tool calls, tool results, approval events, and termination reason
- [x] Config: `loop_max_turns`, `loop_doom_threshold`, and per-tool enable/disable toggles

**Still deferred beyond M4:** file edit/patch, web search, steering/follow-up queues, extension hooks, sub-agents, and richer long-term permissions UI.

**Acceptance tests:**
- [ ] Open the task UI, enter a prompt -> conversation appears in the chat surface, model response streams in
- [x] Model calls `file_read` on a path within the active project's sandbox root -> content is returned and injected into context (automated: `TestFileRead_WithinSandbox`)
- [x] Model calls `file_read` on a path outside any configured sandbox root -> request is rejected with a clear error and the rejection is visible to the model (automated: `TestFileRead_OutsideSandbox`, `TestTaskRunnerDoesNotUseMemoryRepoAsSandboxFallback`)
- [x] Loop hits the step limit -> terminates gracefully and emits the limit event/error (automated engine coverage: `TestEngineCachesToolSchemasAcrossTurns`)
- [ ] Model repeats the same tool call three times in a row -> loop terminates and the UI shows the doom-loop event
- [x] Click cancel mid-task -> active engine cancellation is routed and partial transcript is retained (automated route/runtime coverage: `TestHandleTaskCancel_Success`, `TestTaskRunnerCancelTaskCancelsActiveEngine`, `TestTaskRunnerRecordsPartialTranscriptOnCancel`)
- [x] Disable `file_list` in config -> model receives a tool-not-available result when it calls it, loop continues (automated disabled-tool coverage: `TestToolDisabledInConfigReturnsNotAvailable`)
- [ ] Complete a multi-turn task (read several files, answer a question about them) entirely inside the harness UI

---

## M5 — Semantic Memory

**Goal:** embedding-based retrieval blended with recency.

**Deferred:** Attached-directory indexing. The embed-on-commit pipeline for project
directory trees and the per-directory index rebuild require chunking files from
the git HEAD of each configured project directory. These depend on:
- Directory tree walking integrated with the git backend
- Chunking strategy for arbitrary file types
- UI controls per directory tree
This work is descoped from the current phase; project directory indexes will be
implemented when directory-level semantic search becomes a user-facing feature.

- [x] Embedder sidecar: llama-server --embedding mode, health check, restart on crash
- [x] Embed-on-commit pipeline (episodes): new episode -> embed chunks -> update the active project repo's `index/_episodes/{vectors.bin, manifest.json}` -> commit
- [ ] Embed-on-commit pipeline (attached directories): for each tree configured on the active project, walk by HEAD -> embed chunks -> update the active project repo's `index/<dir-slug>/{vectors.bin, manifest.json}` -> commit
- [x] ANN search: flat scan initially, upgrade to usearch if latency becomes a problem
- [x] Blended retrieval: `score = (semantic_weight * similarity) + (recency_weight * recency_decay)`
- [x] Index rebuild (UI-triggered from memory browser): walk episodes, re-embed missing SHAs (idempotent). **Directory indexing deferred** — project directory trees require chunking embedded files from the git HEAD, which depends on the attached-directory embed-on-commit pipeline. Episode index rebuild is implemented in `internal/runtime/memory_api.go:indexRebuilder`.
- [x] UI: memory browser shows retrieval scores per episode (indexed / not indexed badge)

**Acceptance tests:**
- [ ] Start embedder sidecar -> appears healthy in UI status page
- [ ] Kill embedder -> harness detects and restarts it, same as llama-server
- [ ] Complete a session -> the active project repo's `index/_episodes/{vectors.bin, manifest.json}` is updated and committed
- [x] Ask a question referencing content from session N-10 -> that episode is retrieved despite not being the most recent (automated blended scoring coverage: `TestAssemble_BlendedRetrievalKeepsTopN`, `TestAssemble_BlendedRetrievalTrimDropsLowestScore`)
- [ ] Ask a question with no relevant past sessions -> retrieval returns empty gracefully, no crash
- [x] Run index rebuild on a fresh clone of the memory repo -> episode index is reconstructed (automated: `TestEpisodeRebuilderCreatesMissingEpisodeIndex`, `TestIndexRebuilderCreatesMissingEpisodeIndex`)
- [x] Set `semantic_weight = 0` -> retrieval falls back to pure recency, top-K matches last N episodes exactly (automated: `TestAssemble_BlendedRecencyUsesExponentialDecay`)
- [x] Set `recency_weight = 0` -> retrieval is pure semantic, oldest relevant episode can appear in top-K (automated: `TestAssemble_BlendedRetrievalTrimDropsLowestScore`, `TestAssemble_BlendedRetrievalUsesBestChunkScore`)
- [x] UI memory browser -> shows blended score next to each retrieved episode (automated: `TestHandleMemoryEpisodes_RendersRetrievalScores`)

---

## M6 — Memory Promotion + Cross-Agent

**Goal:** memory is actively curated, not just accumulated.

- [x] `PromoteFact(text)`: UI action -> append to the active project `facts.md` + commit
- [x] `AppendAgentNote(agent, text)`: UI action -> append to `agents/<n>/notes.md` + commit
- [x] Cross-agent episode browser: view episodes for any agent from the memory page
- [x] Dedup pass on commit: detect near-duplicate facts before appending (embedding similarity threshold)
- [x] UI: promotion controls (promote fact + append note forms) in memory browser, cross-agent episode picker

**Descoped from M6:** Cross-agent episode injection into the prompt assembler. The
assembler currently retrieves episodes only for the active agent. Cross-agent
injection (e.g. loading `reviewer` episodes during a `coder` task) will be
implemented when the permission + retrieval architecture matures (M7+).

**Acceptance tests:**
- [x] Promote a fact via UI -> text appears in the active project `facts.md`, git commit present
- [x] Promoted fact appears in the assembled prompt of the next session (verify via logs page)
- [x] Promote a near-duplicate of an existing fact -> dedup pass blocks it, user sees a warning
- [x] Append a note to `agents/coder/notes.md` via UI -> appears in next coder session prompt
- [ ] Request cross-agent episodes from `reviewer` while in a `coder` session -> episodes injected correctly (descoped to M7+)
- [x] `facts.md` grows beyond a reasonable size -> assembler still respects `memory_token_budget`; facts/notes are not silently dropped and an over-budget warning is emitted (automated: `TestAssemble_MandatoryLayersNeverTrimmed`)

---

## M7 — Agent Tools + Permissions Hardening

**Goal:** expand the native loop from read-only inspection to safe code-changing workflows.

Consolidation Phase 5 landed the approval-gated core. The remaining M7 scope is
Windows-native shell execution, opt-in web search, and browser-level acceptance
verification for the approval-gated tool layer. File edit/patch, steering and
follow-up queues, extension hooks, optional sub-agents, and tool-history retry UI
are deferred beyond M7.

- [x] Destructive tools: `file_write` and `shell_exec` registered in the tool registry, disabled by default in config
- [x] Approval flow: allow once, always allow, and reject decisions for destructive tools
- [x] Layered permissions: builtin defaults -> user config -> session approvals, with later layers taking precedence
- [x] Shell guardrails: sandbox working directory validation, command timeout, output truncation, and explicit destructive-command classification
- [x] Windows-native shell execution path; `shell_exec` uses `cmd.exe` on Windows and `sh` on Unix
- [x] Web search: opt-in tool with clear network-use disclosure and per-tool disable toggle
- [x] UI: approval cards and approval audit trail per session

**Deferred beyond M7:**
- File edit/patch tool
- Steering/follow-up queues: user can redirect the active loop after the current tool or enqueue follow-up instructions after completion
- Extension hooks: documented Go interfaces around loop start/end, tool start/end, and compaction boundaries
- Optional sub-agent/task tool with recursion limits and inherited deny rules
- UI tool history and retry failed tool call

**Acceptance tests:**
- [ ] Model calls `file_write` within sandbox root -> file is written and visible on disk
- [ ] Model calls `shell_exec` with a safe command -> stdout/stderr returned to the model
- [ ] Model calls `shell_exec` with a destructive command (`rm -rf`) -> approval required before execution
- [ ] User selects reject in approval UI -> tool result is a denial, loop can recover
- [ ] User selects always for a matching pattern -> next matching tool call proceeds without asking
- [ ] Disable `shell_exec` in config -> model receives a tool-not-available result, harness does not crash
- [ ] Enable web search -> model can call the web-search tool, the UI/audit trail clearly discloses network use, and results are injected into context
- [ ] Disable web search in config -> model receives a tool-not-available result and the harness makes no network request
- [ ] Complete a multi-step code task (read file, write a sandboxed file, run tests) entirely inside the harness UI
- [ ] Tool call exits non-zero -> error is injected into context and the model can recover

**Unit coverage present:** `internal/approvals`, `internal/agentloop`, `internal/tools`, and `internal/ui` cover layered evaluation, Windows and Unix destructive shell classification, allow/reject/always decisions, disabled destructive/network toggles, web-search parsing/disclosure, native shell dispatch, and approval events. These are not a substitute for the browser-level acceptance tests above.

---

## M8 — Hardening

**Goal:** daily-driver reliability, observability, and native packaging.

- [x] Add remaining metrics: TTFT, token throughput, VRAM usage (nvidia-smi polling), loop turn count, tool call count/error rate
- [x] Optional Prometheus endpoint exposing all SQLite-backed metrics
- [x] Full test suite: inference mock, memory read/write, retrieval scoring, prompt assembly, agent loop, tool sandbox, approvals
- [x] Single binary packaging: harness + embedded UI assets
- [x] Embedder binary: self-contained, no Python dependency
- [x] Graceful shutdown: drain queue, cancel active loops, commit any pending session, clean process teardown
- [x] Startup validation: config checks, model file exists, memory repo accessible, active project references valid directories

**Acceptance tests:**
- [x] Run full test suite -> all pass, no flaky tests
- [ ] Build single binary -> runs correctly
- [ ] Start harness, send 50 sequential requests -> TTFT, throughput, VRAM, loop, and tool metrics visible in UI
- [ ] Send SIGTERM (Linux/headless) -> harness drains in-flight requests, cancels active loops safely, commits any pending session, exits cleanly
- [x] Send SIGKILL -> interactive queued requests are lost, but saved sessions and committed memory remain intact
- [ ] Start with a corrupted `harness.db` -> clear error on the status page, no crash
- [ ] Start with valid config but wrong model path -> clear error at startup, not at first request
- [x] Enable Prometheus endpoint -> `/metrics` returns valid Prometheus text format (automated HTTP handler coverage: `TestHandleMetrics_ExportsLatestPrometheusSamples`)

---

## M9 — Layout V2

**Goal:** move from one configured memory repo to a harness home with one git-backed memory repo per project, while keeping the harness global/resident and preserving SQLite for config, metrics, and runtime state. The shipped layout is documented in [architecture.md](architecture.md#harness-home-and-memory-repo-layout).

Depends on M3b (projects table, active project slug, attached directories), M5 (project-scoped indexes), and M8 (startup validation and reliable packaging).

- [x] Harness home: default `~/.harness/` with `harness.db`, `projects/`, `logs/`, and `cache/`
- [x] Global project repo: initialize `~/.harness/projects/global` as the default project repo containing project rules, user facts, promoted facts, the fallback agent-definition library, sessions, episodes, index, and artifacts
- [x] Project memory repos: one git repo per project, defaulting to `~/.harness/projects/<id>/`, with optional user-provided directories
- [x] Create-project flow: use existing git directory as-is, initialize non-git directory with `go-git`, or create the default directory and initialize it with `go-git`
- [x] GitHub backup flow removed: project creation remains local and dependency-free beyond harness-managed Go code
- [x] Path resolution: memory, session, queue, index, and artifact paths resolve relative to the active project memory repo instead of a shared memory repo root
- [x] Prompt layering: active project rules/user/facts/notes/episodes resolve from the active project repo; persona/rules definitions fall back to `projects/global/agents`
- [x] Legacy migration removed: there were no pre-M9 installs to migrate, so M9 starts directly with layout-v2 project repos
- [x] UI: create/edit project forms expose memory repo directory choice without adding cwd-driven activation

**Acceptance tests:**

- [x] First run creates `~/.harness/harness.db` and initializes `~/.harness/projects/global` as a git repo (automated DB/repo setup coverage: `TestOpen_CreatesTablesAndSeedsConfigRow`, `TestEnsureProjectRepoInitializesAndScaffolds`)
- [ ] Creating a project with no directory creates and initializes `~/.harness/projects/<id>`
- [x] Creating a project with a non-git directory initializes it through `go-git` (automated repo setup coverage: `TestEnsureProjectRepoInitializesAndScaffolds`)
- [x] Creating a project with an existing git directory uses it without rewriting unrelated files (automated repo setup coverage: `TestEnsureProjectRepoInitializesAndScaffolds`, `TestMoveProjectRepoCopiesWorkingTreeWithoutGitDir`)
- [ ] Starting the harness never depends on cwd and never activates a project based on the launch directory
- [ ] One project with two attached code repos writes sessions and episodes to one project memory repo and creates separate index entries for each attached repo
- [x] Agent persona/rules resolution falls back from active project agents to `projects/global/agents` per file; notes do not fall back (automated: `TestAssemble_ProjectPersonaOverrideInheritsGlobalDefinitionRulesOnly`)
- [ ] Pipeline discovery reads `.hp` files from attached code repos, not from project memory repos
- [ ] `harness.db`, logs, and cache files are never committed to any project memory repo

---

## M10 — Pipeline DSL

**Goal:** execute reviewed `.hp` pipeline specs inside the harness using the native agent loop, tool registry, project sandbox, and browser UI.

Depends on M7 (destructive tools, shell execution, approvals, and hardened permissions) and M9 (layout-v2 project memory repos and attached source repo semantics). The DSL contract lives in [DSL.md](DSL.md); the detailed implementation plan and acceptance tests live in [dsl_roadmap.md](dsl_roadmap.md).

- [ ] Isolated `internal/dsl` parser, validator, and linter package; editor and dry-run preview for attached-repo `.hp` specs
- [ ] Runtime execution through `internal/agentloop`, declared artifacts, verify/gate commands, retries, routes, and `lib` calls
- [ ] Durable SQLite run state, project-memory-repo artifacts, UI run graph, surfacing/resume controls, and M10 metrics
