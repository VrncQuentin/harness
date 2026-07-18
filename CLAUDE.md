# CLAUDE.md

This file is the entry point for Claude Code. Read it fully before touching anything.

---

## Project

A local AI inference harness. Double-clickable binary, always-on, browser-based management UI. No cloud, no telemetry, no dependencies at runtime beyond what the harness manages itself.

- **Docs:** `docs/architecture.md`, `docs/roadmap.md`
- **Language:** Go
- **Target OS:** Windows native, Linux (GTK-based systray)

Read the architecture doc before writing any code. It defines component boundaries, package names, and key design decisions that must be respected.

---

## Repo structure

```
cmd/
  harness/          ← main entry point, wires everything together
internal/
  agent/            ← agent registry (persona/rules/notes definitions)
  agentloop/        ← native agent turn loop (tool calls, approvals, doom detection)
  api/              ← optional OpenAI-compatible HTTP server
  approvals/        ← layered permission evaluator + destructive-command classifier
  config/           ← config schema, defaults, validation (no SQL)
  db/               ← SQLite persistence: config, metrics, projects, migrations
  embedder/         ← embedding sidecar client
  git/              ← go-git wrapper (memory repos)
  home/             ← harness home (~/.harness) resolution
  index/            ← flat vector index (vectors.bin + manifest.json)
  inference/        ← OpenAI-compatible HTTP client for llama-server
  logbuf/           ← in-memory log rings for the UI
  memory/           ← memory repo access + project repo scaffolding
  memoryops/        ← semantic-memory operations (embed-on-save, rebuild, dedup, scoring)
  metrics/          ← typed metrics API + recorder
  proc/             ← process manager (llama-server + embedder)
  project/          ← project model and store contract
  prompt/           ← layered prompt assembler
  queue/            ← bounded in-process request queue
  reqid/            ← request-id propagation via context
  retrieval/        ← blended semantic + recency episode scoring
  runtime/          ← mutable service graph, wiring, config re-apply
  session/          ← session lifecycle
  tools/            ← tool registry + built-in tools (sandboxed)
  tray/             ← system tray (fyne-io/systray), single-instance
  ui/               ← management web UI (net/http + html/template)
assets/             ← embedded templates, CSS, htmx
migrations/         ← embedded SQL schema (single squashed migration)
internal/httpclient/     ← shared HTTP client construction
docs/
  architecture.md
  roadmap.md
```

`harness.db` (SQLite: config single-row typed table + metrics history + projects) lives under the harness home `~/.harness/`, created on first run. It is machine-local and never committed.

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
- There is no CLI. No subcommands. Project memory repos are created and managed through the `/projects` page: an existing git directory is used as-is, a non-git directory is initialized with `go-git`, and an omitted directory defaults to `~/.harness/projects/<slug>/`.
- All persistent state (config + metrics + projects) lives in `harness.db` (SQLite, under `~/.harness/`). The schema is a single squashed migration; schema changes edit `migrations/0001_init` in place until first release (delete `harness.db` after editing it — `db.Open` fails fast on a version mismatch).
- `harness.db` is not committed to git. It is machine-local.

### UI
- Server-side rendering only: `net/http` + `html/template` + htmx + SSE.
- No JavaScript frameworks. No build step. No `node_modules`.
- Static assets (CSS, htmx) embedded via `embed.FS` and compiled into the binary.
- The status page is the first thing the user sees. If there are startup errors, it displays them as a clear checklist: what is wrong, why, and what the user needs to do to fix it. A "Retry" button re-attempts validation without restarting the binary.
- Error states to handle explicitly in the UI: first run (no config saved yet), `harness.db` cannot be opened, project memory repo missing or invalid, llama-server binary not found, model file not found, llama-server failed to start, embedder failed to start.

### Process management
- `systray` (fyne-io/systray) for the tray icon. It blocks `main()` — all services start before calling `systray.Run()`.
- Single-instance enforcement via named mutex (Windows) or file lock (Linux).
- On double-click when already running: do nothing, exit the second instance silently.
- On Quit from tray: cancel running tasks, flush live sessions (summarize + commit), drain the queue, terminate child processes, exit.

---

## Build dependencies

**Linux:** the systray requires CGO and GTK development headers:

```sh
# Debian/Ubuntu
sudo apt install libayatana-appindicator3-dev

# Fedora
sudo dnf install libayatana-appindicator3-devel

# Arch
sudo pacman -S libayatana-appindicator
```

Windows builds need no external development libraries.

---

## Startup sequence

```
1. Acquire single-instance lock → if already held, exit silently
2. Start UI server (port 3000) → always succeeds, browser opens if not already open
3. Open harness.db → surface error in UI if the database cannot be opened or migrated
4. Load config row → if the user has never saved, show the first-run CTA and stop here
5. Validate project memory repos → surface error in UI if missing or not a git repo
6. Start llama-server process → surface error in UI if binary missing or startup fails
7. Start embedder sidecar → surface error in UI if binary missing or startup fails
8. Begin health check loops for llama-server and embedder
9. Start API server if enabled in config
10. Hand off to systray.Run()
```

Steps 3–9 can fail independently. The UI reflects the state of each. A "Retry" button re-runs steps 3–9 without restarting the binary.

---

## Config

Configuration is stored as a single-row typed table (`config`) in `harness.db` under `~/.harness/`. There is no on-disk config file — the user edits settings through the `/config` page in the management UI.

On first run the row is seeded from `config.Defaults()`. Until the user saves at least once, `saved_at` is NULL and the status page shows a "Set up your harness" CTA pointing at the config editor.

Required fields (validated on save): model binary, model path, embedder binary, embedder model path, model/embedder/UI/API ports.

Schema lives in [internal/config/config.go](internal/config/config.go). The Go struct is flat per section (Model, Embedder, Agent, Project, UI, API, Prompt, Queue, Metrics, Log, Loop); DDL column names are snake_case with the section as prefix (e.g. `model_ctx_size`, `prompt_memory_token_budget`).

---

## Project memory repos

One plain git repo per project under `~/.harness/projects/<slug>/` (or a user-provided directory), read and written via `go-git` — no git binary required. The `global` project is simply the project that is active by default; it behaves like any other. Prompt memory (`rules.md`, `user.md`, `facts.md`, agent notes, episodes) is always read from the **active** project's repo; the global repo's `agents/` directory additionally serves as the fallback *definition* library (`persona.md`, `rules.md` only — notes never fall back).

Per-repo structure:
```
~/.harness/projects/<slug>/    ← git repo, one per project
  rules.md                     ← project rules
  user.md                      ← facts about the user (this project)
  facts.md                     ← promoted facts (this project)
  agents/<name>/{persona.md, rules.md, notes.md}
  sessions.jsonl               ← append-only session log
  episodes/<agent>/<timestamp>.md
  index/_episodes/{vectors.bin, manifest.json}
  artifacts/
```

## Current milestone

Check `docs/roadmap.md` for the current milestone and do not advance until all of its acceptance tests pass.
