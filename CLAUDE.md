# CLAUDE.md

This file is the single entry point for every coding agent working in this
repository — Claude Code reads it automatically, and `AGENTS.md` points here for
tools that look for that name instead. There is deliberately only one copy of
these rules; do not fork them into a second file.

Read it fully before touching anything.

---

## Project

A local AI inference harness. Double-clickable binary, always-on, browser-based management UI. No cloud, no telemetry, no dependencies at runtime beyond what the harness manages itself.

- **Docs:** `docs/architecture.md`, `docs/roadmap.md`
- **Language:** Go
- **Target OS:** Windows native, Linux (GTK-based systray)

Read `docs/architecture.md` before writing any code. It is the architectural index: it
defines the component map, startup sequence, cross-cutting invariants, and key design
decisions that must be respected, and links to the detailed current references. When
your change touches packages, tools, memory, filesystem security, or the runtime
lifecycle, read the corresponding reference too:

- `docs/packages.md` — package boundaries, ownership, dependency direction
- `docs/tools.md` — tool registry, approvals, sandbox, provenance
- `docs/memory.md` — memory layout, session lifecycle, indexing
- `docs/filesystem-security.md` — pathid/rootfs primitives and threat model
- `docs/runtime-lifecycle.md` — generation ownership, apply transaction, shutdown

Each current fact has one canonical home; do not duplicate normative descriptions into
roadmap documents or the architecture overview.

---

## Repo structure

```
cmd/
  harness/          ← main entry point, wires everything together
  eval-retrieval/   ← offline retrieval eval harness (D3 labeled query set)
internal/
  agent/            ← agent registry (persona/rules/notes definitions)
  agentloop/        ← native agent turn loop (tool calls, approvals, doom detection)
  api/              ← optional OpenAI-compatible HTTP server
  approvals/        ← layered permission evaluator + destructive-command classifier
  config/           ← config schema, defaults, validation (no SQL)
  db/               ← SQLite persistence: config, metrics, projects, migrations
  embedder/         ← embedding sidecar client
  git/              ← go-git wrapper (memory repos)
  governor/         ← tool-output transforms: skeletonizer, output folding, token gate
  home/             ← harness home (~/.harness) resolution
  httpclient/       ← shared HTTP client construction
  index/            ← flat vector index (vectors.bin + manifest.json)
  inference/        ← OpenAI-compatible HTTP client for llama-server
  logbuf/           ← in-memory log rings for the UI
  memory/           ← memory repo access + project repo scaffolding
  memoryops/        ← semantic-memory operations (embed-on-save, rebuild, dedup, scoring)
  metrics/          ← typed metrics API + recorder
  parser/           ← source symbol extraction backing ast_map / ast_find
  pathid/           ← physical filesystem path identity (sandbox, C2 lock, git write lock)
  proc/             ← process manager (llama-server + embedder)
  project/          ← project model and store contract
  prompt/           ← layered prompt assembler
  queue/            ← bounded in-process request queue
  reqid/            ← request-id propagation via context
  retrieval/        ← blended semantic + recency episode scoring
  rootfs/           ← rooted filesystem access (os.Root) for sandbox + worktree reads
  runtime/          ← mutable service graph, wiring, config re-apply
  session/          ← session lifecycle
  summarizerprompt/ ← session summarizer prompt template
  tokens/           ← token count estimation
  tools/            ← tool registry + built-in tools (sandboxed)
  tray/             ← system tray (fyne-io/systray), single-instance
  ui/               ← management web UI (net/http + html/template)
  vector/           ← vector math for the index
assets/             ← embedded templates, CSS, htmx
migrations/         ← embedded SQL schema (single squashed migration)
scripts/            ← format.ps1, git hooks + installer
docs/
  architecture.md   ← component map, startup sequence, invariants, links to references
  packages.md       ← package boundaries by layer
  tools.md          ← tool system current reference
  memory.md         ← memory system current reference
  filesystem-security.md ← filesystem threat model + pathid/rootfs primitives
  runtime-lifecycle.md  ← runtime composition, apply transaction, shutdown
  roadmap.md        ← milestones and acceptance tests
  tool_roadmap.md   ← remaining tool surface work
  memory_roadmap.md ← memory layer plan
  DSL.md            ← pipeline DSL specification (planned)
  dsl_roadmap.md    ← pipeline DSL milestones
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
- **Every listener binds `127.0.0.1`, never `0.0.0.0`.** Neither server has an authentication layer, and the UI exposes state-changing routes (`/config`, `/shutdown`, `/task/send`). The origin check stops cross-origin browsers but not a non-browser client that omits the `Origin` header, so the bind address is the security boundary. `TestStart_BindsLoopbackOnly` enforces this — do not weaken it without adding authentication first.
- **The UI server starts first, always.** It must be up before anything else is attempted. Config loading, memory repo validation, llama-server startup, embedder startup — all of this happens after the UI is serving. If anything fails, it is displayed in the UI as a setup error. The user should never need a terminal to diagnose or fix a problem.
- There is no CLI. No subcommands. Project memory repos are created and managed through the `/projects` page: an existing git directory is used as-is, a non-git directory is initialized with `go-git`, and an omitted directory defaults to `~/.harness/projects/<slug>/`.
- Operational SQLite state (config + metrics + projects) lives in `harness.db` (SQLite, under `~/.harness/`). The schema is a single squashed migration; schema changes edit `migrations/0001_init` in place until first release (delete `harness.db` after editing it — `db.Open` fails fast on a version mismatch).
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

The canonical startup sequence is documented in [docs/architecture.md](docs/architecture.md#startup-sequence). The rules to respect:

- The UI server starts first and always succeeds. Config loading, memory repo validation, llama-server startup, embedder startup — all of this happens after the UI is serving. If anything fails, it is displayed in the UI as a setup error.
- Steps 3–9 can fail independently. The UI reflects the state of each. A "Retry" button re-runs steps 3–9 without restarting the binary.

---

## Config

Configuration is stored as a single-row typed table (`config`) in `harness.db` under `~/.harness/`. There is no on-disk config file — the user edits settings through the `/config` page in the management UI.

On first run the row is seeded from `config.Defaults()`. Until the user saves at least once, `saved_at` is NULL and the status page shows a "Set up your harness" CTA pointing at the config editor.

Required fields (validated on save): model binary, model path, embedder binary, embedder model path, model/embedder/UI/API ports.

Schema lives in [internal/config/config.go](internal/config/config.go). The Go struct is flat per section (Model, Embedder, Agent, Project, UI, API, Prompt, Queue, Metrics, Log, Loop); DDL column names are snake_case with the section as prefix (e.g. `model_ctx_size`, `prompt_memory_token_budget`).

---

## Project memory repos

One plain git repo per project under `~/.harness/projects/<slug>/` (or a user-provided directory) — no git binary required. Repository *file I/O* goes through rooted `rootfs` handles; *git operations* (init, commit, workspace ops) go through the go-git wrapper. The `global` project is simply the project that is active by default; it behaves like any other. Prompt memory (`rules.md`, `user.md`, `facts.md`, agent notes, episodes) is always read from the **active** project's repo; the global repo's `agents/` directory additionally serves as the fallback *definition* library (`persona.md`, `rules.md` only — notes never fall back).

The canonical per-repo layout and memory-system reference live in
[docs/memory.md](docs/memory.md); do not duplicate the layout tree here.

## Current milestone

Check `docs/roadmap.md` for the current milestone and do not advance until all of its acceptance tests pass.
