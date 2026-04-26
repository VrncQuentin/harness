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

- [ ] Agent registry: named agents with their own persona file path
- [ ] Prompt Assembler: layer ordering (`rules → user → persona → facts → notes → episodes → conversation`)
- [ ] Total memory cap + conversation reserve enforcement, episode trim oldest-first
- [ ] Qwen3 prompt template formatting
- [ ] Hot-reload: fsnotify on rules and persona files, no restart needed
- [ ] API Server: OpenAI-compatible `/v1/chat/completions` (streaming), disabled by default
- [ ] UI: agents page — switch active agent, view active persona

**Acceptance tests:**
- [ ] Send a request with agent `coder` → response reflects coder persona, not default
- [ ] Switch active agent to `reviewer` via UI → next request uses reviewer persona
- [ ] Edit `global/rules.md` on disk → next request without restarting reflects the change
- [ ] Edit `agents/coder/persona.md` on disk → next request without restarting reflects the change
- [ ] Construct a context that would exceed ctx_size → episodes are trimmed oldest-first until it fits, rules and persona are untouched
- [ ] Set `memory_token_budget` to a small value → assembler respects it, does not overflow
- [ ] Enable API server → `curl /v1/chat/completions` returns a valid streaming response
- [ ] Disable API server in config → port is not open, connection refused

---

## M3 — Memory Foundation

**Goal:** sessions are summarized and committed. Recency retrieval works.

- [ ] Git Backend: go-git wrapper, commit, log query, blob fetch
- [ ] Startup check: if `memory.repo_path` unset or path missing, refuse to start and prompt user to set it or run `init-memory <path>`
- [ ] `init-memory <path>` command: explicit one-time scaffold of directory structure + initial commit
- [ ] Session lifecycle: on-end → summarize via Qwen → write episode file → commit
- [ ] `runtime/sessions.jsonl`: append-only session log
- [ ] Recency retrieval: inject last N episodes on session start
- [ ] UI: memory browser page — episode list by agent/date, view episode content

**Acceptance tests:**
- [ ] Start harness with no `memory.repo_path` set → startup refuses with actionable error message
- [ ] Start harness with a path that does not exist → startup refuses with actionable error message
- [ ] Run `init-memory ~/memory` → directory structure is created, git repo initialized, initial commit present
- [ ] Complete a session → episode file appears in `agents/<n>/episodes/`, committed to git
- [ ] Episode commit message matches format `[agent:x] [type:episode] ...`
- [ ] Start a new session → previous episode content appears in the assembled prompt
- [ ] Complete 10 sessions → all 10 episode files present in git log, `sessions.jsonl` has 10 entries
- [ ] Corrupt `sessions.jsonl` by appending garbage → harness starts without crashing, logs a warning
- [ ] UI memory browser → lists episodes for active agent, click one to view content

---

## M3b — Projects

**Goal:** introduce project-scoped rules, agents, sessions, directories, and optional model overrides. The previous "no project" baseline is implemented as the always-present `global` project. Full design in [M3b.md](M3b.md).

Depends on M2 (agent registry, layered prompt) and M3 (memory repo, sessions, git backend). Indexing of project directories is staged here (layout + activation check); vector refresh lands in M5.

- [ ] `projects` and `project_directories` tables; system `global` row seeded on first run (cannot be hidden, deleted, or renamed)
- [ ] `config` additions: `active_project_slug` (NOT NULL, default `'global'`) and `project_llama_on_switch` (`'keep' | 'reload'`, default `'reload'`)
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

## M4 — opencode Integration

**Goal:** full agentic coding workflow via opencode, memory transparent to it.

- [ ] Harden API server for production use: timeouts, error responses, connection limits
- [ ] Verify opencode tool calling works end-to-end through the proxy
- [ ] Session tracking: correlate opencode sessions to harness session lifecycle
- [ ] UI: status page shows active sessions, logs page shows token counts per layer

**Acceptance tests:**
- [ ] Point opencode at harness API server → opencode connects and lists the model successfully
- [ ] Run a multi-step opencode task (read file, edit, run command) → all tool calls complete successfully
- [ ] Memory from a previous session is present in context during an opencode session (verify via logs page token breakdown)
- [ ] opencode session ends → episode file committed to git within 30 seconds
- [ ] Harness API server receives 10 concurrent requests → all complete, none dropped, queue behaves correctly
- [ ] opencode disconnects mid-stream → harness cleans up session state, next request succeeds
- [ ] UI logs page → shows correct token count for each prompt layer during an active session

---

## M5 — Semantic Memory

**Goal:** embedding-based retrieval blended with recency.

- [ ] Embedder sidecar: nomic-embed-text, health check, restart on crash
- [ ] Embed-on-commit pipeline: new episode → embed chunks → update `index/vectors.bin` + `index/manifest.json` → commit
- [ ] ANN search: flat scan initially, upgrade to usearch if latency becomes a problem
- [ ] Blended retrieval: `score = (semantic_weight * similarity) + (recency_weight * recency_decay)`
- [ ] Index rebuild command: walk commits, re-embed missing SHAs (idempotent)
- [ ] UI: memory browser shows retrieval scores per episode

**Acceptance tests:**
- [ ] Start embedder sidecar → appears healthy in UI status page
- [ ] Kill embedder → harness detects and restarts it, same as llama-server
- [ ] Complete a session → `index/vectors.bin` and `index/manifest.json` updated and committed
- [ ] Ask a question referencing content from session N-10 → that episode is retrieved despite not being the most recent
- [ ] Ask a question with no relevant past sessions → retrieval returns empty gracefully, no crash
- [ ] Run index rebuild on a fresh clone of the memory repo → index reconstructed correctly, retrieval works
- [ ] Set `semantic_weight = 0` → retrieval falls back to pure recency, top-K matches last N episodes exactly
- [ ] Set `recency_weight = 0` → retrieval is pure semantic, oldest relevant episode can appear in top-K
- [ ] UI memory browser → shows blended score next to each retrieved episode

---

## M6 — Memory Promotion + Cross-Agent

**Goal:** memory is actively curated, not just accumulated.

- [ ] `PromoteToGlobalFact(text)`: UI action → append to `global/facts.md` + commit
- [ ] `AppendAgentNote(agent, text)`: UI action → append to `agents/<n>/notes.md` + commit
- [ ] Cross-agent read: explicit API to pull episodes from another agent's directory
- [ ] Dedup pass on commit: detect near-duplicate facts before appending (embedding similarity threshold)
- [ ] UI: promotion controls in memory browser, cross-agent episode browser

**Acceptance tests:**
- [ ] Promote a fact via UI → text appears in `global/facts.md`, git commit present
- [ ] Promoted fact appears in the assembled prompt of the next session (verify via logs page)
- [ ] Promote a near-duplicate of an existing fact → dedup pass blocks it, user sees a warning
- [ ] Append a note to `agents/coder/notes.md` via UI → appears in next coder session prompt
- [ ] Request cross-agent episodes from `reviewer` while in a `coder` session → episodes injected correctly
- [ ] `global/facts.md` grows beyond a reasonable size → assembler still respects `memory_token_budget`, oldest facts are not silently dropped (warn instead)

---

## M7 — Native Agent Layer

**Goal:** opencode becomes optional. Harness owns tool execution.

- [ ] Tool calling protocol: OpenAI function calling format, parsed and dispatched by harness
- [ ] Built-in tools: file read, file write, shell exec, web search
- [ ] Sandboxing: working directory scoping, no writes outside project root
- [ ] Agentic loop: model calls tool → harness executes → result injected → next turn
- [ ] UI: tool call display, approval controls for destructive tools
- [ ] Config: enable/disable individual tools, sandbox root path

**Acceptance tests:**
- [ ] Model calls `file_read` on a file within sandbox root → content returned correctly
- [ ] Model calls `file_read` on a path outside sandbox root → rejected with clear error, not executed
- [ ] Model calls `file_write` → file is written, change visible on disk
- [ ] Model calls `shell_exec` → command runs, stdout/stderr returned to model
- [ ] Model calls `shell_exec` with a destructive command (`rm -rf`) → approval required in UI before execution
- [ ] Disable `shell_exec` in config → model receives a tool-not-available error, harness does not crash
- [ ] Complete a multi-step task (read file, edit, run tests) without opencode running → task completes end-to-end
- [ ] Tool call fails (file not found, command exits non-zero) → error injected into context, model can recover

---

## M8 — Hardening

**Goal:** daily driver reliability, observable, packaged.

- [ ] Add remaining metrics: TTFT, token throughput, VRAM usage (nvidia-smi polling)
- [ ] Optional Prometheus endpoint exposing all SQLite-backed metrics
- [ ] Full test suite: inference mock, memory read/write, retrieval scoring, prompt assembly
- [ ] Single binary packaging: harness + embedded UI assets, Windows native
- [ ] Embedder binary: self-contained, no Python dependency
- [ ] Graceful shutdown: drain queue, flush WAL, clean process teardown
- [ ] Startup validation: config checks, model file exists, memory repo accessible

**Acceptance tests:**
- [ ] Run full test suite → all pass, no flaky tests
- [ ] Build single binary on Windows native → runs correctly
- [ ] Start harness, send 50 sequential requests → TTFT, throughput, and VRAM metrics visible in UI
- [ ] Send SIGTERM → harness drains in-flight requests, commits any pending session, exits cleanly
- [ ] Send SIGKILL → on next start, WAL is replayed, no data lost
- [ ] Start with a corrupted `harness.db` → clear error on the status page, no crash
- [ ] Start with valid config but wrong model path → clear error at startup, not at first request
- [ ] Enable Prometheus endpoint → `curl /metrics` returns valid Prometheus text format
