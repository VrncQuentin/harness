# Architecture

## Overview

A local AI inference harness with git-backed project memory, layered prompt assembly, a browser-based management UI, a planned first-party native agent loop, and a planned declarative pipeline runner. The harness owns chat, tool-call orchestration, tool execution, and `.hp` pipeline execution locally; external coding agents are references for design patterns, not runtime dependencies.

The harness runs as a native desktop binary (Windows and Linux). It starts silently, opens the management UI in the default browser if not already open, and lives in the system tray until explicitly quit. The browser UI is the only user-facing surface — all errors (unconfigured on first run, missing model, llama-server failures, missing memory repo) are surfaced there, not in a terminal.

The binary targets llama-server as the inference backend and uses a separate embedding sidecar for semantic memory.

---

## Component Map

```
┌─────────────────────────────────────────────────────────┐
│           Browser (management UI)                        │
│ chat/tasks │ pipelines │ model status │ agents │ projects │ memory │ logs │
└───────────────────────┬─────────────────────────────────┘
                        │ HTTP (htmx + SSE)
┌───────────────────────▼─────────────────────────────────┐
│                    UI Server                             │
│         net/http + html/template + embed.FS             │
└───────────────────────┬─────────────────────────────────┘
                        │ internal Go API
         ┌──────────────┴──────────────┐
         │        System Tray          │
         │  (fyne-io/systray) — Quit   │
         └─────────────────────────────┘
┌───────────────────────▼─────────────────────────────────┐
│                       Core                               │
│                                                          │
│  ┌──────────────┐  ┌────────────────┐  ┌─────────────┐  │
│  │   Session    │  │    Prompt      │  │   Memory    │  │
│  │   Manager   │─▶│   Assembler    │─▶│   Store     │  │
│  └──────┬───────┘  └───────┬────────┘  └──────┬──────┘  │
│         │                  │                  │         │
│  ┌──────▼───────┐  ┌───────▼────────┐         ▼         │
│  │ Pipeline     │  │ Tool Registry  │   ┌────────────┐  │
│  │ Runner       │  │ + Sandbox      │   │ Git Backend│  │
│  │ (planned)    │  │ (planned)      │   └──────┬─────┘  │
│  └──────┬───────┘  └───────▲────────┘          │        │
│         ▼                  │                   ▼        │
│  ┌──────────────┐          │            ┌─────────────┐ │
│  │ Agent Loop   │──────────┘            │  Embedder   │ │
│  │ (planned)    │                       │  (nomic)   │ │
│  └──────┬───────┘                       └─────────────┘ │
│         │                                               │
│         ▼                                               │
│  ┌──────────────┐                                      │
│  │    Queue     │                                      │
│  └──────┬───────┘                                      │
│         │                                              │
│  ┌──────▼───────┐                                     │
│  │  Inference   │                                     │
│  │   Client     │                                     │
│  └──────┬───────┘                                     │
└─────────┼───────────────────────────────────────────────┘
          │ HTTP (OpenAI-compatible)
┌─────────▼────────────┐
│   llama-server       │  ← harness spawns, monitors, restarts
└──────────────────────┘
```

The browser UI is the primary chat/task surface and, once M10 lands, the primary pipeline execution surface. The optional OpenAI-compatible API server remains available for external clients, but first-party agent-loop and pipeline execution stay inside the harness.

---

## Components

### UI Server (`internal/ui`)
Lightweight browser interface for chat/tasks and management. Serves pre-rendered HTML fragments, no JavaScript framework, no build step.

Stack:
- `net/http` — request routing
- `html/template` — server-side rendering
- `embed.FS` — static assets (CSS, htmx) compiled into the binary
- htmx — dynamic updates via HTML attributes, no custom JS
- SSE — live log tailing and model health streaming

Pages:
- **Chat / Tasks** — first-party conversation and agent-loop task surface; streams model output, tool calls, tool results, and cancellation state
- **Pipelines** — planned `.hp` authoring, lint, dry-run preview, execution graph, surfacing, resume, and artifact browser
- **Status** — llama-server health, queue depth, VRAM usage, restart controls; errors (missing config, missing model, failed starts) displayed prominently here
- **Projects** — create, edit, hide, and switch active project once M3b lands
- **Agents** — switch active agent, edit persona and notes inline, trigger hot-reload
- **Memory** — browse episodes by agent/date, view retrieval scores, promote facts
- **Logs** — live process manager events, prompt assembly debug, loop turns, and tool-call traces

### System Tray (`internal/tray`)
Manages the binary's desktop presence. Uses `fyne-io/systray` (native on Windows and Linux, no CGO required for Windows; GTK-based on Linux).

Behavior:
- **On start:** check if another instance is already running (via Windows mutex on Windows, file lock on Linux); if so, do nothing and exit
- **If first instance:** start all services, open browser to UI if not already open, show tray icon
- **Tray icon menu:** Open UI, Quit
- **On Quit:** graceful shutdown — drain queue, flush WAL, terminate child processes, release lock

### Session Manager (`internal/session`)
Owns conversation lifecycle for the browser chat/task surface, the optional OpenAI-compatible API server, and the planned native agent loop.

- **On start:** resolve active agent → trigger memory read → assemble initial context
- **Per turn:** append to conversation history → call Prompt Assembler → send to Queue
- **On end:** call summarizer (Qwen) → write episode file → trigger git commit
- **Persistence:** append-only `sessions.jsonl` in the active project memory repo (defaults to `~/.harness/projects/global/` once layout-v2 lands)

### Agent Loop (`internal/agentloop`) — planned M4
Owns the first-party agentic turn loop. This package is planned separately from `internal/agent`, which remains the agent/persona registry.

Responsibilities:
- Maintain part-based messages (`text`, `tool_call`, `tool_result`) for UI display and session replay.
- Call the Prompt Assembler, submit requests through Queue/Inference Client, parse tool calls, dispatch tools, inject results, and continue until stop/limit/cancel.
- Enforce max turn count, doom-loop detection, cancellation propagation, and compaction triggers.
- Emit loop events to the UI logs/SSE stream and metrics store.

### Tool Registry + Sandbox (`internal/tools`) — planned M4/M7
Defines the local tool surface available to the agent loop.

Responsibilities:
- Register tools by id, JSON Schema parameters, description, and execute function.
- Pass a typed context to each tool: active project slug, sandbox roots, session id, caller identity, and cancellation context.
- M4 starts read-only (`file_read`, `file_list`) and rejects paths outside active project directories.
- M7 adds destructive tools, shell execution, web search, approvals, richer permissions, and extension hooks.

### Pipeline Runner (`internal/pipeline`) — planned M10
Owns parsing, validation, and execution of Harness Pipeline DSL specs (`.hp`). The language contract is documented in [DSL.md](DSL.md). Specs are declarative workflow files stored with the attached project git repos they operate on; the runner executes them through the same first-party agent loop, tool registry, queue, inference client, memory store, and UI event stream used by interactive tasks.

Language implementation is isolated under `internal/dsl` (`parser/`, `validate/`, `linter/`, `source/`, and core AST/diagnostic types) so it can be extracted later. `internal/dsl` does not import harness runtime packages; `internal/pipeline` is the harness-specific adapter.

Responsibilities:
- Load `.hp` specs from the active project's attached git directories and resolve imports relative to the source repo root.
- Parse and validate specs before a run starts: grammar, type bindings, route targets, import/call graph cycles, path safety, output declarations, and agent/model resolution.
- Render the dry-run preview shown by the UI: agents, models, steps, routes, verify/gate commands, declared outputs, optional bindings, and suspicious paths.
- Execute one step at a time by opening agent-loop sessions, applying declared bindings, validating declared outputs, running verify/gate argv commands, and resolving routes.
- Checkpoint surfaced runs with spec SHA, supplied agent args, reject counters, consumed artifact hashes, output hashes, and verify/gate output tails so the UI can resume safely.
- Store durable run state in SQLite, record source repo commit/spec hashes, and commit prompts/artifacts to the active project memory repo as run evidence.


### Prompt Assembler (`internal/prompt`)
Builds the final context sent to the model. Layers assembled in order:

```
1. projects/global/rules.md           — always injected, never trimmed
2. projects/global/user.md            — always injected, never trimmed
3. projects/<active>/rules.md         — never trimmed, skipped when active is global
4. resolved agent persona.md          — active project overrides global per file
5. resolved agent rules.md            — active project overrides global per file
6. projects/global/facts.md           — always injected (keep lean by design)
7. resolved agent notes.md            — active project overrides global per file
8. retrieved episodes                 — active-project top-K by blended score, trimmed oldest-first
9. conversation turns                 — current session history
```

Responsibilities:
- **Total memory cap:** sum of layers 6–8 must not exceed `memory_token_budget` (default 6144). Episodes are trimmed oldest-first to fit. Layers 1–5 are never trimmed — keep them small by convention.
- **Conversation reserve:** always guarantee `conversation_reserve` tokens (default 8192) for live turns. If memory + conversation would exceed ctx_size, reduce episode count further.
- Apply Qwen3 prompt template formatting
- Hot-reload rule and persona files on change via fsnotify
- Expose layer debug output to UI logs page (shows token count per layer)

### Memory Store (`internal/memory`)
Mediates all reads and writes to git-backed project memory repos.

**Read path:**
1. Load static layers from the global project repo and active project repo: global rules/user/facts, resolved agent persona/rules/notes, and optional active project rules
2. Retrieve episodes: recency (last N) + semantic (ANN on `index/_episodes/vectors.bin`) → merge → deduplicate → re-rank by blended score
3. Return ordered list of chunks to Prompt Assembler

**Write path:**
1. Post-session: summarize via Qwen → write `episodes/<n>/<timestamp>.md` in the active project memory repo
2. Embed new chunks → update `index/_episodes/{vectors.bin, manifest.json}` in that repo
3. Commit via Git Backend with structured message in that repo

**Promotion API:**
- `PromoteToGlobalFact(text string)` → append to `projects/global/facts.md` + commit in the global project repo
- `AppendAgentNote(agent, text string)` → append to resolved `agents/<n>/notes.md` + commit in the owning project repo
- Both exposed in the UI memory page

**Cross-agent reads:** explicit only. An agent may request episodes from another agent's directory. Not automatic.

### Git Backend (`internal/git`)
Thin wrapper around `go-git` (pure Go — no git binary dependency).

Operations:
- `Init(path string)` — init or open one memory repo or attached code repo
- `Commit(msg string, files []string)` — stage specific files + commit in the selected repo
- `QueryLog(tags map[string]string) []CommitMeta` — filter by structured tags in commit message
- `BlobByRef(sha string) ([]byte, error)` — fetch chunk content by SHA

Commit message format (machine-parseable):
```
[agent:coder] [type:episode] brief human-readable summary
```

Index rebuild: walk all commits, re-embed any SHA missing from `index/manifest.json`. Idempotent, safe to run on a fresh clone.

### Embedder (`internal/embedder`)
Runs nomic-embed-text as a sidecar process. Same spawn/monitor pattern as llama-server — consistent failure handling, restart on crash.

Interface:
```go
type Embedder interface {
    Embed(ctx context.Context, chunks []string) ([][]float32, error)
    Health(ctx context.Context) error
}
```

Ships as a self-contained binary (no Python dependency).

ANN index: flat scan for small corpora (<10k chunks). Upgrade to usearch or hnswlib if retrieval latency becomes a problem.

### Queue (`internal/queue`)
Single-model request queue. Simple bounded channel in Go.

- **Backpressure:** reject with clear error when full, UI shows live queue depth
- **WAL:** append-only file (`queue.wal` in the active project memory repo, defaulting to `~/.harness/projects/global/queue.wal` after layout-v2) for crash recovery — cleared on clean shutdown
- **Cancellation:** supports context cancellation per request (client disconnect cancels generation)

### Inference Client (`internal/inference`)
OpenAI-compatible HTTP client pointed at llama-server.

- Handles streaming responses (SSE), proxies tokens to the caller
- Per-request sampling param overrides (temp, top-p, repeat penalty)
- Timeout + cancellation via context
- Abstraction boundary: swapping llama-server for vllm or another backend touches only this package

### Process Manager (`internal/proc`)
Spawns and monitors llama-server and the Embedder sidecar as child processes.

- Health check loop: HTTP ping on configurable interval
- Restart with exponential backoff on crash or failed health check
- Reads from config: binary path, model path, ctx size, GPU layers, n_parallel
- Emits structured events to UI log stream via SSE

### Metrics Store (`internal/metrics`)
Collects and persists time-series metrics to the shared `harness.db` SQLite file. Layout-v2 places it under `~/.harness/`. Uses the same `*sql.DB` handle as the config store — opened once in `main`, shared by all subsystems. No external dependencies.

Metrics collected grow with each milestone:
- **M1:** llama-server health, uptime, queue depth, restart count
- **M2:** requests per agent, token counts per prompt layer
- **M3:** episodes written per agent, memory repo size, commit count
- **M4:** session duration, tokens in/out per session, loop turn count, read-only tool call count
- **M5:** retrieval latency, embedding latency, index size
- **M7:** tool call count per type, tool error rate, approval decisions, shell execution outcomes
- **M8:** TTFT, token throughput, VRAM usage
- **M10:** pipeline run status, step attempts, reject counts, surface counts, verify/gate duration, artifact bytes

Interface:
```go
type Metrics interface {
    Record(name string, value float64, tags map[string]string)
    Query(name string, from, to time.Time) ([]DataPoint, error)
}
```

The UI reads directly from SQLite to render history charts. An optional Prometheus endpoint (M8) exposes the same data for external scraping.

### API Server (`internal/api`) — optional
Thin OpenAI-compatible HTTP server. Enables external clients to send chat completions through the same prompt, queue, inference, and session-recording path.

- Exposes `/v1/chat/completions` (streaming)
- Each request goes through Session Manager → Prompt Assembler → Queue → Inference Client
- Memory and persona injection is transparent to the caller
- Separate port from UI server, disabled by default, enabled via config

### Metrics Store (`internal/metrics`)
Time-series tables inside the shared `harness.db` under the harness home after layout-v2. Written to by all components, read by the UI server for the status and logs pages.

Schema grows per milestone:
- **M1:** `uptime`, `process_health` (llama-server, embedder), `queue_depth`, `restart_count`
- **M2:** `prompt_layer_tokens` (per layer, per request), `hot_reload_events`
- **M3:** `session_count`, `episode_count`, `git_commit_latency_ms`
- **M4:** `active_sessions`, `agent_loop_turns`, `tool_calls_by_type`, `tool_failures`
- **M5:** `embedding_latency_ms`, `index_size_chunks`, `retrieval_scores`, `ann_search_latency_ms`
- **M6:** `promotions_count`, `dedup_blocks`, `cross_agent_reads`
- **M7:** `approval_decisions`, `shell_exec_duration_ms`, `tool_output_truncations`
- **M8:** `ttft_ms`, `token_throughput`, `vram_usage_mb`
- **M10:** `pipeline_runs`, `pipeline_step_attempts`, `pipeline_rejects`, `pipeline_surfaces`, `pipeline_verify_duration_ms`, `pipeline_gate_duration_ms`, `pipeline_artifact_bytes`

Retention: raw rows kept for 30 days, downsampled hourly aggregates kept indefinitely. UI shows both live values (SSE) and historical charts (htmx polling).

---

## Harness Home And Memory Repo Layout

```
~/.harness/
  harness.db                   ← config, metrics, and runtime control state
  projects/
    global/                    ← git repo: global project and fallback agent library
      rules.md                 ← always-on base prompt
      user.md                  ← stable facts about the user, hand-authored
      facts.md                 ← promoted cross-agent facts, kept lean
      agents/<n>/{persona.md, rules.md, notes.md}
      sessions.jsonl
      queue.wal
      episodes/<n>/<timestamp>.md
      index/_episodes/{vectors.bin, manifest.json}
      artifacts/<run>/...
    <id>/                      ← git repo: user project memory
      rules.md                 ← project-specific rules
      agents/<n>/{persona.md, rules.md, notes.md}   ← optional project agent overrides/additions
      sessions.jsonl
      queue.wal
      episodes/<n>/<timestamp>.md
      index/_episodes/{vectors.bin, manifest.json}
      index/<dir-slug>/{vectors.bin, manifest.json}
      artifacts/<run>/...
  logs/
  cache/
```

Each directory under `~/.harness/projects/` is its own git repo. `harness.db`, logs, and cache files are machine-local and are never committed.

M3 stages the single-repo `projects/global/` paths immediately; M3b introduces the `projects` table, the `active_project_slug` config, and user-created project rows on top of that layout. M9 layout-v2 splits those project subdirectories into separate project memory repos under `~/.harness/projects/`, with `global` as a first-class project repo. M10 adds `artifacts/` as project-owned run evidence so prompts and outputs travel with the active project memory repo while operational run state remains in SQLite. Pipeline source specs do not live in memory repos by default; they live in the attached project git repos they operate on, and runs record the source repo commit plus spec hash.

The shared `harness.db` SQLite file (config + metrics + runtime control state) lives in `~/.harness/`, not in any memory repo — it is machine-local operational data, not user data.

---

## Config

Configuration lives in a single-row typed `config` table inside `harness.db`. There is no on-disk config file — the user edits settings through the `/config` page in the management UI, which writes back to the database.

The schema mirrors the Go `config.Config` struct: one column per field, snake-cased with a section prefix (`model_binary`, `embedder_port`, `prompt_memory_token_budget`, etc.). Column defaults in the DDL mirror `config.Defaults()` in Go so the two stay honest.

Sections and fields:
- **model:** `binary`, `model_path`, `ctx_size`, `gpu_layers`, `n_parallel`, `port`
- **embedder:** `binary`, `model_path`, `port`
- **memory:** current single-repo path before M9; after layout-v2, harness home and per-project memory repo paths
- **ui:** `port`, `open_on_start`
- **api:** `enabled`, `port`
- **project:** `active_project_slug`, `llama_on_switch`
- **prompt:** `ctx_size`, `memory_token_budget`, `conversation_reserve`
- **queue:** `max_depth`, `wal_path`
- **metrics:** `retention_days`

First run: the row is seeded with defaults and `saved_at` is NULL. The status page shows a "Set up your harness" CTA until the user saves at least once. Changes to `ui.port`, model/embedder binaries, and ports take effect on the next harness restart; everything else is reloaded when the retry callback fires.

---

## Key Design Decisions

**First-party chat and tool execution.** The harness owns the browser chat/task surface, native agent loop, and local tool execution. External coding agents may inspire design choices, but they are not runtime dependencies.

**Desktop app behavior, no terminal required.** Double-click to start, system tray to quit. Single-instance enforced via lock file. Browser opens automatically on first launch. All errors surface in the browser UI.

**htmx over JavaScript.** The management UI is read-mostly with a handful of simple actions. htmx + SSE + `html/template` covers everything without a build step, bundler, or node_modules. Ships as a single binary via `embed.FS`.

**go-git over git binary.** Removes the git-on-PATH requirement. Pure Go, no subprocess.

**Embedder as sidecar.** Keeps Core free of Python/C dependencies. Uses the same process management pattern as llama-server — uniform failure handling, restart logic, and health checking.

**Single SQLite file for operational state.** Config (single-row typed table), metrics (time-series tables), project identity, and runtime control state share `harness.db` under the harness home after layout-v2. One `*sql.DB` handle is opened in `main` and passed to subsystems — no per-package database connection, no lock contention. The UI reads metrics directly — no separate metrics server. Each milestone adds its own table(s). On restart, history is preserved. Prometheus export (M8) reads from the same database.

**Project memory repos are explicit git repos.** Before M9, a missing `memory.repo_path` is a setup error. Layout-v2 replaces that with a harness home and explicit project creation flow: provided git directories are used as-is, provided non-git directories are initialized with `go-git`, and omitted directories create `~/.harness/projects/<id>/` with `go-git`. No cwd inference and no terminal-only setup path.

**Append-only sessions.jsonl.** Never mutate, only append. Trivial crash recovery, full audit log.

**Commit message tags.** Structured `[key:value]` tags in commit messages enable log filtering without a separate metadata store. The git log *is* the index for episode discovery.

**Two HTTP servers, one binary.** UI server (port 3000) and API server (port 8080) are separate. UI is always on. API is opt-in for external OpenAI-compatible clients and is never required for the first-party browser workflow.

**Native agent layer staged after Projects.** M4 introduces `internal/agentloop` and `internal/tools` as planned first-party components. `internal/agent` remains the registry/persona package. The MVP starts read-only and project-scoped; destructive tools and approvals are deferred to M7.

**Pipeline DSL staged after layout-v2 and tool permissions.** `.hp` pipelines depend on project memory repos, attached source repos, the native agent loop, and the hardened tool/approval layer because model steps write declared outputs through harness tools and verify/gate commands run as trusted local processes. The DSL is deliberately not part of M4: interactive agent-loop execution comes first, safe write/shell permissions come second, storage layout stabilization comes third, declarative multi-step automation comes after those foundations.
