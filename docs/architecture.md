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

#### Explicit session recovery state (PR 11)

A save is an explicit, durable recovery transaction rather than a correctness
signal inferred from timestamps, empty paths, or physical log order. One save
attempt runs under the manager-wide save lock:

1. allocate a monotonic attempt identifier before any fallible work;
2. durably publish the raw conversation sidecar through the rooted writer;
3. append and fsync an explicit `pending` record for that attempt;
4. summarize;
5. publish and commit the episode;
6. append and fsync an explicit `complete` record for the same attempt.

Recovery selects the winning record per session by the attempt identifier
first, then state precedence: for one attempt, `complete` deterministically
supersedes `pending`. Wall-clock timestamps, empty paths, and physical log
order never decide correctness; timestamps remain display/sort-only. A
summarizer, episode-publication, or commit failure leaves a discoverable
`pending` session whose raw sidecar can be resumed.

**Compatibility rule:** records without the explicit fields are legacy records
from before PR 11 — they were only ever appended after a fully successful
save, so they normalize to `complete` ordered by `save_seq`. The log is never
rewritten; new-format state is never inferred from `EpisodePath`. Malformed
hybrids (an unknown state, a state without an attempt, an attempt without a
state, or negative counters) are rejected at append time and skipped at
selection time, so they never influence recovery.

### Runtime (`internal/runtime`)
Owns the mutable service graph behind the harness. `cmd/harness/main.go` creates the UI first, then asks `internal/runtime` to wire and retry the rest of the subsystems after the browser surface is already available.

Responsibilities:
- Hold the active config, config store, project store, process managers, queue, inference client, memory reader, prompt assembler, session manager, API server, tool registry, and log rings.
- Implement the retry/config-save path that revalidates config, memory repo, projects, llama-server, embedder, API server, and session services without requiring a binary restart where possible.
- Adapt package boundaries for the UI: chat/task runners, memory APIs, project health checks, approval routing, and session persistence.
- Keep runtime state behind locks because UI handlers, process events, metrics, and retry callbacks run concurrently.

#### Immutable UI dependency snapshots (PR 8)

The UI never reads individual live getters. Each runtime generation owns one
immutable `ui.ServiceDeps` snapshot — memory repo path/store, agent registry,
session store, committer, dedup checker + threshold, retrieval scorer, index
rebuilder, chat runner, and task runner — bound to that generation's concrete
readers, git handle, and episode index. Every snapshot adapter is bound to
concrete candidate-generation resources (a static assembler over the
candidate's concrete assembler, the candidate's session manager, active
agent, project slug, loop config, and active memory); none dereference
`Runtime` at execution time, so an old snapshot reads and records exclusively
in the project it was published for. `Runtime.AcquireUISnapshot` captures the
current generation's snapshot and pins the generation under `rt.mu`; the UI
server holds the runtime as a `ui.SnapshotProvider`, and every handler calls
`acquireSnapshot` once, uses only fields of that snapshot, and releases on
every completion/error path. The active agent is resolved into
`ServiceDeps.ActiveAgent` per acquisition under the same lock — `/agents/active`
switches it without a generation rebuild — and chat/task handlers fall back to
it for an empty agent field, and the `/chat` and `/agents` pages render their
active-agent marker from the snapshot's value rather than re-reading the
registry's live selection. The provider is installed by both `Runtime.Start`
and `ApplyConfig`, so a retry-only startup still wires generation-backed
handlers.

Lifetime protocol:
- **Publication:** the candidate and its snapshot are built locally, bound to
  the candidate generation, and installed by swapping the live generation
  under the same lock acquisition uses. The old publisher lease is retired
  next; old readers and handles close only after the last acquired snapshot on
  the old generation is released. Acquisition and publication share the lock,
  so a handler cannot select an old snapshot after its generation was retired
  and its readers closed (no load-before-increment window).
- **Handlers:** synchronous handlers release via `defer`. `/chat/send` and
  `/task/send` acquire before reading the runner or session store and transfer
  the release to the detached goroutine, which releases after the entire
  run/stream ends; every pre-launch error path releases exactly once.
  Long-lived SSE subscription handlers do not pin a generation merely for
  remaining connected — the spawned chat/task operation owns the lease.
- **Old references:** a held old snapshot keeps its generation's readers open
  and usable for real rooted operations after a reload, until the release runs.

This replaces the eliminated `memoryHandles`, `genGate`, and
`memoryAPISnapshot` mechanisms (retained in `docs/pr403-findings.md`); the
API request-generation lease (`AcquireRequestGeneration`) is unchanged, and
the API server alone keeps a dynamic assembler because API requests
legitimately use the current generation.

#### Explicit applied runtime state (PR 9)

`Runtime` owns one explicit applied-state record (`appliedState`) containing
the facts needed to compare, publish, and roll back the live system: the
committed config, the active project, the preferred/effective model, and the
actually-running llama/embedder process configuration. The old/live state is
read exclusively from this record — never reconstructed from the mutable
config store or the mutable project store — so a store edit (a project
override change, a global port change) is compared against what was actually
committed, not against a store-derived guess.

`ApplyConfig` runs as one transaction serialized end-to-end by a dedicated
apply lock (`applyMu`); two concurrent applies cannot interleave validation,
preparation, process changes, generation publication, or retirement. The
transaction phases are explicit:

- **prepare** — the candidate and its API server are built locally and left
  unpublished; the API listener is bound (reserving its port) but does not
  accept requests until commit, so a request on the candidate's port can never
  run against a generation that is not the one it was prepared for. A failed
  candidate is discarded wholesale (`applyTx.close`), leaving the installed
  generation and recorded applied state untouched.
- **quiesce** — task loops are cancelled and sessions flushed when a rebuild
  will drop the old generation; these waits run without `rt.mu` so session
  summarization can read live config without deadlocking.
- **commit** — the generation and one coherent applied state are installed
  atomically under `rt.mu`, process reconfigurations are issued from that
  state (never re-derived from the stores), and the bound API listener is
  activated. Commit is structured to be infallible so the recorded applied
  state always describes the live processes, and `ui.ApplyResult.LiveApplied`
  reports exactly what happened.
- **retire** — the old generation's publisher lease is released under the
  same lock acquisition uses, and the previous API server is retired under
  the timeout ownership protocol: a server whose shutdown does not confirm
  termination within the timeout keeps a retained slot (`rt.retiredAPI`)
  until a later Stop confirms it, so the runtime never clears or replaces the
  pointer to a still-serving component.

`project.llama_on_switch=keep` records the actually-running model separately
from the newly preferred model; llama-server is never reconfigured during a
config apply or project switch under keep, the prompt context ceiling and the
inference client track the running model's port/ctx (the harness keeps
talking to wherever llama-server actually runs), and the status UI renders
the running-versus-preferred mismatch honestly from the two recorded values.

The API listener build decision is live-aware: `rebuild` can be forced by
`memoryAPIUnavailable()` finding `rt.apiServer == nil` while the applied
config wants the API running, so an apply rebuilds a missing listener even
when the recorded config is unchanged. The `/agents/active` write
(`setActiveAgent`) takes the same apply lock as `ApplyConfig`, so an apply
that has loaded one agent and is preparing cannot be overwritten by a
concurrent active-agent save, or vice versa; the live config, the recorded
applied config, and the store always agree.

#### Project-edit transaction (PR 10)

`/projects/edit` never constructs and executes `project.Workflow` directly.
`Runtime.EditProject` is the single Runtime-owned project-update surface for
the UI: it serializes the edit end-to-end with `applyMu`, so an edit cannot
interleave with a config apply, an active-agent write, or a shutdown. Inside
the transaction:

- The active project's memory-repository boundary cannot be moved while the
  installed generation still targets it. The edit refuses before any metadata
  or filesystem mutation. The repository identity is settled once as a
  handle-bound proof (`Workflow.SettleUpdate` pins the destination via
  `PinRepoIdentity` and `OpenIdentified`), and that proof is re-verified at the
  moment of mutation (`Workflow.ApplyUpdate`): `SameAs` opens the current path
  and compares the retained handles with `Root.SameDir` (`os.SameFile`), so a
  repointed alias or a same-name physical replacement fails closed even when it
  reuses the pathid key. The settlement is produced only by `SettleUpdate` — the
  decision is private behind `IsSameRepo` and a forged or zero `SettledUpdate`
  is rejected — so an old "same" result can never persist a path that no
  longer identifies the installed reader. Active-project display and
  model-override edits proceed; their live apply runs through the same
  transaction boundary (`applyConfigLocked`), so the reload decision compares
  the freshly-mutated store contents with the recorded applied state — never
  with an "old" value derived from the store the edit already changed. If that
  re-apply fails (config load, validation, or candidate-preparation failure),
  the edit reports failure and restores the captured project row, so the store
  never silently diverges from the live generation.
- Inactive-project repository moves continue through the rooted
  `MoveProjectRepo` workflow (`project.Workflow.Update`), preserving its
  rollback behavior on initialization or move failure. The pre-edit active
  model or repository is never derived from the mutated project store.

`ui.ApplyResult` carries an explicit `Err` so a failed apply is distinguishable
from a successful no-op: `LiveApplied=false` plus `Err=nil` means "nothing
needed changing", while a non-nil `Err` means the apply could not commit and
the installed generation and recorded applied state are untouched.

#### Shutdown lifecycle and ownership protocol (PR 10)

`Runtime.Shutdown` is the one cohesive shutdown lifecycle, serialized with the
apply transaction so a shutdown cannot interleave with a config apply or a
project edit. It replaces the split coordination between `cmd/harness/main.go`
and `Runtime.Stop`: `main` calls `rt.Shutdown(rootCancel, 10s)` and nothing
else, and `Runtime.Stop` is retained only as the no-root-cancel compatibility
wrapper for tests. The lifecycle is explicit:

1. **stop admissions** — the request queue closes its intake
   (`Queue.CloseAdmissions`), so new UI/API chat or task work is refused before
   anything is drained;
2. **cancel root/task contexts** — `rootCancel` stops process managers, the
   queue worker, and UI request contexts before any wait begins;
3. **bounded drain** — task loops are cancelled and live sessions flushed with
   explicit timeouts. The runtime owns exactly one in-flight session flush: a
   save can block on the manager-wide save lock or a hung summarizer, so the
   flush runs detached from any single attempt, retries join the in-flight
   flush instead of stacking another (blocked flushes cannot accumulate saveMu
   waiters or duplicate durable saves), and a new flush starts only after a
   previous one completed with a retryable failure. The flush result is
   published under the flush lock before a broadcast completion channel is
   closed, so an immediate retry can never miss a completion. The summarizer's
   token loop is itself context-aware so a stream that never sends or closes
   cannot hang it;
4. **stop API/queue/process components** — API servers stop under the timeout
   ownership protocol; the queue is waited on with a bounded, context-aware
   wait (`Queue.Wait`), never an unbounded `Queue.Stop`. Queue cancellation is
   terminal: every accepted request — in-flight or buffered — is resolved
   (failed and closed), so consumers ranging a response channel never hang;
5. **release only resources proven idle**;
6. **retain ownership for anything whose termination is unconfirmed**.

A drain timeout is not termination. When a bounded wait expires, the runtime
retains ownership and a later `Shutdown` retries: the queue stays owned, and
the session manager and task runner stay owned together with the complete
generation they are bound to — its readers and handles stay open so a retry can
save through them. API ownership is preserved to termination for every class of
server: active, pending-retired, and previously timed-out. `Queue.Stop` is
never called after a failed bounded drain.

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
- Enforce the sandbox through `internal/rootfs`: tools that read or write file
  content (`read`, `file_list`, `ast_map`, `ast_find`, `edit`) operate through an
  open handle on the owning sandbox root. Tools that hand a path to something
  outside the package — a subprocess working directory, a `go-git` repository —
  validate it with `internal/pathid` instead, because there is no handle to give
  them.
- Pass a typed `CallInfo` to each tool: active project slug, sandbox roots, memory repo paths (C2 scope list), session id, caller identity, HTTP client, optional `MemoryQuery` closure, and optional `GHTokenFn` closure (reads `GITHUB_TOKEN` at call time — never stored).
- Record how tool output was produced with `OriginClass` (`extraction` / `inference`). M12 MR1 adds per-hit memory-content origin; origin metadata never bypasses approvals, sandboxing, or verification.

Tool tiers:
- **Tier 1 — read-only, default-allow:** `read` (range- and locator-addressed), `file_list`, `ast_map`, `ast_find`, `git_status`, `git_diff`, `git_log`, `memory_query` (blended semantic+recency retrieval over the active project episode store; requires embedder)
- **Tier 2 — approval-gated, disabled by default:** `edit` (hash-anchored line operations; anchor hash is only emitted by `ast_map`/`ast_find`, so recon-first is enforced by type), `exec` (argv-array shell with deny filter), `go_test`, `go_lint`
- **Tier 2 write + C2 scope check:** `git_commit`, `git_branch`, `git_checkout` — reject any repo path that resolves inside a project memory repo; carry ref-SHA for reflog-based undo
- **Tier 3 — manual-action proposal, no side effect:** `git_push`, `gh_pr_create`, `gh_pr_merge` return `Result{Proposal: true}` plus command text for the user to execute outside Harness. The agent loop does not currently persist or approve/execute these proposals. `gh_pr_wait` is tier-1 (read-only CI poller): exponential backoff 10 s → 60 s, returns `green`/`red`/`timed_out` JSON.

### Path Identity (`internal/pathid`)
Answers one question — where a path physically is — for every component that
enforces a boundary with it. It is a security boundary in its own right: the
tool sandbox, the C2 memory-repo lock, and the git write lock all decide
containment or identity from its results, and they must not decide differently.

Responsibilities:
- `Canonical` resolves an existing path to its physical location. On Windows
  this is `GetFinalPathNameByHandle`, which resolves junctions, mount points,
  symlinks, and 8.3 short names; elsewhere `filepath.EvalSymlinks` is complete
  because symlinks are the only reparse mechanism.
- `Resolve` canonicalizes the deepest existing component and re-appends the
  components below it, so a path that does not exist yet is judged by where it
  would land rather than by its parent. A relative input is made absolute
  first, so the result is absolute on every OS.
- `Resolve` returns an opaque `ID` rather than a string. Comparison lives on
  the `ID` — `Equal`, `Contains`, `Key` — so there is no exported operation
  with an "already resolved" precondition for a caller to forget.
- `ID.Contains` is `filepath.Rel`-based, not prefix-based. A prefix test
  rejects everything below a filesystem or volume root (`C:\`, `/` already end
  in a separator), accepts a sibling sharing a textual prefix, and has no
  answer for two different volumes.
- `Same`, `SameOrWithin`, and `LockKey` are the high-level operations: repo
  identity, sandbox/C2 containment, and the physical-identity key of a path.
  The repository-wide mutation coordinator is keyed by an identity's `Key`,
  never by a pathname or a hand-composed resolution.
- Key maps and locks with `ID.Key`, never with the `ID` itself. Go compares
  every field including the display path, and `Resolve` re-appends a
  not-yet-created tail in the caller's case, so one identity can produce two
  structs.

Design constraints:
- **`filepath.EvalSymlinks` must not be used for containment or identity.** It
  leaves a Windows junction unresolved and fails outright on paths below one,
  so a check built on it accepts a path that physically reads outside its root
  and gives a junction alias a different identity from its target.
- **Every function fails closed.** Only `fs.ErrNotExist` walks upward; any other
  resolution failure is returned. A caller cannot distinguish an unresolvable
  path from a safe one, so the unknown case must be a refusal rather than a
  lexical guess.

### Rooted Filesystem Access (`internal/rootfs`)
The other half of the pair. `internal/pathid` decides *where* a path is;
`internal/rootfs` acts on that place rather than on the name that led to it.
Both are needed and neither substitutes for the other.

Validating a pathname and then reopening it checks one resolution and acts on
another. Canonicalizing the opened target and comparing it against a pinned
root path is no better: rename the real root aside, move an attacker's
directory into the name it vacated, and the target opens inside the attacker's
directory while canonicalizing to a path under the pinned string — the
comparison agrees with itself and admits the file. `os.Root` removes the
pathname from the decision by holding the directory open and resolving every
component against that handle.

Responsibilities:
- `Root` wraps an open directory for relative access (`ReadFile`, `Lstat`,
  `Stat`, `Readlink`, `Open`, `CreateExclusive`, `ReadDir`, `MkdirAll`).
  `internal/git`'s `DiffWorktree` pins the worktree with it; `internal/memory`
  pins both ends of a project-repo copy, its scaffolding, its validation, and
  its file enumeration with it.
- `OpenIdentified` pins a directory and returns it **with** the physical
  identity that directory has been confirmed to have. The pairing is the point:
  an identity resolved separately from a pin describes a name, so any later
  reasoning about the handle — is it inside that other directory, is it the same
  as this one — is reasoning about something that need not be what is held open.
  `internal/git` opens its repository boundary through `OpenIdentified` and
  retains it (`NewAnchorFromRoot`), `memory.DirReader` opens and identifies in
  the same step (`OpenIdentified` + `NewAnchorFromRoot`), and runtime
  construction compares the retained boundaries with `os.SameFile` before
  publishing a candidate, so git commits, session writes, and index
  publication are bound to the same physical repository or the candidate
  fails closed.
- `Set` is the sandbox-root list. `Set.Open` uses `OpenIdentified` on the
  configured root, then picks the owner by containment and returns a `Target`.
- `Target` carries the caller's display spelling — locators and tool output stay
  in the terms the caller asked in — while `Read`, `ReadDir`, `MkdirAllParent`,
  `WriteAtomic`, and `CreateExclusive` go through the handle.
- `WriteAtomic` (temp file + rename) and `CreateExclusive` (`O_EXCL`) are
  different operations, not variants. A rename replaces whatever holds the name,
  which is right for editing an existing file and destructive for creating a new
  one, so `edit`'s whole-file mode uses the latter and has no preceding
  existence check to race against. A failed `CreateExclusive` leaves its partial
  file: cleaning up means removing a *name*, which by then may belong to someone
  else's file.
- `OpenWrite` does not truncate. Truncation is a separate step the caller takes
  after it has compared the open handles, because O_TRUNC destroys the file
  before anyone can look at it.
- `Root.SameDir` compares two open directories as filesystem objects. It settles
  the directories only: it says nothing about the files inside them, nor about
  one being inside the other.
- `Root.OpenChild` pins a subdirectory as a `Root` of its own, so a traversal
  that inspects a directory and then descends into it uses one handle rather
  than resolving the same name twice.
- `Root.WriteStreamAtomic` publishes by rename. Replacing a directory *entry*
  leaves the inode that held the name alone, which is the only way to write into
  a tree whose entries may be hard links to files elsewhere — truncating in
  place writes *through* the link, and comparing the pair being copied cannot
  detect it, because the destination entry may link to a different source file
  than the one being read.
- `Root.AppendSync` is the deliberate in-place exception to rename publication,
  used for append-only logs like `sessions.jsonl`. It opens with
  `O_WRONLY|O_CREATE|O_APPEND` and nothing else — no truncate, no seek, no
  caller-supplied flags — appends the complete record with one write, fsyncs
  before success, and never cleans up by name after a failed append. Because
  appending necessarily writes in place, it writes *through* a hard link: a
  `sessions.jsonl` entry hard-linked to a file outside the repo gains the
  record on both names. Rooted access prevents pathname, symlink, and junction
  escapes but cannot distinguish a hard-linked entry from the same underlying
  file elsewhere. This is an inherent property of append (unlike atomic
  replacement) and is documented rather than solved; the log is not rewritten
  with a read-modify-rename because doing so would replace the append-only
  identity the log exists for.
- The repo copy layers checks rather than relying on any single one: the two
  trees must be disjoint by name, disjoint again against handle-bound identities
  once both ends are pinned, distinct as directories, and disjoint level by
  level during the walk — every newly pinned source directory against every
  pinned destination directory and vice versa, which is what catches a directory
  being moved from one tree into the other mid-copy. Files need no comparison,
  because they are published by rename.
- `ReadDir` sorts by filename. `os.Root` has no `ReadDir`, and `File.ReadDir`
  returns filesystem order where the `os.ReadDir` it replaced sorted — tool
  output has to be stable across identical calls.

Design constraints:
- **Pin before authorizing.** Resolving a root and pinning it afterwards leaves
  a window in which the resolved directory is replaced, so the open pins the
  replacement while the authorization describes the original. Dereferencing the
  configured name once, then binding the identity to that handle, is what closes
  it. A configured name that already meant the wrong directory at pin time is
  not a race and is not defended against.
- **`os.Root` is a containment boundary, not a ban on links.** It follows a
  symlink whose target stays inside the root. It refuses an absolute link
  target unconditionally, so a Windows junction is never traversed through a
  root; `Set` sidesteps that by addressing the target through the physical path
  `pathid` resolved.
- **Containment is within a directory tree, not within a filesystem.** On Linux
  `os.Root` does not stop traversal across bind mounts, ordinary mount points,
  or into `/proc`. Mount-based escapes are outside the threat model — staging
  one needs privileges that already defeat the sandbox — and closing them would
  need `openat2` with `RESOLVE_NO_XDEV`, which has no Windows counterpart.
- **It does not sandbox subprocesses.** `exec`, `go_test`, and `go_lint`
  validate their working directory with `pathid` and nothing more; command
  containment is a separate problem. `go-git` likewise takes a pathname, so
  repository opening keeps the explicit identity and C2 checks around it.
- The toolout spill directory is outside every sandbox root, so it opens its own
  `os.Root` in `internal/tools` rather than going through `Set`.

### Filesystem Threat Model

The harness's security architecture for filesystem operations is built on two
primitives: `internal/pathid` (physical path identity) and `internal/rootfs`
(rooted directory operations). Together they implement **pin-before-authorize**:
resolve the configured directory once, bind the open handle to its physical
identity, and perform all subsequent operations through that handle.

The threat model describes the target state. Packages not yet migrated
(`internal/retrieval`, `internal/governor`) currently operate by pathname and
are tracked as migration entries in `cmd/fsaudit/allowlist.json`. Once migrated
they will inherit the guarantees below.

#### Defended threats

| Threat | Mechanism | Status |
|--------|-----------|--------|
| Symlink escape from a sandbox root | `os.Root` resolve-by-handle; absolute-target links refused unconditionally | implemented |
| Windows junction escape | `pathid` resolves junctions before the root sees the name; `Set.Open` addresses the target through the physical path | implemented |
| Case / 8.3 alias (Windows) | `pathid.Canonical` resolves to a single physical name; containment checked against the canonical form | implemented |
| Same-name directory replacement | `OpenIdentified` verifies the pinned handle against the physical identity with `os.SameFile`; a replacement fails the comparison | implemented |
| Rename of original directory | Operations through the pinned handle continue to address the original directory; `OpenIdentified` fails closed on the renamed name | implemented |
| Hard-link leaf writes | `WriteStreamAtomic` publishes by rename — a rename replaces the directory entry and leaves the linked inode alone. Truncating in place would write through the link into a file elsewhere. The one deliberate exception is `AppendSync` for append-only logs: appending necessarily writes in place, so a hard-linked `sessions.jsonl` entry is written through. That limitation is inherent to append and documented, not solved by rewriting the log (PR 7) | implemented |
| In-process concurrent writers | One repository-wide mutation coordinator per physical repository identity (`internal/coord`), shared by git mutations, index publication, and project-repo scaffolding and moves; index publication and the following git commit run inside one repository transaction held across both (PR 5b4), and project-repo scaffold writes and their commit, and a project-repo move's copy and its commit, each run inside one transaction held across both (PR 6) | implemented |
| Check/use races on intermediate directories | `OpenChildNoFollow` opens the child, Lstats the entry through the parent, rejects links, and compares the entry with the opened handle via `os.SameFile` — what is opened and what is checked are the same object | implemented |
| Memory repo reads/writes through pathname | Read and write operations use pinned `os.Root` handles (PR 4, PR 5a); index uses vector-first copy-on-write publication (PR 5b1); DirReader identity via PR 2c, compared against the git repository's retained identity at runtime construction (PR 5b4); index rooted identity via PR 5b2; repository-wide mutation coordinator spanning git commits and index publication via PR 5b4; project-repo scaffolding, validation, destination creation, and file enumeration route through pinned roots, scaffold/move writes are bound to the retained git boundary with `os.SameFile` and run inside the repository transaction (PR 6); the session log is read and appended through the same generation-owned pinned reader, with only `fs.ErrNotExist` meaning "no sessions" and a rooted append primitive that cannot truncate (PR 7) | implemented |

#### Out of scope

- **Privileged mount manipulation.** Staging a bind mount, mount point, or
  `/proc` entry inside a tree requires privileges that already defeat the
  sandbox. `openat2` with `RESOLVE_NO_XDEV` would close this but has no Windows
  counterpart.
- **Subprocess sandboxing.** `exec`, `go_test`, and `go_lint` validate their
  working directory with `pathid` and hand a pathname to the child process.
  Command containment is a separate problem.
- **go-git pathname boundary.** go-git resolves its storage by pathname, not by
  handle. The harness keeps explicit identity and C2 checks around the go-git
  boundary: the wrapper pins and verifies the repository boundary at open,
  retains that verified identity for coordinator selection, and refuses to open
  through a repointed spelling. A spelling repointed *after* the open is a
  documented go-git limitation the wrapper does not claim to close, and go-git
  itself will not open through a directory link.

#### Acknowledged residual window

Within `WriteStreamAtomic`, the temporary file is created inside a
destination parent directory pinned with `OpenChild`.  After writing, the
data is fsynced and the temp file's identity is captured from the live
handle with `f.Stat()` and compared against the named entry through the
pinned parent via `os.SameFile`.  A substituted entry is refused.  The
remaining window is between that identity comparison and the rename: an
external process can substitute the entry in that interval.  Closing it
requires a compare-and-rename primitive that operates on a handle rather
than a name; no such primitive exists in the portable Go standard library.
Documenting the window rather than claiming it closed is deliberate.

#### Audit enforcement

Production calls to the symbols in `cmd/fsaudit`'s compiled `watched` policy
(approximately 35 symbols across `os` and `path/filepath`, including
`MkdirTemp`, `Lchown`, `Chdir`, `CopyFS`, and `DirFS`) are inventoried in
`cmd/fsaudit/allowlist.json`. Each call is classified as:
- **migration** — will be routed through `rootfs` in a future PR; or
- **permanent** — an intentional boundary exception with a justification.

The `cmd/fsaudit` tool verifies on every CI run that no new direct filesystem
call appears without a matching entry. The audit scans all production `.go`
files including `internal/rootfs` and `internal/pathid`. Only the audit tool
itself (`cmd/fsaudit/`) is exempt. The watched-function policy is compiled into
the scanner — it is not configurable from the allowlist.

The scanner also blocks capability escapes that cannot be inventoried:
dot imports of watched packages, extracting watched functions as values, and
`os.Root` type references outside `internal/rootfs`. Within rootfs, every
`os.Root` reference is blocked except the single private `Root.root` backing
field.

### Parser Front-Ends (`internal/parser`)
Hosts the language front-ends behind the `ast_*` tools and the governor's skeletonizer (M10).

Responsibilities:
- Define the `FrontEnd` contract: language name, claimed extensions, deterministic `Outline`, syntax `Check`, and a `SelfTest` that must pass at registry construction — a front-end with no working parser fails startup.
- Resolve files to front-ends by extension; `ast_*` tools generate their supported-language declarations from this registry, never by hand.
- Go front-end uses `go/parser` in single-file mode (M10.1 tier); cross-package type resolution via `go/packages` is a later tier.

### Approvals (`internal/approvals`)
Owns the M7 permission evaluator used by the agent loop.

Responsibilities:
- Evaluate layered rules in order: builtin defaults, user config, then session approvals.
- Support allow, reject, and always-allow session decisions.
- Classify destructive shell commands so broad rules cannot silently allow them; exact session approvals are required to bypass Ask for a destructive command.
- Return audit-friendly decision sources for UI/session history.

### Governor (`internal/governor`)
Applies result transforms between tool execution and context injection in the agent loop. Transforms are not model-callable and are invisible to the agent as tools.

Transforms applied in order (B1 → B2 → B3). B5 gates B1 and B2; B3 is exempt:
- **B1 — query-aware skeletonizer:** reduces read output for parser-supported files, keeping full bodies for spans relevant to the active query and emitting only signatures for the rest.
- **B2 — tool-output folder:** per-tool content cap with head/tail elision for high-volume toolchain tools (`exec`, `go_test`, `go_lint`, `git_diff`, `git_log`).
- **B3 — tee-on-failure:** writes the full unfiltered output of a failed call to `~/.harness/cache/toolout/` and injects a compact handle into the conversation so the model can reference it without bloating context. Tools hand over the complete output in `Result.FullOutput` when their inline text is a bounded excerpt of it; the inline caps alone would leave B3 preserving the same truncated text the model already saw.
- **B5 — token gate:** auto-reverts B1 or B2 when it increases the estimated token count (rune-quarter heuristic, swappable via `WithTokenizer`). **B3 is deliberately outside the gate.** B1 and B2 are pure — they rewrite the result and nothing else, so discarding the rewrite genuinely undoes them. B3 is side-effectful: the spill file is already written when the gate inspects its return value, so discarding that value does not undo the transform, it only drops the locator and leaves the file orphaned. B3 does rewrite `Error` into a prefix plus handle, and that can grow the text, because the handle is often longer than the short inline failure it accompanies once a small failure can carry a large preserved output. Retaining the bounded locator is the requirement — it is the only route back to output that is otherwise unreachable.

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

**Project repo workflow:** scaffolding, layout validation, `.gitkeep` creation,
destination creation, and file enumeration route through pinned `os.Root`
handles, and layout entries are addressed by validated repo-relative paths
through those handles. Scaffolding and moving an existing repository hold the
repository-wide mutation coordinator, and every write through an independently
opened handle is first bound to the retained git boundary with `os.SameFile`
(`Repo.SameRoot`), so a name re-pointed between the git open and the handle
open fails closed instead of writing one repository under another's
coordinator. `EnsureProjectRepo` scaffolds and commits inside one git
transaction. `MoveProjectRepo` creates and pins the destination, pins the
source, refuses overlap by handle-bound identity, and only then initializes
git, so a destination re-pointed into the source is refused before git metadata
can be written inside it; the copy, scaffolding, enumeration, and migration
commit then run inside one destination transaction using the transaction
session's commit path. The project-repo copy pins both trees and descends
through pinned child handles (see Rooted Filesystem Access).

**Planned M12 semantic-write gate:** after M11 and MR0 closure, session summaries,
promoted facts, notes, and `memory_propose` use a project-local append-only event log and
immutable proposal payloads. Session logs, conversation sidecars, and vector/FTS indexes
remain outside the semantic gate as evidence, operational state, or derived projections.

### Project Store (`internal/project`)
Defines project identity and validation rules. SQL persistence lives in `internal/db`, while this package owns typed project values, slugs, directory metadata, effective model overrides, and lifecycle status such as hidden or system projects.

### Git Backend (`internal/git`)
Thin wrapper around `go-git` (pure Go — no git binary dependency).

Operations:
- `Init(path string)` — init or open one memory repo or attached code repo
- `Commit(msg string, files []string)` — stage specific files + commit in the selected repo
- `WithMutation(fn)` — run index publication and the following git commit as one
  repository-wide mutation transaction

Every `Repo` handle retains the pinned repository boundary (`rootfs.NewAnchorFromRoot`)
and compares it against other components' opened boundaries with `os.SameFile`
(`SameAnchor`), so a directory replaced at the same pathname between two opens
is detected rather than accepted on the strength of a shared canonical path.
That retained boundary selects the repository-wide mutation coordinator
(`internal/coord`), the same gate index publication on the repository
acquires, so git mutations and index publication serialize on one object and
an alias spelling cannot split one repository across two coordinators.

`WithMutation` holds the coordinator across both the index publication and the
git commit; the `Mutation` session's commit methods do not reacquire it.
Memoryops `AfterSaveEmbed` and the episode-index rebuild publish and commit
inside one such transaction. Lock order is fixed as repository gate, then the
per-handle mutex, in both the standalone and in-transaction paths.

go-git resolves its storage by pathname, not by handle. The wrapper pins and
verifies its repository boundary at open and retains that boundary for
identity comparison, but it does not claim go-git itself is handle-relative: a
pathname repointed after the open is a documented go-git limitation, and
go-git refuses to open through a directory link at all (go-billy reports "path
escapes from parent"), so git handles are only ever opened through non-link
spellings.

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

### Retrieval (`internal/retrieval`)
Owns the blended semantic + recency scoring pipeline and the D3 trace layer.

- `ScoreEpisodePaths` takes a query and episode paths, calls the embedder, blends cosine similarity with exponential recency, and currently returns `(map[path]score, scored, error)`.
- `RetrievalTrace` and `NDJSONSink` exist, and startup installs `DefaultTraceSink` when construction succeeds. Production calls normally append rows without buffering, but M10.3/MR0 is not accepted: constructor failures are silently ignored, emission errors are discarded, and shutdown never closes the sink.
- The current candidate row lacks project identity, weights, selected/top-K state, and final score rank; its `Rank` field is path-order position. Empty/unscoreable/error calls emit nothing.
- `QueryID` is currently a SHA-256[:8] prefix. MR0 replaces it with the canonical full hash and adds project-scoped call/candidate records; prompt assembly and `memory_query` pass trace context with project slug and requested top-K.
- `NDJSONSink` already implements date-bucketed files and 30-day pruning. MR0 preserves its startup wiring, surfaces construction/emission failures, and closes it during shutdown.
- `EpisodeID` derives a stable, path-relative identifier for indexing and scoring across different repo roots.

`cmd/eval-retrieval` currently reads one NDJSON `query`/`relevant` file and reports MRR
and Recall@K for the configured blend. MR0 aligns it with the runtime schema, adds
semantic-only/recency-only/configured-blend Precision@3 and Recall@3, enforces ten real
labels in baseline mode, and writes a machine-readable baseline before M12 begins.

### Memory Operations (`internal/memoryops`)
Semantic-memory operations that sit on top of the memory repo, embedder, and episode index. The runtime wires these into session saving and the UI; the domain logic lives here.

- `EpisodeScorer` wraps an embedder client, episode index, and `config.PromptConfig` (carries `SemanticWeight`, `RecencyWeight`) to expose a `ScoreEpisodes` method used by the prompt assembler and the `memory_query` tool adapter.
- `AfterSaveEmbed` returns a `session.AfterSaveFunc` that embeds the saved episode body into chunks, calls the embedder, and upserts vectors into the project's `_episodes` index.
- `EpisodeIndex` wraps `internal/index` with episode-specific content-hash–based idempotency: re-embedding an unchanged episode is a no-op.
- Dedup and fact-promotion helpers used by the prompt assembler and memory UI.

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
    retrieval/                 ← D3 NDJSON trace files (date-bucketed, 30-day retention)
  cache/
    toolout/                   ← B3 tee-on-failure spill directory
  eval/                        ← developer eval data; retrieval labeled query sets (M10.3/MR0)
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
- **loop:** `max_turns`, `doom_threshold`, `read_enabled`, `file_list_enabled`, `ast_map_enabled`, `ast_find_enabled`, `git_status_enabled`, `git_diff_enabled`, `git_log_enabled`, `edit_enabled`, `exec_enabled`, `go_test_enabled`, `go_lint_enabled`, `git_commit_enabled`, `git_branch_enabled`, `git_checkout_enabled`, `web_search_enabled`, `memory_query_enabled`, `git_push_enabled`, `gh_pr_create_enabled`, `gh_pr_merge_enabled`, `gh_pr_wait_enabled`

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

**Native agent layer staged after Projects.** M4 introduced `internal/agentloop` and `internal/tools` as first-party components. `internal/agent` remains the registry/persona package. The MVP started read-only and project-scoped; M7 added approvals and M10 shipped the parser-backed editing, execution, git, retrieval, and governor surface.

**Approval-gated tools.** M10 replaced `file_write`/`shell_exec` with `edit`/`exec`; both remain disabled by default and route through the approvals evaluator when enabled. Every `exec` command requires explicit per-call approval, while matching deny rules still deny. `go_test`, `go_lint`, and local git writes are also opt-in/gated. Web search is opt-in and disclosed. External VC tools only generate manual-action proposals; they do not perform pushes or PR mutations.

**Pipeline DSL staged with project memory repos and tool permissions.** `.hp` pipelines depend on project memory repos, attached source repos, the native agent loop, and the hardened tool/approval layer because model steps write declared outputs through harness tools and verify/gate commands run as trusted local processes. The DSL is deliberately not part of M4: interactive agent-loop execution comes first, safe write/shell permissions come second, storage layout stabilization comes third, declarative multi-step automation comes after those foundations.
