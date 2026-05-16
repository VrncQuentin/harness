# Roadmap

Each milestone ends with a usable, stable state. Don't start the next until all acceptance tests pass.

---

## M1 — Inference Core

**Goal:** model runs, requests go through, harness owns the process.

- [x] Config store (SQLite, single-row typed table in `harness.db`): model path, ctx size, GPU layers, etc., edited via the `/config` page
- [x] Process Manager: spawn llama-server, health check loop, restart with backoff
- [x] Inference Client: OpenAI-compatible HTTP, streaming, cancellation
- [x] Queue: bounded channel, backpressure, WAL for crash recovery
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
- [x] Qwen3 prompt template formatting
- [x] Hot-reload: fsnotify on rules and persona files, no restart needed
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

M3 stages the project-scoped layout that M3b later formalizes: sessions, episodes, queue WAL, and indexes live under `projects/global/` from day one (`global` is a hardcoded slug at this milestone; the `projects` table and multi-project plumbing are introduced in M3b). Top-level `agents/<n>/` holds definition only — episodes live under the project.

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

**Goal:** introduce project-scoped rules, agents, sessions, directories, and optional model overrides. The previous "no project" baseline is implemented as the always-present `global` project. Full design in [M3b.md](M3b.md).

Depends on M2 (agent registry, layered prompt) and M3 (memory repo, sessions, git backend). Indexing of project directories is staged here (layout + activation check); vector refresh lands in M5.

- [x] `projects` and `project_directories` tables; system `global` row seeded on first run (cannot be hidden, deleted, or renamed)
- [x] `config` additions: `active_project_slug` (NOT NULL, default `'global'`) and `project_llama_on_switch` (`'keep' | 'reload'`, default `'reload'`)
- [ ] Memory repo layout: top-level `runtime/` and `index/` fold into `projects/global/`; episodes move out of `agents/<name>/episodes/` into `projects/<slug>/episodes/<agent-name>/` (agent dirs become definition-only); user projects live at `projects/<slug>/{rules.md, agents/, sessions.jsonl, queue.wal, episodes/<agent-name>/, index/<dir-slug>/}`
- [ ] Prompt assembler: new `projects/<slug>/rules.md` layer between global rules and agent persona
- [ ] Agent resolution: per-file override of the global agents library by `projects/<slug>/agents/<name>/`
- [ ] Activation: eager git-repo check on configured directories (warn-and-continue), fresh session, conditional llama-server swap based on `llama_on_switch`
- [ ] UI: `/projects` page (CRUD + hide), topbar switcher with `Global` always present, project-aware `/agents`, mismatch indicator on status page when `keep` causes a model/preference divergence

**Acceptance tests:** see [M3b.md](M3b.md#acceptance-tests). Highlights:

- [ ] First run seeds the `global` project; `global` cannot be hidden, deleted, or renamed
- [ ] Switching projects with `llama_on_switch = reload` drains the queue and reloads the llama-server with the destination's effective model; identical effective configs are a no-op regardless of mode
- [ ] Project agent overrides resolve per-file (project `persona.md` + global `rules.md` works)
- [ ] Activating a project with a missing directory succeeds and surfaces a "directory missing" badge
- [ ] Indexable trees produce manifest entries under `projects/<slug>/index/<dir-slug>/`; vector refresh deferred to M5

---

## M4 — Native Agent Loop MVP

**Goal:** the harness owns agentic tool execution through a first-party loop engine and browser task surface.

Design references: opencode (part-based messages, step counter, doom-loop detection, compaction-first loops, tool id/schema/execute/context contract, layered permissions and once/always/reject approvals, abort propagation) and Pi (minimal loop, minimal built-in tools, steering/follow-up queues, tree sessions, small prompts, extension hooks). These are references only; neither is a runtime dependency or integration milestone.

- [ ] Chat/task surface: first-party browser UI for task input and conversation display; no external chat client needed
- [ ] Loop engine: send conversation to the model, parse the response, dispatch tool calls, inject results, and repeat until stop/limit/cancel
- [ ] OpenAI-style tool-call parsing for streaming and non-streaming chat completion responses
- [ ] Part-based message model: text, tool_call, and tool_result parts with durable state for UI display and session replay
- [ ] Tool registry and schema contract: tools declare id, JSON Schema parameters, execute function, and context (active project, sandbox roots, caller identity)
- [ ] Read-only file tools first: `file_read` and `file_list`; no writes, edits, shell execution, or web search in the MVP
- [ ] Sandbox rooting: all file operations scoped to active project directories; paths outside those roots are rejected
- [ ] Step limit and doom-loop detection for repeated identical tool calls or response patterns
- [ ] Cancellation and abort propagation through the loop, current tool call, and in-flight model request
- [ ] Visibility: UI logs/token breakdown show loop turn count, tool calls, tool results, and loop termination reason
- [ ] Config: `loop_max_turns`, `loop_doom_threshold`, and per-tool enable/disable toggles

**Deferred to M7:** destructive tools (`file_write`, file edit, shell execution), approvals, richer permissions, web search, steering/follow-up queues, extension hooks, and sub-agents.

**Acceptance tests:**
- [ ] Open the task UI, enter a prompt -> conversation appears in the chat surface, model response streams in
- [ ] Model calls `file_read` on a path within the active project's sandbox root -> content is returned and injected into context
- [ ] Model calls `file_read` on a path outside any configured sandbox root -> request is rejected with a clear error and the rejection is visible to the model
- [ ] Loop hits the step limit -> terminates gracefully and the UI shows the limit was reached
- [ ] Model repeats the same tool call three times in a row -> loop terminates and the UI shows the doom-loop event
- [ ] Click cancel mid-task -> in-flight model request is aborted, running tool call is cancelled, loop terminates, UI returns to idle
- [ ] Disable `file_list` in config -> model receives a tool-not-available result when it calls it, loop continues
- [ ] Complete a multi-turn task (read several files, answer a question about them) entirely inside the harness UI

---

## M5 — Semantic Memory

**Goal:** embedding-based retrieval blended with recency.

- [ ] Embedder sidecar: nomic-embed-text, health check, restart on crash
- [ ] Embed-on-commit pipeline (episodes): new episode -> embed chunks -> update `projects/<active>/index/_episodes/{vectors.bin, manifest.json}` -> commit
- [ ] Embed-on-commit pipeline (attached directories): for each tree configured on the active project, walk by HEAD -> embed chunks -> update `projects/<active>/index/<dir-slug>/{vectors.bin, manifest.json}` -> commit
- [ ] ANN search: flat scan initially, upgrade to usearch if latency becomes a problem
- [ ] Blended retrieval: `score = (semantic_weight * similarity) + (recency_weight * recency_decay)`
- [ ] Index rebuild (UI-triggered from memory browser): walk commits, re-embed missing SHAs (idempotent), per-tree
- [ ] UI: memory browser shows retrieval scores per episode

**Acceptance tests:**
- [ ] Start embedder sidecar -> appears healthy in UI status page
- [ ] Kill embedder -> harness detects and restarts it, same as llama-server
- [ ] Complete a session -> `projects/<active>/index/_episodes/{vectors.bin, manifest.json}` updated and committed
- [ ] Ask a question referencing content from session N-10 -> that episode is retrieved despite not being the most recent
- [ ] Ask a question with no relevant past sessions -> retrieval returns empty gracefully, no crash
- [ ] Run index rebuild on a fresh clone of the memory repo -> index reconstructed correctly, retrieval works
- [ ] Set `semantic_weight = 0` -> retrieval falls back to pure recency, top-K matches last N episodes exactly
- [ ] Set `recency_weight = 0` -> retrieval is pure semantic, oldest relevant episode can appear in top-K
- [ ] UI memory browser -> shows blended score next to each retrieved episode

---

## M6 — Memory Promotion + Cross-Agent

**Goal:** memory is actively curated, not just accumulated.

- [ ] `PromoteToGlobalFact(text)`: UI action -> append to `global/facts.md` + commit
- [ ] `AppendAgentNote(agent, text)`: UI action -> append to `agents/<n>/notes.md` + commit
- [ ] Cross-agent read: explicit API to pull episodes from another agent's directory
- [ ] Dedup pass on commit: detect near-duplicate facts before appending (embedding similarity threshold)
- [ ] UI: promotion controls in memory browser, cross-agent episode browser

**Acceptance tests:**
- [ ] Promote a fact via UI -> text appears in `global/facts.md`, git commit present
- [ ] Promoted fact appears in the assembled prompt of the next session (verify via logs page)
- [ ] Promote a near-duplicate of an existing fact -> dedup pass blocks it, user sees a warning
- [ ] Append a note to `agents/coder/notes.md` via UI -> appears in next coder session prompt
- [ ] Request cross-agent episodes from `reviewer` while in a `coder` session -> episodes injected correctly
- [ ] `global/facts.md` grows beyond a reasonable size -> assembler still respects `memory_token_budget`, oldest facts are not silently dropped (warn instead)

---

## M7 — Agent Tools + Permissions Hardening

**Goal:** expand the native loop from read-only inspection to safe code-changing workflows.

- [ ] Destructive tools: `file_write`, file edit/patch, and shell execution
- [ ] Approval flow: once/always/reject decisions for destructive tools and external-directory access
- [ ] Layered permissions: agent defaults -> user config -> session approvals, with last-match-wins evaluation
- [ ] Shell safety: working directory scoping, command timeouts, output truncation, and explicit destructive-command classification
- [ ] Web search: opt-in tool with clear network-use disclosure and per-tool disable toggle
- [ ] Steering/follow-up queues: user can redirect the active loop after the current tool or enqueue follow-up instructions after completion
- [ ] Extension hooks: documented Go interfaces around loop start/end, tool start/end, and compaction boundaries
- [ ] Optional sub-agent/task tool with recursion limits and inherited deny rules
- [ ] UI: approval cards, tool history, retry failed tool call, and audit trail per session

**Acceptance tests:**
- [ ] Model calls `file_write` within sandbox root -> file is written and visible on disk
- [ ] Model calls file edit outside sandbox root -> rejected before touching disk
- [ ] Model calls `shell_exec` with a safe command -> stdout/stderr returned to the model
- [ ] Model calls `shell_exec` with a destructive command (`rm -rf`) -> approval required before execution
- [ ] User selects reject in approval UI -> tool result is a denial, loop can recover
- [ ] User selects always for a matching pattern -> next matching tool call proceeds without asking
- [ ] Disable `shell_exec` in config -> model receives a tool-not-available result, harness does not crash
- [ ] Complete a multi-step code task (read file, edit, run tests) entirely inside the harness UI
- [ ] Tool call exits non-zero -> error is injected into context and the model can recover

---

## M8 — Hardening

**Goal:** daily-driver reliability, observability, and Windows-native packaging.

- [ ] Add remaining metrics: TTFT, token throughput, VRAM usage (nvidia-smi polling), loop turn count, tool call count/error rate
- [ ] Optional Prometheus endpoint exposing all SQLite-backed metrics
- [ ] Full test suite: inference mock, memory read/write, retrieval scoring, prompt assembly, agent loop, tool sandbox, approvals
- [ ] Single binary packaging: harness + embedded UI assets, Windows native
- [ ] Embedder binary: self-contained, no Python dependency
- [ ] Graceful shutdown: drain queue, flush WAL, cancel active loops, commit any pending session, clean process teardown
- [ ] Startup validation: config checks, model file exists, memory repo accessible, active project references valid directories

**Acceptance tests:**
- [ ] Run full test suite -> all pass, no flaky tests
- [ ] Build single binary on Windows native -> runs correctly
- [ ] Start harness, send 50 sequential requests -> TTFT, throughput, VRAM, loop, and tool metrics visible in UI
- [ ] Send SIGTERM -> harness drains in-flight requests, cancels active loops safely, commits any pending session, exits cleanly
- [ ] Send SIGKILL -> on next start, WAL is replayed, no data lost
- [ ] Start with a corrupted `harness.db` -> clear error on the status page, no crash
- [ ] Start with valid config but wrong model path -> clear error at startup, not at first request
- [ ] Enable Prometheus endpoint -> `curl /metrics` returns valid Prometheus text format
