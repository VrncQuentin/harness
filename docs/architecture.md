# Architecture

## Overview

A local AI inference harness with a git-backed memory system, layered prompt assembly, and a browser-based management UI. First iteration delegates agentic tool execution (file edits, shell, web search) to opencode. Future iterations replace opencode with a native agent layer.

The harness runs as a double-clickable Windows native binary. It starts silently, opens the management UI in the default browser if not already open, and lives in the system tray until explicitly quit. The browser UI is the only user-facing surface — all errors (missing config, missing model, llama-server failures, missing memory repo) are surfaced there, not in a terminal.

The binary targets llama-server as the inference backend and uses a separate embedding sidecar for semantic memory.

---

## Component Map

```
┌─────────────────────────────────────────────────────────┐
│           Browser (management UI)                        │
│   model status │ agent config │ memory browser │ logs   │
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
│  └──────────────┘  └────────────────┘  └──────┬──────┘  │
│         │                                     │         │
│         ▼                                     ▼         │
│  ┌──────────────┐                    ┌───────────────┐   │
│  │    Queue     │                    │  Git Backend  │   │
│  └──────┬───────┘                    └──────┬────────┘   │
│         │                                   │           │
│  ┌──────▼───────┐                    ┌──────▼────────┐   │
│  │  Inference   │                    │   Embedder    │   │
│  │  Client     │                    │  (nomic)      │   │
│  └──────┬───────┘                    └───────────────┘   │
└─────────┼───────────────────────────────────────────────┘
          │ HTTP (OpenAI-compatible)
┌─────────▼────────────┐
│   llama-server       │  ← harness spawns, monitors, restarts
└──────────────────────┘
```

opencode connects to the harness via the optional OpenAI-compatible API server (separate port from the UI server) and handles all chat, file editing, and tool execution.

---

## Components

### UI Server (`internal/ui`)
Lightweight management interface. Serves pre-rendered HTML fragments, no JavaScript framework, no build step.

Stack:
- `net/http` — request routing
- `html/template` — server-side rendering
- `embed.FS` — static assets (CSS, htmx) compiled into the binary
- htmx — dynamic updates via HTML attributes, no custom JS
- SSE — live log tailing and model health streaming

Pages:
- **Status** — llama-server health, queue depth, VRAM usage, restart controls; errors (missing config, missing model, failed starts) displayed prominently here
- **Agents** — switch active agent, edit persona and notes inline, trigger hot-reload
- **Memory** — browse episodes by agent/date, view retrieval scores, promote facts
- **Logs** — live process manager events, prompt assembly debug (token counts per layer)

No chat UI. opencode owns that surface.

### System Tray (`internal/tray`)
Manages the binary's desktop presence. Uses `fyne-io/systray` (native Windows, no CGO required).

Behavior:
- **On start:** check if another instance is already running (via lock file or named pipe); if so, do nothing and exit
- **If first instance:** start all services, open browser to UI if not already open, show tray icon
- **Tray icon menu:** Open UI, Quit
- **On Quit:** graceful shutdown — drain queue, flush WAL, terminate child processes, release lock

### Session Manager (`internal/session`)
Owns conversation lifecycle. Used by the OpenAI-compatible API server when opencode connects.

- **On start:** resolve active agent → trigger memory read → assemble initial context
- **Per turn:** append to conversation history → call Prompt Assembler → send to Queue
- **On end:** call summarizer (Qwen) → write episode file → trigger git commit
- **Persistence:** append-only `runtime/sessions.jsonl` in the memory repo

### Prompt Assembler (`internal/prompt`)
Builds the final context sent to the model. Layers assembled in order:

```
1. global/rules.md       — always injected, never trimmed
2. global/user.md        — always injected, never trimmed
3. agents/<n>/persona.md — always injected, never trimmed
4. global/facts.md       — always injected (keep lean by design)
5. agents/<n>/notes.md   — always injected (keep lean by design)
6. retrieved episodes    — top-K by blended score, trimmed oldest-first
7. conversation turns    — current session history
```

Responsibilities:
- **Total memory cap:** sum of layers 4–6 must not exceed `memory_token_budget` (default 6144). Episodes are trimmed oldest-first to fit. Layers 1–3 are never trimmed — keep them small by convention.
- **Conversation reserve:** always guarantee `conversation_reserve` tokens (default 8192) for live turns. If memory + conversation would exceed ctx_size, reduce episode count further.
- Apply Qwen3 prompt template formatting
- Hot-reload rule and persona files on change via fsnotify
- Expose layer debug output to UI logs page (shows token count per layer)

### Memory Store (`internal/memory`)
Mediates all reads and writes to the git memory repo.

**Read path:**
1. Load static layers: `global/rules.md`, `global/facts.md`, `agents/<n>/persona.md`, `agents/<n>/notes.md`
2. Retrieve episodes: recency (last N) + semantic (ANN on `index/vectors.bin`) → merge → deduplicate → re-rank by blended score
3. Return ordered list of chunks to Prompt Assembler

**Write path:**
1. Post-session: summarize via Qwen → write `agents/<n>/episodes/<timestamp>.md`
2. Embed new chunks → update `index/vectors.bin` + `index/manifest.json`
3. Commit via Git Backend with structured message

**Promotion API:**
- `PromoteToGlobalFact(text string)` → append to `global/facts.md` + commit
- `AppendAgentNote(agent, text string)` → append to `agents/<n>/notes.md` + commit
- Both exposed in the UI memory page

**Cross-agent reads:** explicit only. An agent may request episodes from another agent's directory. Not automatic.

### Git Backend (`internal/git`)
Thin wrapper around `go-git` (pure Go — no git binary dependency, required for Windows native).

Operations:
- `Init(path string)` — init or open existing repo
- `Commit(msg string, files []string)` — stage specific files + commit
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

Ships as a self-contained binary on Windows native (no Python dependency).

ANN index: flat scan for small corpora (<10k chunks). Upgrade to usearch or hnswlib if retrieval latency becomes a problem.

### Queue (`internal/queue`)
Single-model request queue. Simple bounded channel in Go.

- **Backpressure:** reject with clear error when full, UI shows live queue depth
- **WAL:** append-only file (`runtime/queue.wal`) for crash recovery — cleared on clean shutdown
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
- Windows native: uses `os/exec` with Windows paths
- Emits structured events to UI log stream via SSE

### Metrics Store (`internal/metrics`)
Collects and persists time-series metrics to a SQLite file alongside the binary. No external dependencies.

Metrics collected grow with each milestone:
- **M1:** llama-server health, uptime, queue depth, restart count
- **M2:** requests per agent, token counts per prompt layer
- **M3:** episodes written per agent, memory repo size, commit count
- **M4:** session duration, tokens in/out per session, tool call count
- **M5:** retrieval latency, embedding latency, index size
- **M7:** tool call count per type, tool error rate, agentic loop turn count
- **M8:** TTFT, token throughput, VRAM usage

Interface:
```go
type Metrics interface {
    Record(name string, value float64, tags map[string]string)
    Query(name string, from, to time.Time) ([]DataPoint, error)
}
```

The UI reads directly from SQLite to render history charts. An optional Prometheus endpoint (M8) exposes the same data for external scraping.

### API Server (`internal/api`) — optional
Thin OpenAI-compatible HTTP server. Enables opencode and other external tools to connect.

- Exposes `/v1/chat/completions` (streaming)
- Each request goes through Session Manager → Prompt Assembler → Queue → Inference Client
- Memory and persona injection is transparent to the caller
- Separate port from UI server, disabled by default, enabled via config

### Metrics Store (`internal/metrics`)
SQLite database (`metrics.db`) alongside the binary. Written to by all components, read by the UI server for the status and logs pages.

Schema grows per milestone:
- **M1:** `uptime`, `process_health` (llama-server, embedder), `queue_depth`, `restart_count`
- **M2:** `prompt_layer_tokens` (per layer, per request), `hot_reload_events`
- **M3:** `session_count`, `episode_count`, `git_commit_latency_ms`
- **M4:** `active_sessions`, `requests_proxied`, `api_connections`
- **M5:** `embedding_latency_ms`, `index_size_chunks`, `retrieval_scores`, `ann_search_latency_ms`
- **M6:** `promotions_count`, `dedup_blocks`, `cross_agent_reads`
- **M7:** `tool_calls_by_type`, `tool_failures`, `agentic_loop_turns`
- **M8:** `ttft_ms`, `token_throughput`, `vram_usage_mb`

Retention: raw rows kept for 30 days, downsampled hourly aggregates kept indefinitely. UI shows both live values (SSE) and historical charts (htmx polling).

---

## Memory Repo Layout

```
memory/
  global/
    rules.md              ← always-on base prompt (agents.md equivalent)
    user.md               ← stable facts about the user, hand-authored
    facts.md              ← promoted cross-agent facts, kept lean
  agents/
    <n>/
      persona.md          ← agent-specific role and behavior rules
      notes.md            ← persistent facts for this agent
      episodes/
        2026-04-20T14:32.md   ← one file per session summary
  index/
    vectors.bin           ← ANN index (committed, portable)
    manifest.json         ← maps chunk SHA → file path + line range
  runtime/
    sessions.jsonl        ← append-only session log
    queue.wal             ← crash recovery WAL, cleared on clean shutdown
```

Everything in the repo is committed. The repo travels with the user — portable across machines and mediums.

The metrics SQLite file (`metrics.db`) lives alongside the harness binary, not in the memory repo — it is machine-local operational data, not user data.

---

## Config

Single TOML file, hot-reloaded where safe:

```toml
[model]
binary = "/usr/local/bin/llama-server"   # or Windows path
model_path = "/models/qwen3-coder-30b-a3b.Q4_K_M.gguf"
ctx_size = 32768
gpu_layers = 35
n_parallel = 1

[embedder]
binary = "/usr/local/bin/nomic-embed-server"
model_path = "/models/nomic-embed-text-v1.5.gguf"

[memory]
repo_path = "~/memory"
retrieval_top_k = 5
recency_weight = 0.4
semantic_weight = 0.6
episode_summary_max_tokens = 512

[queue]
max_depth = 8
wal_path = "~/memory/runtime/queue.wal"

[prompt]
ctx_size = 32768
memory_token_budget = 6144    # max tokens for layers 3–5 combined
conversation_reserve = 8192   # guaranteed headroom for live turns

[metrics]
db_path = "metrics.db"      # relative to binary, or absolute path
retention_days = 30

[ui]
port = 3000
open_on_start = true

[api]
enabled = false
port = 8080
```

---

## Key Design Decisions

**No chat UI in the harness.** opencode owns the chat and tool execution surface. The harness UI is purely for management: model health, agent config, memory browsing.

**Desktop app behavior, no terminal required.** Double-click to start, system tray to quit. Single-instance enforced via lock file. Browser opens automatically on first launch. All errors surface in the browser UI.

**htmx over JavaScript.** The management UI is read-mostly with a handful of simple actions. htmx + SSE + `html/template` covers everything without a build step, bundler, or node_modules. Ships as a single binary via `embed.FS`.

**go-git over git binary.** Removes the git-on-PATH requirement. Pure Go, no subprocess.

**Embedder as sidecar.** Keeps Core free of Python/C dependencies. Uses the same process management pattern as llama-server — uniform failure handling, restart logic, and health checking.

**Metrics store is a SQLite file alongside the binary.** All metrics are written to `metrics.db` in the same directory as the binary. The UI reads from it directly — no separate metrics server. Each milestone adds its own table(s). On restart, history is preserved. Prometheus export (M8) reads from the same database.

**Memory repo is never auto-created.** If `memory.repo_path` is not set or the path does not exist, the harness refuses to start and prompts the user to either provide an existing repo path or run `init-memory <path>` explicitly. No silent creation.

**Append-only sessions.jsonl.** Never mutate, only append. Trivial crash recovery, full audit log.

**Commit message tags.** Structured `[key:value]` tags in commit messages enable log filtering without a separate metadata store. The git log *is* the index for episode discovery.

**Two HTTP servers, one binary.** UI server (port 3000) and API server (port 8080) are separate. UI is always on. API is opt-in — enables opencode integration without exposing it by default.

**opencode as first-iteration agent layer.** Tool execution (file edits, shell, web search) is delegated to opencode in M1–M4. opencode connects to the API server and gets memory + persona injected transparently. Replacing opencode with a native agent layer is isolated to a new `internal/agent` package — nothing else changes.
