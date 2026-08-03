# Architecture

## Overview

A local AI inference harness with git-backed project memory, layered prompt
assembly, a browser-based management UI, a first-party native agent loop, an
approval-gated local tool layer, and a planned declarative pipeline runner. The
harness owns chat, tool-call orchestration, tool execution, and planned `.hp`
pipeline execution locally; external coding agents are references for design
patterns, not runtime dependencies.

The harness runs as a native desktop binary (Windows and Linux). Windows and
Linux are equal first-class targets; platform-specific behavior must be
designed, tested, and documented for both rather than treating either OS as
best-effort. It starts silently, opens the management UI in the default browser
if not already open, and lives in the system tray until explicitly quit. The
browser UI is the only user-facing surface — all errors (unconfigured on first
run, missing model, llama-server failures, missing memory repo) are surfaced
there, not in a terminal.

The binary targets llama-server as the inference backend and uses a separate
embedding sidecar for semantic memory.

## Documentation map

This repository distinguishes three kinds of documents. Keep facts in their one
canonical home and link rather than duplicate.

| Kind | Documents |
| --- | --- |
| **Current reference** — what `main` implements now | [packages.md](packages.md), [tools.md](tools.md), [memory.md](memory.md), [filesystem-security.md](filesystem-security.md), [runtime-lifecycle.md](runtime-lifecycle.md) |
| **Roadmap** — planned deltas, acceptance criteria, open decisions | [roadmap.md](roadmap.md), [tool_roadmap.md](tool_roadmap.md), [memory_roadmap.md](memory_roadmap.md), [dsl_roadmap.md](dsl_roadmap.md) |
| **Planned specification** — a detailed contract for unimplemented functionality | [DSL.md](DSL.md) |

Current-reference documents describe the code as it is; they use no PR
numbers, migration phases, or milestone labels to explain behavior. Roadmaps
may retain milestone and history language where it is genuinely part of
planning.

This document is the architectural index: the component and data-flow map, the
startup sequence, the cross-cutting invariants, short component summaries, and
the design decisions that genuinely span multiple domains. Package-by-package
detail lives in [packages.md](packages.md).

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

The browser UI is the primary chat/task surface. The optional OpenAI-compatible
API server remains available for external clients, but first-party agent-loop
and pipeline execution stay inside the harness.

How this graph is actually composed, re-applied, and torn down is the runtime
lifecycle; see [runtime-lifecycle.md](runtime-lifecycle.md).

## Startup Sequence

The UI server starts first and always succeeds. Every later step can fail
independently, and each failure becomes a line item on the status page with a
Retry button that re-runs the sequence without restarting the binary.

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

The exact lifecycle machinery (generation construction, apply transaction,
shutdown protocol) is documented in [runtime-lifecycle.md](runtime-lifecycle.md).

## Cross-Cutting Invariants

These invariants span multiple packages and must not be weakened by a
local change.

- **Every listener binds `127.0.0.1`, never `0.0.0.0`.** Neither the UI server
  nor the API server has an authentication layer, so the bind address is the
  security boundary.
- **The UI server starts first, always.** Config loading, memory repo
  validation, llama-server startup, and embedder startup all happen after the
  UI is serving; any failure is displayed in the UI as a setup error.
- **Two HTTP servers, one binary, separate ports, never merged.** The UI server
  is always on; the OpenAI-compatible API server is opt-in.
- **No CLI.** There are no subcommands; project memory repos are created and
  managed through the `/projects` page.
- **Operational SQLite state lives in `harness.db`** (config, metrics,
  projects; single squashed migration, under `~/.harness/`), and `harness.db`
  is never committed to git. Semantic memory, sessions, episodes, sidecars,
  and indexes persist separately in the git-backed project memory repos.
- **One git-backed memory repo per project** — no git binary required.
  Repository *file I/O* goes through rooted `rootfs` handles; *git operations*
  (init, commit, workspace ops) go through the go-git wrapper.
- **`sessions.jsonl` is append-only.** It is never rewritten or truncated.
- **Structured `[key:value]` tags in commit messages** keep memory-repo history
  readable without a separate metadata store for commit intent.
- **Pin before authorize.** Every configured-tree filesystem operation acts
  through `internal/rootfs` pinned handles; `internal/pathid` decides physical
  identity. See [filesystem-security.md](filesystem-security.md).
- **One repository-wide mutation coordinator per physical repository
  identity.** Git mutations, index publication, and project-repo scaffolding
  serialize on the same object; an alias spelling cannot split one repository
  across two locks. See [filesystem-security.md](filesystem-security.md).
- **`runtime` is the composition root and each generation is the sole owner of
  its resources.** The applied runtime state is the authority for what is
  actually running. See [runtime-lifecycle.md](runtime-lifecycle.md).
- **The application lock order is `applyMu` then `rt.mu`.** Apply, project
  edit, active-agent write, and shutdown all serialize behind `applyMu`.
  See [runtime-lifecycle.md](runtime-lifecycle.md).
- **Everything fails closed.** A filesystem path that cannot be resolved, a
  repository whose identity cannot be verified, a memory repo that cannot be
  scope-checked — all are refusals, never a guess.
- **Go subpackages are not namespaces.** `internal/agentloop` and
  `internal/agent` share no privileges; boundaries are enforced by imports, not
  by directory nesting.

## Component Summaries

A short map of the architecture's packages. Full boundaries, ownership, and
dependency directions are in [packages.md](packages.md).

### Foundation and security

- **`internal/pathid`** — physical filesystem identity. Decides where a path
  physically is; answers containment and identity for the sandbox, the C2
  memory-repo lock, and the git write lock.
- **`internal/rootfs`** — rooted filesystem operations through pinned
  `os.Root` handles. Acts on the place pathid resolved, not on the name that
  led to it.
- **`internal/coord`** — one mutation gate per physical repository identity.
- **`internal/home`** — harness home (`~/.harness`) resolution and skeleton.
- **`internal/reqid`, `internal/tokens`, `internal/vector`,
  `internal/summarizerprompt`** — small shared-policy or hidden-state packages.
- **`internal/httpclient`** — shared HTTP client construction.

### Domain and storage

- **`internal/config`** — config schema, defaults, validation; no SQL.
- **`internal/project`** — project model, store contract, and the workflow
  that sequences edits with memory-repo setup.
- **`internal/db`** — SQLite persistence and the schema migration.
- **`internal/git`** — the single go-git wrapper for memory repos.
- **`internal/memory`** — memory-repo infrastructure: layout, scaffolding,
  pinned reads/writes, promotion, identity proofs.
- **`internal/session`** — conversation lifecycle and the explicit
  `pending`/`complete` save protocol.
- **`internal/index`** — the flat vector index format.
- **`internal/retrieval`** — blended semantic + recency scoring.
- **`internal/memoryops`** — semantic-memory orchestration: embed-on-save,
  rebuild, dedup, scoring.
- **`internal/metrics`** — typed metrics API.
- **`internal/agent`** — agent/persona registry.

### Execution

- **`internal/inference`** and **`internal/embedder`** — OpenAI-compatible HTTP
  clients for llama-server and the embedding sidecar.
- **`internal/queue`** — bounded in-process request queue.
- **`internal/proc`** — spawn/monitor/restart of the child processes.
- **`internal/prompt`** — layered prompt assembly.
- **`internal/parser`** — language front-ends behind `ast_*`.
- **`internal/tools`** — the tool registry, sandbox, and built-in tools.
- **`internal/approvals`** — layered permission evaluator.
- **`internal/governor`** — tool-output transforms (skeletonizer, folder, tee,
  token gate).
- **`internal/agentloop`** — the native agent turn loop.
- **`internal/api`** — the optional OpenAI-compatible server.

### Presentation and composition

- **`internal/logbuf`** — in-memory log rings.
- **`internal/ui`** — the management web UI.
- **`internal/tray`** — system tray and single-instance enforcement.
- **`internal/runtime`** — composition and lifecycle root.
- **`cmd/`** — `harness` (main), `eval-retrieval` (developer eval harness),
  `fsaudit` (filesystem-call audit).
- **`assets/`, `migrations/`** — embedded UI assets and the SQL schema.

## Key Design Decisions

Decisions that genuinely span multiple domains, kept as durable rationale.

**First-party chat and tool execution.** The harness owns the browser chat/task
surface, native agent loop, and local tool execution. External coding agents
may inspire design choices, but they are not runtime dependencies.

**Desktop app behavior, no terminal required.** Double-click to start, system
tray to quit. Single-instance enforced via named mutex (Windows) or file lock
(Linux). Browser opens automatically on first launch. All errors surface in the
browser UI.

**htmx over JavaScript.** The management UI is read-mostly with a handful of
simple actions. htmx + SSE + `html/template` covers everything without a build
step, bundler, or `node_modules`. Ships as a single binary via `embed.FS`.

**go-git over git binary.** Removes the git-on-PATH requirement. Pure Go, no
subprocess.

**Embedder as sidecar.** A separate llama-server process in embedding mode
reuses the same process-management pattern as the chat model — uniform failure
handling, restart logic, and health checking — and keeps the harness free of
Python dependencies.

**Single SQLite file for operational state.** Config (single-row typed table),
metrics, project identity, and runtime control state share `harness.db` under
the harness home, separate from project memory repos. One `*sql.DB` handle is
opened and passed to subsystems; no per-package connection.

**Project memory repos are explicit git repos.** Provided git directories are
used as-is, provided non-git directories are initialized with go-git, and
omitted directories create `~/.harness/projects/<slug>/` with go-git. No cwd
inference and no terminal-only setup path.

**Append-only sessions.jsonl.** Never mutate, only append. It is the session
save/recovery-state log — each line records an explicit `pending`/`complete`
save attempt — not a general-purpose audit log.

**Commit message tags.** Structured `[key:value]` tags keep memory-repo history
readable and auditable without a separate metadata store. Episode discovery
uses the project memory tree and vector manifests, not a bespoke git-log query
API.

**Two HTTP servers, one binary.** The UI server (always on) and the API server
(opt-in) are separate and loopback-only. The API server is never required for
the first-party browser workflow.

**Approval-gated tools.** Read-only tools default to enabled; anything that
writes, executes, or touches a remote defaults to off and passes through the
approval layer when enabled. See [tools.md](tools.md).

**Filesystem security as two primitives.** `internal/pathid` (physical
identity) and `internal/rootfs` (rooted operations) implement
pin-before-authorize; the threat model is documented once in
[filesystem-security.md](filesystem-security.md).

**Runtime composition is explicit.** The runtime owns the service graph,
applies config changes as one serialized transaction, and retains ownership of
anything whose termination is unconfirmed. See
[runtime-lifecycle.md](runtime-lifecycle.md).

**Staged dependency ordering.** Interactive agent-loop execution came first,
safe write/shell permissions second, storage-layout stabilization third, and
declarative multi-step automation is planned after those foundations (the
pipeline DSL — see [DSL.md](DSL.md) and [dsl_roadmap.md](dsl_roadmap.md)).
