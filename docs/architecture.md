# Architecture

## Overview

A local AI inference harness with git-backed project memory, layered prompt assembly, a browser-based management UI, a first-party native agent loop, an approval-gated local tool layer, and a planned declarative pipeline runner. The harness owns chat, tool-call orchestration, tool execution, and planned `.hp` pipeline execution locally; external coding agents are references for design patterns, not runtime dependencies.

The harness runs as a native desktop binary (Windows and Linux). Windows and Linux are equal first-class targets; platform-specific behavior must be designed, tested, and documented for both rather than treating either OS as best-effort. It starts silently, opens the management UI in the default browser if not already open, and lives in the system tray until explicitly quit. The browser UI is the only user-facing surface — all errors (unconfigured on first run, missing model, llama-server failures, missing memory repo) are surfaced there, not in a terminal.

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
│  │ (planned)    │  │ + Approvals    │   └──────┬─────┘  │
│  └──────┬───────┘  └───────▲────────┘          │        │
│         ▼                  │                   ▼        │
│  ┌──────────────┐          │            ┌─────────────┐ │
│  │ Agent Loop   │──────────┘            │  Embedder   │ │
│  │              │                       │ (sidecar)  │ │
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

The browser UI is the primary chat/task surface and, once M11 lands, the primary pipeline execution surface. The optional OpenAI-compatible API server remains available for external clients, but first-party agent-loop and pipeline execution stay inside the harness.

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
- **Projects** — create, edit, hide, and switch active project
- **Agents** — switch active agent, edit persona and notes inline, trigger hot-reload
- **Memory** — browse episodes by agent/date, view retrieval scores, promote facts
- **Logs** — live process manager events, prompt assembly debug, loop turns, and tool-call traces

### System Tray (`internal/tray`)
Manages the binary's desktop presence. Uses `fyne-io/systray` (native on Windows and Linux, no CGO required for Windows; GTK-based on Linux).

Behavior:
- **On start:** check if another instance is already running (via Windows mutex on Windows, file lock on Linux); if so, do nothing and exit
- **If first instance:** start all services, open browser to UI if not already open, show tray icon
- **Tray icon menu:** Open UI, Quit
- **On Quit:** graceful shutdown — drain queue, terminate child processes, release lock

### Session Manager (`internal/session`)
Owns conversation lifecycle for the browser chat/task surface, the optional OpenAI-compatible API server, and the native agent loop.

- **On start:** resolve active agent → trigger memory read → assemble initial context
- **Per turn:** append to conversation history → call Prompt Assembler → send to Queue
- **On end:** call summarizer (Qwen) → write episode file → trigger git commit
- **Persistence:** append-only `sessions.jsonl` in the active project memory repo (default `~/.harness/projects/global/` for the global project)

### Runtime (`internal/runtime`)
Owns the mutable service graph behind the harness. `cmd/harness/main.go` creates the UI first, then asks `internal/runtime` to wire and retry the rest of the subsystems after the browser surface is already available.

Responsibilities:
- Hold the active config, config store, project store, process managers, queue, inference client, memory reader, prompt assembler, session manager, API server, tool registry, and log rings.
- Implement the retry/config-save path that revalidates config, memory repo, projects, llama-server, embedder, API server, and session services without requiring a binary restart where possible.
- Adapt package boundaries for the UI: chat/task runners, memory APIs, project health checks, approval routing, and session persistence.
- Keep runtime state behind locks because UI handlers, process events, metrics, and retry callbacks run concurrently.

### Agent Loop (`internal/agentloop`)
Owns the first-party agentic turn loop. This package is separate from `internal/agent`, which remains the agent/persona registry.

Responsibilities:
- Maintain part-based messages (`text`, `tool_call`, `tool_result`) for UI display and session replay.
- Call the Prompt Assembler, submit requests through Queue/Inference Client, parse tool calls, dispatch tools, inject results, and continue until stop/limit/cancel.
- Enforce max turn count, doom-loop detection, cancellation propagation, and compaction triggers.
- Emit loop events to the UI task stream and logs.
- Apply the optional approval evaluator before destructive tool dispatch. Ask decisions pause the loop until the UI applies an allow/reject/always decision.

### Tool Registry + Sandbox (`internal/tools`)
Defines the local tool surface available to the agent loop.

Responsibilities:
- Register tools by id, JSON Schema parameters, description, and execute function.
- Pass a typed context to each tool: active project slug, sandbox roots, session id, caller identity, and cancellation context.
- `file_read` and `file_list` are read-only tools and reject paths outside active project directories.
- `file_write` and `shell_exec` are registered for M7, but disabled by default in config and approval-gated before execution.
- Web search remains M7 scope as an opt-in tool with explicit network-use disclosure; file edit/patch, steering queues, extension hooks, sub-agents, and tool-history retry UI are deferred beyond M7.

### Approvals (`internal/approvals`)
Owns the M7 permission evaluator used by the agent loop.

Responsibilities:
- Evaluate layered rules in order: builtin defaults, user config, then session approvals.
- Support allow, reject, and always-allow session decisions.
- Classify destructive shell commands so broad rules cannot silently allow them; exact session approvals are required to bypass Ask for a destructive command.
- Return audit-friendly decision sources for UI/session history.

### Pipeline Runner (`internal/pipeline`) — planned M11
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
1. rules.md in the active project repo       — never trimmed
2. user.md in the active project repo        — never trimmed
3. resolved agent persona.md                 — active project overrides global library
4. resolved agent rules.md                   — active project overrides global library
5. facts.md in the active project repo       — never trimmed (keep lean by design)
6. agent notes.md in the active project repo — never falls back to global library
7. retrieved episodes                        — active-project top-K by blended score, trimmed oldest-first
8. conversation turns                        — current session history
```

> **Project memory repos:** prompt assembly receives two physical repo readers. The active
> project repo provides prompt memory, notes, and episodes; the global project
> repo is only the fallback library for agent definition files (`persona.md` and
> `rules.md`). The global project is otherwise just the default active project.

Responsibilities:
- **Total memory cap:** sum of layers 5–7 must not exceed `memory_token_budget` (default 6144). Episodes are trimmed oldest-first to fit. Layers 1–4 are never trimmed — keep them small by convention.
- **Conversation reserve:** always guarantee `conversation_reserve` tokens (default 8192) for live turns. If memory + conversation would exceed ctx_size, reduce episode count further.
- Return OpenAI-style chat messages; llama-server applies the model-specific chat template
- Hot-reload rule and persona files by re-reading prompt inputs on each request
- Expose layer debug output to UI logs page (shows token count per layer)

### Memory Store (`internal/memory`)
Mediates all reads and writes to git-backed project memory repos.

**Read path:**
1. Load static layers from the active project repo: rules, user, facts, project-local notes, and resolved agent persona/rules (falling back to the global agent library only for persona/rules)
2. Retrieve episodes: recency (last N) + semantic (ANN on `index/_episodes/vectors.bin`) → merge → deduplicate → re-rank by blended score
3. Return ordered list of chunks to Prompt Assembler

**Write path:**
1. Post-session: summarize via Qwen → write `episodes/<n>/<timestamp>.md` in the active project memory repo
2. Embed new chunks → update `index/_episodes/{vectors.bin, manifest.json}` in that repo
3. Commit via Git Backend with structured message in that repo

**Promotion API:**
- `PromoteFact(text string)` → append to `facts.md` in the active project repo + commit
- `AppendAgentNote(agent, text string)` → append to `agents/<n>/notes.md` in the active repo + commit
- Both exposed in the UI memory page

**Cross-agent reads:** explicit only. An agent may request episodes from another agent's directory. Not automatic.

### Project Store (`internal/project`)
Defines project identity and validation rules. SQL persistence lives in `internal/db`, while this package owns typed project values, slugs, directory metadata, effective model overrides, and lifecycle status such as hidden or system projects.

### Git Backend (`internal/git`)
Thin wrapper around `go-git` (pure Go — no git binary dependency).

Operations:
- `Init(path string)` — init or open one memory repo or attached code repo
- `Commit(msg string, files []string)` — stage specific files + commit in the selected repo

Commit message format (machine-parseable):
```
[agent:coder] [type:episode] brief human-readable summary
```

Index rebuild: walk episode files in the project memory repo and re-embed any SHA missing from `index/manifest.json`. Idempotent, safe to run on a fresh clone.

### Vector Index (`internal/index`)
Manages flat vector indices stored as `vectors.bin` plus `manifest.json` pairs under a project's `index/` tree.

Responsibilities:
- Create and open index directories.
- Append vectors idempotently by content SHA.
- Perform cosine-similarity flat scans for top-K search.
- Keep the on-disk format isolated from prompt and memory logic.

Attached code repos are indexed by git state: each attached directory gets its own slot under the project memory repo at `index/<dir-slug>/`, and a new HEAD in one attached repo invalidates only that repo's slot. This keeps multi-repo projects unified without writing runtime memory into any source repo. (Directory-level indexing itself is deferred until directory semantic search becomes a user-facing feature — see the M5 note in the roadmap.)

### Embedder (`internal/embedder`)
Runs llama-server as a sidecar process in --embedding mode using the configured embedding model. Same spawn/monitor pattern as the chat llama-server process — consistent failure handling, restart on crash.

Interface:
```go
type Embedder interface {
    Embed(ctx context.Context, chunks []string) ([][]float32, error)
}
```

Uses the configured llama-server-compatible binary and GGUF embedding model; no Python runtime is required.

ANN index: flat scan for small corpora (<10k chunks). Upgrade to usearch or hnswlib if retrieval latency becomes a problem.

### Queue (`internal/queue`)
Single-model request queue. Simple bounded channel in Go.

- **Backpressure:** reject with clear error when full, UI shows live queue depth
- **Crash behavior:** queued interactive requests are in-process only and are not replayed after a crash; durable session and memory recording is owned by `internal/session` and git commits
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

### Database (`internal/db`)
Owns all SQLite persistence for operational state. Domain packages define typed interfaces (`config.Store`, `metrics.Store`, project store surfaces); `internal/db` is the concrete SQL implementation and migration owner.

Responsibilities:
- Open and migrate `harness.db`.
- Seed and update the single-row config table.
- Store projects and attached project directories.
- Store metrics samples and expose query APIs for UI/status pages.

### Metrics Store (`internal/metrics`)
Defines the typed metrics API and recorder helpers. Persistence lives in `internal/db` and uses the shared `~/.harness/harness.db` SQLite file.

Currently recorded metric names:
- `uptime_seconds`
- `queue_depth`
- `process_health`
- `restart_count`
- `session_count`
- `episode_count`
- `git_commit_latency_ms`
- `ttft_ms`
- `token_throughput_tokens_per_sec`
- `vram_used_mb`
- `loop_turn_count`
- `tool_call_count`
- `tool_call_error_count`
- `tool_call_error_rate`

Pipeline run metrics remain milestone-scoped in the roadmap until the M11 runner lands.

Interface:
```go
type Store interface {
    Record(name string, value float64, tags map[string]string) error
    Latest() ([]DataPoint, error)
    ApplyRetention(retentionDays int) error
}
```

The UI reads latest retained samples directly from SQLite. Retention compacts older raw samples into aggregates so `harness.db` does not grow forever. An optional Prometheus endpoint (M8) exposes the latest samples for external scraping.

### API Server (`internal/api`) — optional
Thin OpenAI-compatible HTTP server. Enables external clients to send chat completions through the same prompt, queue, inference, and session-recording path.

- Exposes `/v1/chat/completions` (streaming)
- Each request goes through Session Manager → Prompt Assembler → Queue → Inference Client
- Memory and persona injection is transparent to the caller
- Separate port from UI server, disabled by default, enabled via config

### Log Buffer (`internal/logbuf`)
Provides in-memory ring buffers for harness logs and child process output. The UI status/log surfaces read recent entries and subscribe to live batches over SSE. `~/.harness/logs/` is reserved for durable logs, but current log buffers are memory-only.

### Request IDs (`internal/reqid`)
Threads a per-request identifier through `context.Context` so API handlers, queue dispatch, prompt assembly, and logs can correlate one request without adding request-id fields to every package API.

---

## Harness Home And Memory Repo Layout

> **Project memory repo layout.** The tree below is the runtime storage layout.

```
~/.harness/                    ← harness home
  harness.db                   ← config, metrics, and runtime control state
  projects/
    global/                    ← git repo: default project and fallback agent-definition library
      rules.md                 ← default project rules
      user.md                  ← default project user facts
      facts.md                 ← default project promoted facts
      agents/<n>/{persona.md, rules.md, notes.md}   ← persona/rules library; notes only when global is active
      sessions.jsonl
      episodes/<n>/<timestamp>.md
      index/_episodes/{vectors.bin, manifest.json}
      artifacts/<run>/...
    <id>/                      ← git repo: user project memory
      rules.md                 ← project rules
      user.md                  ← project user facts
      facts.md                 ← project promoted facts
      agents/<n>/{persona.md, rules.md, notes.md}   ← optional project agent definitions and notes
      sessions.jsonl
      episodes/<n>/<timestamp>.md
      index/_episodes/{vectors.bin, manifest.json}
      index/<dir-slug>/{vectors.bin, manifest.json}
      artifacts/<run>/...
  logs/
  cache/
```

Each directory under `~/.harness/projects/` is its own git repo. `harness.db`, logs, and cache files are machine-local and are never committed.

M9 project memory repos store each project as a separate memory repo under `~/.harness/projects/` by default, with `global` as a first-class project repo. There were no pre-M9 installs to migrate, so legacy single-repo migration code has been removed. `artifacts/` is project-owned run evidence so prompts and outputs travel with the active project memory repo while operational run state remains in SQLite. Pipeline source specs do not live in memory repos by default; they live in the attached project git repos they operate on, and runs record the source repo commit plus spec hash.

The shared `harness.db` SQLite file (config + metrics + runtime control state) lives under `~/.harness/` and is machine-local operational data, not user data.

---

## Config

Configuration lives in a single-row typed `config` table inside `harness.db`. There is no on-disk config file — the user edits settings through the `/config` page in the management UI, which writes back to the database.

The schema mirrors the persisted fields of the Go `config.Config` struct, snake-cased with a section prefix (`model_binary`, `embedder_port`, `prompt_memory_token_budget`, etc.). Runtime-derived fields such as the effective prompt context size are not stored. The database seed path writes every initial value from `config.Defaults()`; migrations deliberately define columns without parallel SQL defaults so Go remains the single source of truth.

Sections and fields:
- **model:** `binary`, `model_path`, `ctx_size`, `gpu_layers`, `n_parallel`, `port`, `verbose`, `cache_type_k`, `cache_type_v`
- **embedder:** `binary`, `model_path`, `port`, `verbose`
- **agent:** `active`
- **ui:** `port`, `open_on_start`
- **api:** `enabled`, `port`
- **project:** `active_project_slug`, `llama_on_switch`
- **prompt:** `memory_token_budget`, `conversation_reserve`, `recency_n`, `summarizer_prompt`, `semantic_weight`, `recency_weight`, `promotion_dedup_threshold`
- **queue:** `max_depth`
- **metrics:** `retention_days`
- **log:** `ring_max_entries`, `proc_max_lines`
- **loop:** `max_turns`, `doom_threshold`, `file_read_enabled`, `file_list_enabled`, `file_write_enabled`, `shell_exec_enabled`, `web_search_enabled`

First run: the row is seeded with defaults and `saved_at` is NULL. The status page shows a "Set up your harness" CTA until the user saves at least once. Changes to `ui.port`, model/embedder binaries, and ports take effect on the next harness restart; everything else is reloaded when the retry callback fires.

---

## Key Design Decisions

**First-party chat and tool execution.** The harness owns the browser chat/task surface, native agent loop, and local tool execution. External coding agents may inspire design choices, but they are not runtime dependencies.

**Desktop app behavior, no terminal required.** Double-click to start, system tray to quit. Single-instance enforced via lock file. Browser opens automatically on first launch. All errors surface in the browser UI.

**htmx over JavaScript.** The management UI is read-mostly with a handful of simple actions. htmx + SSE + `html/template` covers everything without a build step, bundler, or node_modules. Ships as a single binary via `embed.FS`.

**go-git over git binary.** Removes the git-on-PATH requirement. Pure Go, no subprocess.

**Embedder as sidecar.** Runs a separate llama-server process in embedding mode, keeping Core free of Python dependencies while reusing the same process management pattern as the chat model — uniform failure handling, restart logic, and health checking.

**Single SQLite file for operational state.** Config (single-row typed table), metrics (time-series tables), project identity, and runtime control state share `harness.db` under the harness home with project memory repos. One `*sql.DB` handle is opened in `main` and passed to subsystems — no per-package database connection, no lock contention. The UI reads metrics directly — no separate metrics server. Each milestone adds its own table(s). On restart, history is preserved. Prometheus export (M8) reads from the same database.

**Project memory repos are explicit git repos.** The harness uses a harness home and explicit project creation flow: provided git directories are used as-is, provided non-git directories are initialized with `go-git`, and omitted directories create `~/.harness/projects/<id>/` with `go-git`. No cwd inference and no terminal-only setup path.

**Append-only sessions.jsonl.** Never mutate, only append. Trivial crash recovery, full audit log.

**Commit message tags.** Structured `[key:value]` tags in commit messages keep memory-repo history readable and auditable without requiring a separate metadata store for commit intent. Episode discovery uses the project memory tree and vector manifests, not a bespoke git-log query API.

**Two HTTP servers, one binary.** UI server (port 3000) and API server (port 8080) are separate. UI is always on. API is opt-in for external OpenAI-compatible clients and is never required for the first-party browser workflow.

**Native agent layer staged after Projects.** M4 introduced `internal/agentloop` and `internal/tools` as first-party components. `internal/agent` remains the registry/persona package. The MVP started read-only and project-scoped; M7 adds approval-gated destructive tools.

**Approval-gated tools.** M7 registers `file_write` and `shell_exec` but leaves both disabled by default. Enabling them still routes calls through the approvals evaluator. Every `shell_exec` command is always gated on an explicit per-call approval: a matching deny rule still denies, but there is no remembered `allow` that lets a shell command skip the prompt, so the task UI does not offer "always allow" for shell. Web search also remains M7, but must be opt-in and clearly disclosed because it uses the network. File edit/patch, steering/follow-ups, extension hooks, sub-agents, and tool-history retry UI are deferred beyond M7.

**Pipeline DSL staged with project memory repos and tool permissions.** `.hp` pipelines depend on project memory repos, attached source repos, the native agent loop, and the hardened tool/approval layer because model steps write declared outputs through harness tools and verify/gate commands run as trusted local processes. The DSL is deliberately not part of M4: interactive agent-loop execution comes first, safe write/shell permissions come second, storage layout stabilization comes third, declarative multi-step automation comes after those foundations.
