# Package Boundaries

> **Current reference.** This document describes what `main` implements now.
> Roadmaps live in [roadmap.md](roadmap.md), [tool_roadmap.md](tool_roadmap.md),
> [memory_roadmap.md](memory_roadmap.md), and [dsl_roadmap.md](dsl_roadmap.md).

This document maps the Go package layout and explains each boundary: what a
package owns, what it deliberately does not own, its dependency direction, and
why the boundary exists. Packages are grouped by architectural layer, not
alphabetically, because the layer tells you the intended dependency direction.

## Reading the layers

| Layer | Role | Packages |
| --- | --- | --- |
| Foundation / security | primitives everything else depends on | `pathid`, `rootfs`, `coord`, `home`, `reqid`, `tokens`, `vector`, `httpclient` |
| Domain / storage | owned state, contracts, and their persistence | `config`, `project`, `db`, `git`, `memory`, `session`, `index`, `retrieval`, `memoryops`, `metrics`, `agent` |
| Execution | live request and agent-loop machinery | `inference`, `embedder`, `queue`, `proc`, `prompt`, `parser`, `tools`, `approvals`, `governor`, `agentloop`, `api` |
| Presentation / composition | user surface and process composition | `logbuf`, `ui`, `tray`, `runtime`, commands, embedded assets/migrations |

Dependencies should point "down" the table: presentation depends on execution
and domain; execution depends on domain; domain depends on foundation. The
exceptions are deliberate and noted in place.

---

## Foundation and security

### pathid

- **Owns:** physical filesystem identity. `Resolve` returns an opaque `ID`;
  `Equal`, `Contains`, and `Key` are the only comparisons, and `Same` /
  `SameOrWithin` are the high-level identity and containment operations.
- **Does not own:** any filesystem operation. It answers where a path
  physically is and nothing else. See
  [filesystem-security.md](filesystem-security.md).
- **Dependency direction:** none — it is a leaf. `rootfs`, `coord`, `git`,
  `index`, `memory`, `tools`, and `config`/`detect` consume it.
- **Why the boundary exists:** every component that enforces containment or
  identity (tool sandbox, C2 memory-repo lock, git write lock) must reach the
  same answer. A shared leaf prevents each component from re-implementing
  junction, case, and 8.3 resolution differently.

### rootfs

- **Owns:** rooted filesystem access through pinned `os.Root` handles: `Root`,
  `OpenIdentified`, `Anchor`, `Set`, `Target`, atomic and append-only writes,
  and verified removal.
- **Does not own:** identity resolution (that is `pathid`'s job) or subprocess
  sandboxing.
- **Dependency direction:** depends on `pathid`; consumed by `git`, `memory`,
  `index`, `tools`, `governor`, and `runtime`.
- **Why the boundary exists:** it is the other half of the pathid pair —
  pathid decides where a path is; rootfs acts on that place through a handle
  rather than a name. See [filesystem-security.md](filesystem-security.md).

### coord

- **Owns:** the process-wide registry of repository mutation gates, one per
  physical repository identity.
- **Does not own:** any filesystem or git operation itself.
- **Dependency direction:** depends on `pathid`; consumed by `git`, `index`,
  `memory`, and `memoryops`.
- **Why the boundary exists:** git mutations, index publication, and
  project-repo scaffolding must serialize on one object per physical
  repository so an alias spelling cannot split one repository across two locks.
  See [filesystem-security.md](filesystem-security.md).

### home

- **Owns:** the harness-home path (`~/.harness`) and the stable machine-local
  directory skeleton (`projects/`, `logs/`, `cache/`).
- **Does not own:** any per-project layout or repository state — see `memory`.
- **Dependency direction:** depends on `project` (slug validation); consumed
  by `main` and `runtime`.

### Small shared-policy and hidden-state packages: reqid, tokens, vector, summarizerprompt

These are intentionally tiny packages so a single policy or a piece of hidden
state has exactly one home:

- **`reqid`** — threads a per-request identifier through `context.Context` so
  handlers, queue dispatch, prompt assembly, and logs correlate one request
  without adding request-id parameters to every package API. Leaf; consumed by
  `api`, `prompt`, and `runtime`.
- **`tokens`** — token-count estimation. Leaf; consumed by `prompt`, `memory`,
  and `governor` (the B5 token gate uses the same counter as budgeting).
- **`vector`** — vector math for the index. Leaf; consumed by `index` and
  `memoryops`.
- **`summarizerprompt`** — the default episode-summarizer system prompt. Leaf;
  consumed by `config` to seed the configurable default.

None of these should grow domain logic; when their one job moves into a larger
package, the small package dissolves rather than absorbing features.

### httpclient

- **Owns:** shared `*http.Client` construction (timeouts, dial settings) used
  by every outbound HTTP consumer.
- **Does not own:** any specific endpoint, auth, or retry policy.
- **Dependency direction:** leaf; consumed by `inference`, `embedder`, `proc`,
  and `runtime`.

---

## Domain and storage

### config

- **Owns:** the config schema, defaults, validation, and the seed values. It is
  the single source of truth for what a config row is. No SQL.
- **Does not own:** persistence (`db` owns the SQLite `config` table) or any
  live service-graph state.
- **Dependency direction:** depends on `project`, `summarizerprompt`, and
  `tools` — tool-enablement defaults come from `tools.BuiltinDefaultEnabled`,
  so config deliberately depends on the execution layer for that one fact.
  Consumed by `db`, `prompt`, `memoryops`, `agentloop`, `ui`, and `runtime`.
- **Why the boundary exists:** config stays flat typed state plus validation;
  it never owns SQL or decides how subsystems are wired.

### project

- **Owns:** the project domain model: slug/display/path validation, lifecycle
  status, the `Store` interface, and the workflow contract that sequences
  metadata edits with memory-repo setup.
- **Does not own:** SQL persistence (`db`), repository infrastructure
  (`memory`), or prompt logic. It defines the `MemoryRepoManager` interface;
  `memory.ProjectRepoManager` implements it.
- **Dependency direction:** a leaf among domain packages — it imports no
  internal packages. That is what lets `config`, `db`, `home`, `memory`,
  `session`, `prompt`, `ui`, and `runtime` depend on it without cycles.
- **Why the boundary exists:** `project` owns the *contract* (what a project
  is and how edits to it are sequenced); `memory` owns the *plumbing* (how a
  project's git repo is scaffolded and moved). `project` never imports
  `memory`, so the contract stays testable in isolation and the direction is
  one-way.

### db

- **Owns:** all SQLite persistence and the single squashed schema/migration. It
  is the concrete implementation of the `config.Store`, `metrics.Store`, and
  project-store surfaces.
- **Does not own:** domain rules — it persists what the domain packages define.
- **Dependency direction:** depends on `config`, `project`, and `metrics`;
  consumed by `main` and `runtime`.

### git

- **Owns:** the single go-git wrapper for memory repos: init/open, commit,
  workspace operations, commit-message construction, and the `WithMutation`
  repository transaction.
- **Does not own:** tool-level scope policy (that lives in `tools`), memory
  semantics, or the projects table.
- **Dependency direction:** depends on `rootfs`, `pathid`, and `coord`;
  consumed by `memory`, `tools`, and `runtime`.
- **Why the boundary exists:** keeping the wrapper policy-free means the memory
  writer and the `git_*` tools can share it without importing each other.

### memory

- **Owns:** project memory repository infrastructure: `DirReader` pinned-root
  reads/writes, the canonical layout, scaffolding and validation, move/copy,
  the promotion service, and identity proofs.
- **Does not own:** project domain rules (that is `project`'s), session
  lifecycle (that is `session`'s), or the vector index format (`index`).
- **Dependency direction:** depends on `git`, `rootfs`, `pathid`, `coord`, and
  `project`; consumed by `session`, `prompt`, `agent`, `memoryops`, `ui`, and
  `runtime`.
- **Why the boundary exists:** `memory` is the repository *infrastructure*
  layer. Keeping it separate from `project` (contracts) and `session`
  (lifecycle) is what keeps all three acyclic. See
  [memory.md](memory.md).

### session

- **Owns:** conversation lifecycle: start/append/end/resume, the explicit
  `pending`/`complete` save transaction, sidecar publication, episode
  summarization and commit, and flush.
- **Does not own:** the prompt assembler (summarization bypasses it
  deliberately), tool execution, or the vector index.
- **Dependency direction:** depends on `git`, `memory`, `project`, `inference`,
  and `summarizerprompt`; consumed by `runtime` and `memoryops`.
- **Why the boundary exists:** the durable save/recovery protocol is a
  correctness surface of its own; it is not memory-repo plumbing or prompt
  assembly. See [memory.md](memory.md).

### index

- **Owns:** the flat vector index format: `vectors.bin` + `manifest.json`,
  content-hash idempotence, rooted open/create/upsert, and flat cosine scans.
- **Does not own:** episode-specific semantics, embedding, or retrieval
  scoring.
- **Dependency direction:** depends on `vector`, `rootfs`, `pathid`, and
  `coord`; consumed by `prompt`, `memoryops`, and `retrieval`.
- **Why the boundary exists:** the on-disk format is isolated from prompt and
  memory logic so a format change is a one-package change.

### retrieval

- **Owns:** the pure blended scoring pipeline (semantic + recency) and the D3
  trace types and sink.
- **Does not own:** the embedder client, the index, or episode storage;
  `memory_query` reaches it through `memoryops.EpisodeScorer`.
- **Dependency direction:** depends on `index` (via interface); consumed by
  `prompt` and `memoryops`.
- **Why the boundary exists:** retrieval stays pure and testable; embedding
  and index access are injected. `index` stores vectors, `retrieval` scores,
  and `memoryops` orchestrates the two for episodes — three different jobs in
  three packages.

### memoryops

- **Owns:** semantic-memory operations on top of the repo, embedder, and index:
  `EpisodeScorer`, `AfterSaveEmbed`, `EpisodeIndex`, episode rebuild, and dedup.
- **Does not own:** the index format (`index`), the scoring math (`retrieval`),
  the repository plumbing (`memory`), or session saving (`session`).
- **Dependency direction:** depends on `config`, `coord`, `embedder`, `git`,
  `index`, `memory`, `retrieval`, `session`, and `vector`; consumed by `runtime`
  and `ui`.
- **Why the boundary exists:** it is the orchestration layer that turns "save
  an episode" into "embed, upsert into the index, commit under the
  coordinator". See [memory.md](memory.md).

### metrics

- **Owns:** the typed metrics API and recorder. No persistence.
- **Dependency direction:** leaf; `db` implements its store, `runtime` and `ui`
  consume the API.

### agent

- **Owns:** the agent registry: enumerates `agents/` directories, exposes
  persona/rules/notes paths, validates names, and tracks the active agent via
  callbacks.
- **Does not own:** active-agent persistence (that is config/db) or prompt
  assembly.
- **Dependency direction:** depends on `memory` (repo interface); consumed by
  `prompt` and `runtime`.

---

## Execution

### inference and embedder

- **Owns:** `inference` — the OpenAI-compatible HTTP client for the chat model;
  `embedder` — the embedding-sidecar client.
- **Does not own:** process lifecycle (`proc`), request serialization (`queue`),
  or any prompt/memory logic.
- **Dependency direction:** both depend on `httpclient`. `inference` is
  consumed by `queue`, `session`, `prompt`, `api`, `agentloop`, and `runtime`;
  `embedder` by `prompt`, `memoryops`, and `runtime`.
- **Why the boundary exists:** swapping llama-server for another backend
  touches `inference` only.

### queue

- **Owns:** the bounded in-process request queue: backpressure, cancellation,
  and terminal resolution of accepted requests.
- **Does not own:** durable recording (`session` owns that) or model
  management.
- **Dependency direction:** depends on `inference`; consumed by `api`,
  `agentloop`, and `runtime`.

### proc

- **Owns:** spawn/monitor/restart of llama-server and the embedder sidecar.
- **Dependency direction:** depends on `httpclient`; consumed by `runtime`.

### prompt

- **Owns:** layered prompt assembly: layer ordering, resolution rules (project
  overrides, global persona/rules fallback), memory budget, and conversation
  reserve.
- **Does not own:** where files live (`memory` does), retrieval math
  (`retrieval` does), or the request queue.
- **Dependency direction:** depends on `agent`, `config`, `embedder`, `index`,
  `inference`, `memory`, `project`, `reqid`, `retrieval`, and `tokens`;
  consumed by `runtime` and `api`.
- **Why the boundary exists:** prompt assembly is the only place layer
  ordering and budget rules live; keeping it out of memory/session avoids
  duplicating the rule set. See [memory.md](memory.md).

### parser

- **Owns:** language front-ends behind `ast_*`: the `FrontEnd` contract,
  extension→front-end resolution, and the Go single-file front-end.
- **Dependency direction:** leaf; consumed by `tools` and `governor`.
- **Why the boundary exists:** `ast_*` tool declarations are generated from the
  registry at construction, so adding a language means adding a front-end, not
  tools.

### tools

- **Owns:** the tool registry, descriptors, schemas, `CallInfo`, the built-in
  tool implementations, the sandbox helpers (rooted `openTarget` and pathname
  `validatePath`), the C2 memory-repo scope check, and the toolout spill
  reader.
- **Does not own:** approval evaluation (`approvals`), output transforms
  (`governor`), or the loop (`agentloop`).
- **Dependency direction:** depends on `parser` and, through `rootfs`/`pathid`/
  `git`, the foundation. Consumed by `agentloop`, `approvals`, `governor`,
  `config`, and `runtime`. `tools` does not import `approvals` — the direction
  is approvals → tools.
- **Why the boundary exists:** the tool layer is the model-callable surface; it
  stays approval-free and transform-free so each sibling can compose over it.
  See [tools.md](tools.md).

### approvals

- **Owns:** layered permission evaluation: builtin defaults (generated from
  `tools.BuiltinDescriptors`), user config, and session rules; allow/ask/deny
  semantics; the conservative `exec` rule.
- **Does not own:** tool execution or session persistence.
- **Dependency direction:** depends on `tools`; consumed by `agentloop` and
  `runtime`.

### governor

- **Owns:** tool-output transforms between execution and context injection: B1
  skeletonizer, B2 output folder, B3 tee-on-failure, B5 token gate.
- **Does not own:** tool execution or the loop.
- **Dependency direction:** depends on `parser`, `rootfs`, `tools`, and
  `tokens`; consumed by `agentloop` and `runtime`.
- **Why the boundary exists:** transforms are not model-callable and are
  invisible to the agent as tools; they belong in their own package rather than
  inside `tools`. See [tools.md](tools.md).

### agentloop

- **Owns:** the native agent turn loop: dispatch, approval gating, doom-loop
  detection, cancellation, and event emission.
- **Does not own:** agent definitions (`agent`), tool implementations
  (`tools`), or the prompt assembler.
- **Dependency direction:** depends on `approvals`, `config`, `inference`, and
  `tools`; consumed by `runtime`.
- **Why the boundary exists:** `agentloop` (the engine) and `agent` (the
  registry) are deliberately separate — the loop is execution machinery, the
  registry is domain state. They are siblings, not parent and child.

### api

- **Owns:** the optional OpenAI-compatible HTTP server; routes requests through
  the same prompt/queue/inference/session path.
- **Dependency direction:** depends on `inference`, `queue`, and `reqid`;
  consumed by `runtime`.
- **Why the boundary exists:** a separate package keeps the optional external
  surface from leaking into the UI or the loop.

---

## Presentation and composition

### logbuf

- **Owns:** in-memory ring buffers for harness and process logs. Leaf; consumed
  by `ui` and `runtime`.

### ui

- **Owns:** the management web UI: routes, server-rendered templates,
  htmx/SSE streaming, embedded assets, and the `ServiceDeps` snapshot contract.
- **Does not own:** domain logic; it adapts package boundaries through the
  runtime.
- **Dependency direction:** depends on `config`, `logbuf`, `memory`, `metrics`,
  `project`, and `tokens`; consumed by `runtime` and `main`.

### tray

- **Owns:** the system tray, single-instance enforcement, and the Quit path.
  Leaf; consumed by `main`.

### runtime

- **Owns:** the composition and lifecycle root. See
  [runtime-lifecycle.md](runtime-lifecycle.md).
- **Dependency direction:** depends on every layer; consumed by `main`.
- **Why the boundary exists:** `runtime` is the single place the service graph
  is built, re-applied, and torn down. Keeping that composition in one package
  means no other package wires subsystems together.

### Commands

- `cmd/harness` — main entry point; wires the runtime, UI server, db, tray,
  and shutdown.
- `cmd/eval-retrieval` — developer-side retrieval eval harness, not part of the
  shipped binary.
- `cmd/fsaudit` — direct-filesystem-call audit; fails CI when an
  un-allowlisted `os`/`path/filepath` call appears. See
  [filesystem-security.md](filesystem-security.md).

### Embedded assets and migrations

- `assets/` — UI templates/CSS/htmx compiled into the binary via `embed.FS`.
- `migrations/` — the single squashed SQL schema, embedded and applied by
  `db`.

---

## Cross-cutting notes

- **`runtime` is the composition/lifecycle root.** No package other than
  `cmd/harness` builds subsystems; `main` constructs `runtime`, and `runtime`
  constructs everything else.
- **`pathid` decides identity; `rootfs` performs rooted operations.** One
  answers where a path physically is; the other acts on that place through a
  pinned handle. Neither substitutes for the other.
- **`coord` is the single physical-repository mutation coordinator.** It is
  keyed by `pathid.ID.Key()`, never by a pathname, so junction aliases of one
  repository share a gate.
- **`project` owns domain/workflow contracts; `memory` supplies repository
  infrastructure.** `project` defines the `MemoryRepoManager` contract;
  `memory.ProjectRepoManager` implements it; `project` never imports `memory`.
- **`index`, `retrieval`, and `memoryops` have different responsibilities.**
  `index` stores vectors, `retrieval` scores, `memoryops` orchestrates the two
  for episodes.
- **`tools`, `approvals`, and `agentloop` remain sibling boundaries.** `tools`
  neither evaluates approvals nor transforms output; the direction is
  approvals/governor → tools and agentloop → tools.
- **Small packages (`reqid`, `tokens`, `vector`, `summarizerprompt`) are
  intentional shared-policy or hidden-state boundaries.** They exist so one
  policy or one hidden value has one home that both sides of a larger split
  can depend on.
- **Go subpackages are not namespaces.** `internal/agentloop` and
  `internal/agent` share no privileges; one is execution machinery, the other a
  domain registry. Subpackage status grants nothing — boundaries are enforced
  by imports, not by directory nesting.
- **Dependency exceptions are deliberate.** `config` imports `tools` (for
  tool-enablement defaults) and `summarizerprompt` (for the summarizer
  default); these cross-layer edges exist so seed values have a single source
  of truth.
- **Planned packages are not current.** `internal/pipeline` and the
  `internal/dsl` front-end are planned for the pipeline DSL; they are not part
  of the current layout and are not described here.
