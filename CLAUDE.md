# CLAUDE.md

This file is the entry point for Claude Code. Read it fully before touching anything.

---

## Project

A local AI inference harness for Windows native. Double-clickable binary, always-on, browser-based management UI. No cloud, no telemetry, no dependencies at runtime beyond what the harness manages itself.

- **Docs:** `docs/architecture.md`, `docs/roadmap.md`, `docs/agents.md`
- **Language:** Go
- **Target OS:** Windows native (no WSL)

Read the architecture doc before writing any code. It defines component boundaries, package names, and key design decisions that must be respected.

---

## Repo structure

```
cmd/
  harness/          ← main entry point, wires everything together
internal/
  api/              ← optional OpenAI-compatible HTTP server
  embedder/         ← nomic-embed-text sidecar client
  git/              ← go-git wrapper (memory repo)
  inference/        ← OpenAI-compatible HTTP client for llama-server
  memory/           ← memory store: read/write/retrieval
  metrics/          ← SQLite-backed metrics collection
  proc/             ← process manager (llama-server + embedder)
  prompt/           ← layered prompt assembler
  queue/            ← bounded request queue + WAL
  session/          ← session lifecycle
  tray/             ← system tray (fyne-io/systray)
  ui/               ← management web UI (net/http + html/template)
docs/
  architecture.md
  roadmap.md
  agents.md
harness.db          ← SQLite database: config (single-row typed table) + metrics history (created on first run)
```

---

## Rules

### Git
- Always work on a feature branch, never directly on `main`. Branch naming: `feat/<short-description>` or `fix/<short-description>`.
- Commit throughout the work — small, logical commits with clear messages. Do not batch everything into one commit at the end.
- When the task is complete, open a PR against `main`. The PR description must summarize what changed and reference the relevant milestone and acceptance tests.
- Do not merge. Wait for the user to explicitly say so.
- After the user approves and merges: switch to `main`, pull, delete the feature branch locally and remotely, confirm the working tree is clean.

### General
- Read `docs/architecture.md` before starting any task. Do not deviate from component boundaries without explicit discussion.
- Work one milestone at a time. Check `docs/roadmap.md` for the current milestone and its acceptance tests. Do not implement M2 features while working on M1.
- All acceptance tests for the current milestone must pass before considering it done.
- If a design decision is unclear, ask — do not assume and proceed.

### Go
- Idiomatic Go: explicit error handling, no magic, no `panic` in library code.
- Prefer standard library. Add a dependency only when the stdlib genuinely cannot do the job.
- Interfaces where they reduce coupling, not for abstraction's sake.
- Table-driven tests. Every package gets a `_test.go`.
- No `init()` functions.
- Errors wrapped with context: `fmt.Errorf("proc: failed to start llama-server: %w", err)`.

### Architecture
- `systray` must own the main goroutine. Everything else runs in goroutines launched from `cmd/harness/main.go`.
- The UI server and the API server are on separate ports. Never merge them.
- **The UI server starts first, always.** It must be up before anything else is attempted. Config loading, memory repo validation, llama-server startup, embedder startup — all of this happens after the UI is serving. If anything fails, it is displayed in the UI as a setup error. The user should never need a terminal to diagnose or fix a problem.
- There is no CLI. No subcommands. No `init-memory`. The user sets up the memory repo themselves (it is a plain git repo) and points the harness at it via the config editor in the UI.
- All persistent state (config + metrics) lives in `harness.db` (SQLite, alongside the binary). Each milestone adds its own metrics tables — see `docs/roadmap.md` for what each milestone must instrument.
- `harness.db` is not committed to git. It is machine-local.

### UI
- Server-side rendering only: `net/http` + `html/template` + htmx + SSE.
- No JavaScript frameworks. No build step. No `node_modules`.
- Static assets (CSS, htmx) embedded via `embed.FS` and compiled into the binary.
- The status page is the first thing the user sees. If there are startup errors, it displays them as a clear checklist: what is wrong, why, and what the user needs to do to fix it. A "Retry" button re-attempts validation without restarting the binary.
- Error states to handle explicitly in the UI: first run (no config saved yet), `harness.db` cannot be opened, missing or invalid `memory.repo_path`, llama-server binary not found, model file not found, llama-server failed to start, embedder failed to start.

### Process management
- `systray` (fyne-io/systray) for the tray icon. It blocks `main()` — all services start before calling `systray.Run()`.
- Single-instance enforcement via a named Windows mutex (`CreateMutex`).
- On double-click when already running: do nothing, exit the second instance silently.
- On Quit from tray: drain queue, flush WAL, commit any pending session, terminate child processes, exit.

---

## Startup sequence

```
1. Acquire single-instance mutex → if already held, exit silently
2. Start UI server (port 3000) → always succeeds, browser opens if not already open
3. Open harness.db → surface error in UI if the database cannot be opened or migrated
4. Load config row → if the user has never saved, show the first-run CTA and stop here
5. Validate memory repo path → surface error in UI if missing or not a git repo
6. Start llama-server process → surface error in UI if binary missing or startup fails
7. Start embedder sidecar → surface error in UI if binary missing or startup fails
8. Begin health check loops for llama-server and embedder
9. Start API server if enabled in config
10. Hand off to systray.Run()
```

Steps 3–9 can fail independently. The UI reflects the state of each. A "Retry" button re-runs steps 3–9 without restarting the binary.

---

## Config

Configuration is stored as a single-row typed table (`config`) in `harness.db` next to the binary. There is no on-disk config file — the user edits settings through the `/config` page in the management UI.

On first run the row is seeded with defaults (column defaults in the DDL mirror `config.Defaults()`). Until the user saves at least once, `saved_at` is NULL and the status page shows a "Set up your harness" CTA pointing at the config editor.

Required fields (validated on save): model binary, model path, embedder binary, embedder model path, model/embedder/UI ports.

Schema lives in [internal/config/config.go](internal/config/config.go). The Go struct is flat per section (Model, Embedder, Memory, UI, API, Prompt, Queue, Metrics); DDL column names are snake_case with the section as prefix (e.g. `model_ctx_size`, `prompt_memory_token_budget`).

---

## Memory repo

A plain git repo the user creates and manages. The harness reads and writes it via `go-git` — no git binary required.

Expected structure:
```
memory/
  global/
    rules.md
    user.md
    facts.md
  agents/
    coder/
      persona.md
      notes.md
      episodes/
  index/
    vectors.bin
    manifest.json
  runtime/
    sessions.jsonl
    queue.wal
```

See `docs/agents.md` for the content of `rules.md`, `user.md`, and agent personas.

---

## Current milestone

Check `docs/roadmap.md`. Start at M1 and do not advance until all acceptance tests pass.
