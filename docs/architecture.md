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
  identity, sandbox/C2 containment, and the git mutation-lock key. `LockKey`
  exists so no caller composes resolution and key derivation by hand.
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
- `Root` wraps an open directory for relative access (`ReadFile`, `Stat`,
  `Lstat`, `Readlink`, `OpenRead`, `ReadDir`, `MkdirAll`, `Remove`,
  `RemoveAll`, `CreateExclusive`, `AppendSync`, `OpenAppend`,
  `WriteStreamAtomic`, `OpenChild`, `Clone`, `Walk`, `IsNotDirectory`).
  `internal/git`'s `DiffWorktree` pins the worktree with it; `internal/memory`
  pins a project repo for the lifetime of its reader and both ends of a repo
  copy with it. There is no raw `Open` returning an `*os.File`: the only read
  handle is `OpenRead`, returning a `File` — see below for why.
- `Root.Walk` pins each subdirectory before describing or entering it, so what
  is reported to the visitor and what is recursed into are the same handle
  rather than two separate resolutions of one name. A directory-listing entry's
  *type* decides whether to attempt that pin at all: a symbolic link present at
  listing time is a distinct type from a directory even when its target is one,
  so it is Lstat'd and reported without an `OpenChild` attempt ever being made.
  That listing is a snapshot, though, and `os.Root` follows an in-root symlink
  rather than refusing it — so a name that was a real directory when listed and
  becomes a symlink before the pin runs would be opened by `OpenChild` if entry
  type at listing time were the only check. It is not the only check: `walkDir`
  Lstat's the same name through the parent immediately after pinning it, which
  does not follow links, and refuses to describe or enter the pinned child
  unless that Lstat still shows a directory. That alone is not enough to trust
  that the pinned child and the Lstat result describe the same object, though:
  `OpenChild` and the Lstat are two separate resolutions of the name, so a
  symlink present when `OpenChild` follows it — or, with no symlink involved at
  all, an ordinary directory present when `OpenChild` opens it — can be
  replaced again by an ordinary, non-symlink directory or file before the Lstat
  runs. The replacement passes the "is it a symlink" check exactly as easily as
  the original would, since neither is a link. `walkDir` additionally requires
  `os.SameFile` between the Lstat result and the pinned child's own `Stat(".")`
  before visiting or descending — comparing the two as filesystem objects,
  which a same-name replacement cannot pass regardless of its type. Walk is
  also depth-bounded: `os.Root` follows an in-root symlink, so a link pointing
  at one of its own ancestors is a cycle that would otherwise not terminate.
- `Root.AppendSync` / `Root.OpenAppend` are the append-only capability the
  session log and the retrieval trace use. There is deliberately no general
  `OpenFile`: exposing the flag set would put `O_TRUNC` one argument away from
  an operation whose whole guarantee is that previous records survive.
- `Root.WriteStreamAtomic` pins `rel`'s destination directory once, before the
  temporary file is created, and the create, the failure-path remove, and the
  final rename all resolve against that one handle rather than the directory
  component a second and third time. Re-resolving the *directory* on each of
  those calls would let a directory swapped in between them redirect the
  rename to a different directory entirely; pinning it once closes that. It
  does not, on its own, close the narrower window one level down: the temp
  file's name becomes visible to anything else with access to the directory
  the moment it is created, and a `src` reader that takes any real time (a
  slow disk, an unlucky scheduling gap) gives a concurrent writer a chance to
  remove that visible name and recreate it holding different content —
  independent of the directory-level pin, since the replacement is a new file
  under an old name in the same, correctly pinned directory. `WriteStreamAtomic`
  captures the temp file's own `FileInfo` immediately after creating it, and
  before both the failure-path remove and the final rename confirms via
  `os.SameFile` that the name still refers to that same file, refusing to
  remove or publish anything it cannot confirm is still its own. No portable
  "rename by handle" primitive exists in Go's stdlib to close this atomically —
  Windows' `SetFileInformationByHandle` could, but is not exposed — so a
  narrow stat-to-syscall window remains; this shrinks it from the entire copy,
  sync, and close down to that. What remains needs a second writer with
  independent access to the same directory, actively racing this exact
  sequence — a genuinely external process, not this codebase's own
  concurrency, since every current caller onto a shared destination is
  itself serialized: `memory.LockRepoWrite`'s `repoWriteLocks` for
  facts.md/notes.md/agent files, `index.Index`'s own `mu` for its
  `vectors.bin`/`manifest.json` pair, and `project.Workflow`'s `workflowMu`
  for project creation and repo moves. That is the same boundary
  `repoMutationLocks` below already draws for its own lock.
- `File`, returned only by `OpenRead`, is read-only and exposes no pathname
  accessor: `os.File.Name` on a file opened through an `os.Root` reports the
  root path joined with the relative name, an authorized read that could be
  turned back into an unauthorized direct reopen. There is no read-write
  variant, and no in-place mutation capability (no `Append`, no `Truncate`) —
  see the vector index below for why in-place mutation through a rooted handle
  is exactly the operation this package avoids, not merely discourages.
- `Root.Clone` hands out a second independent handle on the same directory, for
  a component that needs a root for its own lifetime without inheriting somebody
  else's `Close`.
- `OpenIdentified` pins a directory and returns it **with** the physical
  identity that directory has been confirmed to have. The pairing is the point:
  an identity resolved separately from a pin describes a name, so any later
  reasoning about the handle — is it inside that other directory, is it the same
  as this one — is reasoning about something that need not be what is held open.
- `Set` is the sandbox-root list. `Set.Open` uses `OpenIdentified` on the
  configured root, then picks the owner by containment and returns a `Target`.
  The caller-supplied path is resolved to a target identity once per candidate
  root, immediately alongside that root's own pin, rather than once before the
  loop starts: a target resolved up front and compared against roots pinned
  afterward is authorized against an identity that predates every root's own
  pin, which is a wider gap than resolving it fresh next to whichever pin it is
  actually being checked against.
- `memory.ValidateProjectRepo` and `memory.OpenValidatedDirReader` share one
  pin-and-check sequence; the latter hands back the reader still holding the
  same pin instead of closing it and making a caller reopen the root by name a
  second time to get a long-lived handle. Validating and then reopening
  separately is the exact pattern this package's design rule prohibits, and
  `internal/runtime`'s startup path used to do it before both project repos
  were folded into one `OpenValidatedDirReader` call per repo.
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
- The toolout spill directory is outside every sandbox root, so both the
  governor's producer and the `read` tool's consumer take a standalone
  `rootfs.Root` on it rather than going through `Set`.

### Filesystem Access Rule

One rule covers the whole repository, and it has two halves:

- **Any decision about physical path equality, containment, deduplication, or
  locking uses `internal/pathid`.**
- **Any operation meant to stay inside a configured directory tree executes
  through a pinned `rootfs.Root` or `rootfs.Target`.** A pathname is never
  validated or canonicalized and then reopened for the operation; identity, when
  it matters, is bound to the opened directory with `OpenIdentified`; a
  multi-step operation holds one handle from end to end; and a traversal
  descends through pinned child handles rather than resolving a child's name a
  second time.

Concretely: project memory repos are pinned by `memory.OpenDirReader` for the
lifetime of the service graph that uses them, and every read, write, listing,
glob, walk, and removal below them resolves through that handle. The session log
appends through the same handle. The vector index is located by a repo-relative
name resolved through the repo's handle — never by an absolute
`<repo>/index/_episodes` pathname, which can already lead out of the repo. The
B3 spill directory is pinned for each write and each read.

**Production `os.OpenRoot` exists only in `internal/rootfs`.**

#### Direct-path exception ledger

Every remaining production filesystem call addressed by pathname is listed here
and documented at its call site. Nothing else is permitted without adding it to
this table.

| Location | Call | Why `pathid`/`rootfs` does not apply |
| --- | --- | --- |
| `internal/home/home.go` `Ensure` | `os.MkdirAll` | Bootstrap: creates the harness home the other roots are opened on. Only creates directories; `MkdirAll` accepts an existing directory and refuses anything else. |
| `internal/governor/governor.go` `tooloutDir` | `os.MkdirAll` | Bootstrap of the spill root. Everything below it is addressed through a pinned handle by `writeSpill`. |
| `internal/retrieval/trace.go` `NewNDJSONSink` | `os.MkdirAll` | Bootstrap of the trace root, pinned on the next line; the daily file, the listing, and the retention deletes all go through that handle. |
| `internal/memory/project_repo.go` `copyTreeWithoutGitHooked` | `os.MkdirAll` | Bootstrap of the copy destination, pinned immediately after and then checked for identity and containment before anything is written. |
| `internal/git/git.go` `Init`, `newRepo` | `os.MkdirAll`, `os.Stat` | Bootstrap of a repository root, immediately followed by `go-git`, which addresses its storage by pathname and cannot take a handle. `newRepo` resolves the repository's physical identity via `pathid` immediately after `go-git` opens it — as close as achievable to what `go-git` itself used, since it exposes no way to ask what it actually opened — and retains it as `Repo.Identity()`. It also captures the directory's `os.FileInfo` via `os.Stat` at the same moment, retained as `Repo.DirInfo()`. `internal/runtime`'s session-manager wiring compares `DirInfo()` against `memory.DirReader.DirInfo()` (queried live through the reader's own still-open handle) with `os.SameFile` — not `Identity()` against `Identity()`. `pathid.ID` reduces a path to its canonical string form and never opens or inspects anything, so it cannot tell two physical directories apart when one has been renamed aside and a different, equally valid directory installed under the exact same configured path between the two opens: both IDs would resolve to the identical string and `Equal` would return true even though the pinned reader and the freshly-opened `Repo` reached different objects. `os.SameFile` on each side's own `FileInfo`, captured as close as possible to its own open time, compares the directory objects themselves rather than the path strings each side happened to resolve to. |
| `internal/db/db.go` `PeekUIPort` | `os.Stat` | The SQLite driver takes a DSN string, so the path is a pathname either way. The stat only decides whether to attempt the open; a wrong answer costs the fallback UI port. |
| `internal/runtime/setup.go` `validateFilePath` | `os.Stat` | User-chosen model and binary paths anywhere on the machine, with no configured tree to contain them, each handed to `os/exec` afterwards. Produces a checklist message; no read, write, or authorization follows. |
| `internal/runtime/project_health.go` `CheckProjectDirectories` | `os.Stat` | An attached project directory is a root in its own right. Produces a UI warning only; the tools that operate inside those directories run their own `pathid` containment check per call. |
| `internal/config/detect.go` `Detect` | `os.Stat`, `os.ReadDir` | Best-effort first-run discovery over locations the harness does not own. Not an authorization boundary: it offers *suggestions* that are validated on save and resolved again at spawn time. |
| `internal/tray/tray_linux.go` `AcquireSingleInstance` | `os.OpenFile` | The lock lives on the descriptor: `flock` is taken on the fd this call opens, so whatever the name resolved to is what is locked. No configured tree, no second resolution. |
| `cmd/eval-retrieval/main.go` `run` | `os.Open` | The labeled query set is a file the operator names on the command line, outside every harness-managed tree. |
| `internal/pathid/canonical_*.go` | `os.Open`, `filepath.EvalSymlinks` | These *are* the physical-identity primitives. Resolving a pathname is their purpose. |

Two limits are inherent rather than exceptions, and are stated where they
matter. `go-git` resolves its own storage by pathname, so `Repo.CurrentBranch`
is one resolution performed by the component that owns the repository rather
than a handle guarantee. And a hard link inside a tree is another name for a
file elsewhere, not a link a root can refuse — writes are protected by
publishing through a rename, which replaces the directory entry, but an append
to a hard-linked file necessarily reaches the file it names.

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
- `PromotionService`'s whole read-modify-write-commit-and-possible-rollback
  sequence is one critical section per repository, serialized by
  `promotionLocks` (a package-level lock table keyed by physical repo identity,
  the same role `internal/git`'s `repoMutationLocks` plays for git mutations).
  A `PromotionService` value is constructed fresh per request, so a lock on the
  value itself would not serialize anything; the lock has to live outside any
  one request's value to serialize across them. This is what actually prevents
  two concurrent promotions to the same path from losing an update, rather than
  merely detecting the collision afterward.
- On top of that, if the commit still fails, `rollback` reads the target file
  back and only restores or removes it when the content still matches what this
  call itself published. The lock already rules out another *promotion* landing
  mid-sequence; this check is for a *different* writer to the same file that
  does not participate in the lock — the UI's direct memory-file editor,
  notably, which writes through the same store without going through
  `PromotionService`. Rolling back by name alone, on the strength of what this
  call remembers writing, would erase such a writer's content instead of this
  call's own — except that a content check alone only detects a collision after
  it has already happened, it cannot prevent one landing in the narrow window
  between the check and the restore. `memory.LockRepoWrite` closes that: it is
  the same per-repository lock `appendAndCommit`'s own read-modify-write-commit-
  rollback sequence takes (keyed by physical identity, in package-level state,
  since a `PromotionService` value is constructed fresh per call and a lock on
  it would serialize nothing), exported so a caller outside the package can take
  it around its own write to the same repo. `handleMemorySave` and
  `handleAgentsEdit` (persona, rules, and notes) hold it for their whole write,
  so a promotion's rollback and a direct editor's save are mutually exclusive
  for one repository rather than merely racing to a content check.
- The UI handler reads the memory store and the committer (and the dedup checker
  and its threshold) from one snapshot of the published service deps, not from
  separate reads, so a config reload landing mid-request cannot make the write
  and the commit act on two different generations of the memory service graph.
- Every handler that reads `MemoryStore`, `Committer`, `AgentRegistry`, or
  `SessionStore` for a quick request/response is wrapped with
  `Server.trackGenRequest` — `/memory`, `/memory/edit`, `/memory/save`,
  `/memory/episodes`, `/memory/episodes/view`, `/memory/scaffold`,
  `/memory/rebuild-index`, `/memory/promote`, `/memory/note`, `/agents`,
  `/agents/active`, `/agents/create`, `/agents/delete`, `/agents/persona`,
  `/agents/rules`, `/agents/notes`, `/chat/save`, and `/chat/session`. A
  reload's `quiesceMemoryAndAPI` step drains them (`Server.DrainGenerationRequests`,
  alongside the existing task-loop cancellation and session flush) before the
  memory/API generation they were using is closed. The SSE endpoints
  (`/events`, `/chat/events`, `/task/events`) are long-lived by design and stay
  outside this entirely — a drain that waited for one to end on its own would
  turn a reload into a hang.
- `/chat/send` and `/task/send` are a different shape from every other handler
  here: each returns promptly but hands off to a goroutine
  (`streamChatTokens`, `streamTaskEvents`) that goes on reading a
  generation-scoped dependency (`ChatRunner`, `TaskRunner`) for as long as the
  completion or task runs, well past the point the HTTP handler itself
  returns. Wrapping them with `trackGenRequest` would not help — that helper
  releases its lease when the *handler* returns, not when a goroutine it
  launched finishes — so both take their lease directly
  (`s.genGate.enter()`, refusing admission with 503 exactly like a wrapped
  handler when the gate is closed) and hand it off explicitly: a `handedOff`
  flag decides whether the deferred release fires when the synchronous
  handler returns (every early-return path) or is skipped because the
  spawned goroutine's own deferred `s.genGate.exit()` now owns it. Both call
  `enter()` *before* reading the runner (or anything else generation-scoped),
  never after: reading it first and entering second would let a reload
  complete in between, so `enter()` succeeds against the new, reopened
  generation while the already-captured runner still points at the old
  one — a lease that protects one generation guarding a dependency read from
  a different one. `handleChat` (the plain `/chat` GET) has no such
  asymmetry — it reads `ChatRunner`, `AgentRegistry`, and `SessionStore`
  synchronously with no detached goroutine — so it is simply wrapped with
  `trackGenRequest` like the rest of the route table.
- The admission this drain waits on is not a bare `sync.WaitGroup`: that type's
  own contract requires a positive `Add` arriving while the counter reads zero
  to happen before the matching `Wait`, which nothing could promise against a
  fresh handler goroutine — a request could `Add` after `Wait` had already
  observed zero and returned, landing inside a window the runtime already
  treated as fully drained. `Server`'s generation gate is a mutex-protected
  counter with a closed flag instead: admission only ever increments while the
  mutex shows the gate open, and the gate closes *before* draining begins, so
  every admission for one drain cycle has already happened by the time the
  drain starts waiting. A request arriving while the gate is closed is refused
  outright (503) rather than let through untracked. `DrainGenerationRequests`
  leaves the gate closed on success — deliberately, since the actual rebuild
  that follows (opening a git repo, starting an embedder) can be slow and none
  of that should run with new admissions still blocked on a mutex — until the
  caller calls `Server.ResumeGenerationAdmission` once the swap, or the
  restore after a failed one, is complete. On a timeout, the gate reopens
  itself before the error returns, and the reload path aborts the whole
  rebuild rather than proceed to close the current generation's handles out
  from under a request the drain could not wait for.
- `taskRunnerAdapter.CancelAll` only cancels and waits for task engines already
  registered in its own snapshot; a `/task/send` that reaches
  `registerEngine` moments later is invisible to it, and its error is only
  logged, never treated as a reason to abort. That is safe rather than merely
  tolerated, because `/task/send` takes its generation lease (above) before
  registering anything and holds it for the task's whole run: the drain that
  follows `CancelAll` — not `CancelAll` itself — is what actually has to wait
  for that task, and does, regardless of whether `CancelAll`'s snapshot ever
  saw it. A task that slips past `CancelAll` this way keeps running past the
  cancellation request, uncancelled, but never past the reload's own drain:
  it still holds that up, so the runtime still will not tear down the
  generation it is reading from until it finishes.
- The API server gets a separate quiesce step, `shutdownAPIServerForReload`,
  run only after the UI drain above has already succeeded — never folded into
  it — because its own side effect cannot be undone the way a UI drain
  timeout can: calling `http.Server.Shutdown` closes the listener immediately,
  whether or not it returns before its context ends, so a caller that decides
  not to proceed cannot go back to serving on that instance regardless. The
  reference in `rt.apiServer` is *not* cleared just because this call gives up
  waiting, though: it stays pointed at the same instance until `Shutdown`
  actually completes successfully. On a timeout, clearing it would let a
  later reload attempt read `rt.apiServer` back as `nil`, skip this step
  entirely (there being nothing left to shut down, from its point of view),
  and let `stopMemoryAndAPI`/`closeReplaced()` close the roots an in-flight
  request on that same, still-running server is reading from. Leaving the
  reference in place means a second attempt calls `Shutdown` again on the
  *same* server — safe and idempotent by `http.Server`'s own contract — which
  extends the wait for whatever is still outstanding instead of silently
  treating it as already gone. The method's error is treated the same as a UI
  drain failure: abort the rebuild, reopen the UI gate, leave `rt.cfg` and the
  rest of the service graph untouched. The one thing that abort cannot
  preserve, unlike every other piece of the current generation, is API
  availability — the listener is down and stays down until `Shutdown` finally
  succeeds (on this attempt or a later one).
- Reconfiguring llama-server and the embedder, and swapping the inference
  client's target port, happens only *after* a rebuild's quiesce (drain plus
  API shutdown) has succeeded — never before it — when a memory/API rebuild
  is also needed. Doing it earlier, unconditionally, created two separate
  problems: an in-flight task or a session-flush summarization still using
  the old model would have its target change out from under it mid-request,
  and an aborted rebuild (a failed drain or API shutdown) would leave the old
  memory/API generation paired with processes that had already moved to the
  new config — coherent for neither generation. The reconfigure itself reads
  the already-computed `newModel`/`loaded` local values, never `rt.cfg`: at
  the point a model-port change needs a fresh inference client,
  `rt.newInferenceClientForModel(newModel)` is used instead of
  `rt.newInferenceClient()` (which reads `rt.cfg` internally), specifically
  because `rt.cfg` is not yet committed to `loaded` at that point — reading it
  back out would install a client still pointed at the old port. When no
  rebuild is needed at all, the reconfigure runs immediately, same as before;
  there is no generation to protect in that case.
- `needsRebuild` (the condition that decides whether to quiesce at all) checks
  `oldModel != newModel` and `old.Embedder != loaded.Embedder` directly, not
  just `modelEndpointChanged`/`embedderEndpointChanged` (port-only): the
  process reconfigure above reacts to *any* field in either changing, so
  gating quiescing on port alone let a same-port model-path, context-size, or
  embedder change reach the no-rebuild path, where the reconfigure ran
  immediately with nothing drained first. `modelEndpointChanged` on its own
  is still used inside the reconfigure to decide whether the inference client
  needs rebuilding, since Model.Port is never project-specific (the harness
  runs exactly one llama-server, whose port comes from the global config —
  see `config.ModelConfigEqual`'s own comment) and is therefore unaffected by
  a project switch.
- The generic llama-server reconfigure skips itself entirely when the active
  project is also changing in the same apply (`projectSwitching`), deferring
  completely to `handleProjectSwitch`'s own, separately-run decision — which
  compares effective models between the source and destination projects and
  honors `llama_on_switch=keep`. Since `oldModel != newModel` is almost always
  true across a switch to a project with a different effective model, running
  the generic reconfigure first would move the process before that decision
  ever ran, defeating "keep" in exactly the common case a switch is meant to
  exercise.
- If the rebuild that follows a successful quiesce is attempted and then
  fails, `restoreMemoryAndAPI` puts the previous memory/git generation back —
  but the process reconfigure (and possibly `handleProjectSwitch`'s own
  reload) already ran ahead of that attempt, moving llama-server/the embedder/
  the inference client to the new config. `undoProcessReconfigure` reverses
  exactly the components this apply actually moved forward (tracked via
  `llamaMoved`/`embedderMoved`/`clientSwapped`, the last two flags also set by
  `handleProjectSwitch`'s own return value), reconfiguring back to
  `oldModel`/`old.Embedder`. Tracking which components actually moved, rather
  than unconditionally reconfiguring back, matters because
  `proc.Manager.Reconfigure` always restarts the process — calling it on a
  component that never moved (e.g. under `llama_on_switch=keep`) would cause
  a needless, disruptive restart of an already-correct process.
- `ApplyConfig` holds a dedicated `Runtime.applyMu` for its entire call,
  distinct from and outside `rt.mu`. `rt.mu` alone cannot serialize two
  concurrent `ApplyConfig` calls (a `/config` save racing `/retry`, say)
  because `ApplyConfig` itself releases it during the UI drain and the API
  shutdown; without `applyMu`, the first call's rebuild could complete and
  reopen admission while the second — still holding stale local copies of
  `old`/`loaded`/`oldModel`/`newModel` from before the first's rebuild —
  proceeded to quiesce and replace the generation the first just installed,
  without ever draining the requests admitted against it. `applyMu` makes
  the whole sequence one transaction that a second caller waits out entirely.
- Sessions are flushed only after *both* the UI drain and the API shutdown
  have succeeded — `flushSessionsForReload`, called from the same success
  branch as the process reconfigure, right before it (so a last-minute
  summarization still runs against the model the conversation was actually
  held with). This used to run inside `quiesceMemoryAndAPI` itself, *before*
  the UI drain — while the UI gate (and the API server) were still admitting
  new requests. A chat/task admitted in that window could append to the
  session manager's live session map only after `FlushAll`'s snapshot of what
  to save had already been taken; the later drain correctly waited for that
  request's own goroutine to finish, but waiting for it to finish is not the
  same as flushing what it wrote, and nothing flushed it again before
  `stopMemoryAndAPI`/`closeReplaced()` closed the manager's underlying roots.
  Moving the flush to after both drains means no new session-modifying
  request can be admitted by the time it runs, and every one that was already
  admitted has already finished.
- `handleChatSend`/`handleTaskSend` capture their runner as a single read
  taken immediately after `s.genGate.enter()` succeeds, reused for both the
  availability check and the actual goroutine launch — not two separate reads
  at different points. Two reads can disagree if a reload lands between them:
  `handleChatSend` used to check `s.getChatRunner() == nil` *before*
  `enter()`, then read it again afterward for the launch. If the second read
  went nil after the first said configured, the handler would have already
  rendered the response fragment (and its SSE placeholder) before discovering
  there was nothing to launch, leaving the client waiting on tokens that
  would never arrive.
- `Runtime.Stop` — normal process shutdown, not a reload — drains the UI
  generation gate the same way a reload does, before anything else it tears
  down. This did not exist until it was added: `Stop` took no `*ui.Server`
  at all and never touched the gate, so the leasing this migration built for
  reloads simply did not apply to shutdown. `main.go`'s `onQuit` calls `Stop`
  before cancelling the context that eventually shuts down the UI listener,
  so a chat, task, or memory request could start — or still be running —
  while `Stop`'s own `FlushAll` and `owned.close()` ran concurrently against
  the handles it reads through: the same use-after-close hazard the reload
  path exists to close, just on the shutdown path instead. Unlike a reload,
  a drain timeout here does not abort anything — there is no generation left
  to preserve for a later retry, since the process is exiting regardless.
  `Stop` calls a dedicated `Server.DrainGenerationRequestsForShutdown`
  (`genGate.closeForShutdown`), not `DrainGenerationRequests`: the latter
  reopens admission on a timeout because a reload that timed out must abort
  and keep serving the current generation, but `Stop` proceeds to close
  handles regardless of the drain's outcome — reopening admission on a
  timed-out shutdown drain would let a fresh request in right before the
  handles it depends on are closed. `closeForShutdown` is the same wait,
  just without that reopen branch. A timeout from either the UI drain or the
  API server's own `Shutdown` means more than "new admissions failed to
  close in time," though — it means a request is still actually running
  against the current generation. `Stop` used to log that and proceed
  regardless, straight into `FlushAll` and `owned.close()`, which would pull
  the handles out from under the very request the drain could not wait for
  — the same use-after-close hazard the drain exists to prevent, reached
  through a different door. `Stop` now tracks whether both phases actually
  quiesced; on either timing out, it skips `FlushAll` and the final
  `owned.close()` and returns, leaving those handles open for the OS to
  reclaim on process exit (the request queue is still stopped either way —
  `Queue.Stop` only drains requests it already accepted, touching neither
  `rt.memHandles` nor either generation gate). `Stop` also now takes a
  `context.Context` parameter threaded into each phase's own
  `context.WithTimeout` exactly like `quiesceMemoryAndAPI`/
  `shutdownAPIServerForReload` already do, so a test (or a future caller)
  can bound every phase's wait by supplying a context with an earlier
  deadline; ordinary shutdown passes `context.Background()`.
- `Stop` and `ApplyConfig` can no longer interleave. `/config` and `/retry`
  intentionally sit outside the generation gate — an apply drains that gate
  itself — so nothing previously stopped an already-running (or newly
  submitted) `ApplyConfig` call from rebuilding services and reopening
  admission while `Stop` was mid-shutdown, with `Stop` then closing the
  replacement generation's handles instead of the one it drained. `Stop` now
  takes `Runtime.applyMu` for its entire call (the same mutex `ApplyConfig`
  already held end to end, see above) and sets a `stopping` flag under it
  before touching anything else; `ApplyConfig` checks `stopping` immediately
  after acquiring `applyMu` and bails out with the zero `ui.ApplyResult` if
  it is set. Guarding `stopping` with `applyMu` rather than `rt.mu` matters
  because `ApplyConfig` releases `rt.mu` during its own drain windows — a
  flag guarded by `rt.mu` alone would still let a concurrent `ApplyConfig`
  start and finish inside those windows while `Stop` proceeded in parallel.
- `handleProjectSwitch` takes `oldModel` (the caller's already-computed
  effective model for the pre-apply config, `rt.effectiveModelFor(&old)` in
  `config.go`) as a parameter, alongside `newModel`, rather than
  re-resolving the source project itself. This closed two related gaps at
  once. First: the `llama_on_switch=keep` branch's port sync (added when
  this was first found: a direct edit to the global `Model.Port` field in
  the same apply as a switch must still take effect, since
  `reconfigureProcesses`'s inference-client swap in `config.go` is
  unconditional on `projectSwitching`) used to re-resolve the source
  project locally, and any resolution failure — a project deleted
  mid-apply, a transient project-store error — silently skipped the sync
  entirely, even though the client had already moved to the new port
  unconditionally. `oldModel` is exactly what `rt.effectiveModelFor` already
  falls back to (the global `Model` config, unmodified) on that same
  failure, so using the caller's copy instead of re-deriving it removes the
  failure mode altogether — nothing in `handleProjectSwitch` can silently
  skip the sync just because the project store had a bad moment. Second:
  both the `keep` port-sync and the `reload` policy's skip check used to
  compare `Port` only (via `config.ModelConfigEqual`, which deliberately
  ignores both `Port` and `Verbose`), so a same-model, same-port,
  *`Verbose`-only* divergence across a switch went unnoticed in either
  direction — the committed config could say `Verbose` changed while the
  running process kept its old `--verbose` setting. Since `ModelConfig` has
  no incomparable fields, a plain `==` is exactly "did anything about the
  running process's args actually change," so `keep`'s sync now merges
  `oldModel` with `newModel`'s `Port` and `Verbose` and compares the merged
  result to `oldModel` with `==`, and `reload`'s skip check compares
  `oldModel == dstModel` directly (`dstModel` already carries the new
  config's global `Port`/`Verbose`, since neither is ever a project
  override) — both cover every field, not just `Port`, and neither depends
  on a project-store lookup succeeding. The model-mismatch UI banner is a
  separate, display-only comparison and deliberately keeps using
  `ModelConfigEqual`'s exclusion: a pure port or verbose difference is not
  a "different model" from the user's point of view.
- `session.Manager.Save` writes the sidecar (the raw conversation JSON)
  before calling the summarizer, not after. A summarizer failure — plausible
  exactly when a last-minute `flushSessionsForReload`/`Stop` flush runs,
  since a reload can be reconfiguring llama-server at that very moment —
  used to mean `Save` returned before writing either file, losing the raw
  conversation entirely rather than just the markdown summary. The sidecar
  was never part of the episode's git commit in the first place (only the
  markdown file is staged), so writing it first changes nothing about what
  ends up in git — only whether the transcript survives a failed summary.
  A durable sidecar alone was not enough for a session's *first* save,
  though: `Records`/`Resume` discover sessions exclusively through
  `sessions.jsonl`, and the only entry ever appended there used to land
  after summarization and the git commit both succeeded — so a first-save
  summarizer failure left the sidecar bytes durable but invisible to a
  later `Resume(id)` (`ErrUnknownSession`), reachable only by hand. `Save`
  now appends a *provisional* `Record` (real `ID`/`Agent`/`Project`/
  `StartedAt`/`SaveSeq`, `EpisodePath` deliberately empty) right after the
  sidecar write, but only when the session has no earlier successful save
  (`!alreadyKnown`) — a later save's failure doesn't orphan anything, since
  the session is already discoverable from its first save's record, and the
  sidecar write always carries that call's latest conversation regardless.
  `sessions.jsonl` is append-only and last-wins-by-ID (see the package's own
  doc comment, and `findLatestRecord`/`allRecords`), so if summarization
  then succeeds, the full record appended afterward simply supersedes the
  provisional one — no log rewriting involved. The provisional and final
  records from the same save deliberately carry the identical `SaveSeq` and
  `SavedAt` (both describe the same logical save attempt), which
  `LatestPerID` — used by `Records`, not `Resume` — did not originally
  handle: its tiebreak only advanced to a record with a strictly *later*
  `SavedAt`, so on an exact tie it kept whichever record it saw first,
  meaning `Records` (and the resume picker built on it) kept surfacing the
  provisional record's empty `EpisodePath` even after the save completed
  successfully. `findLatestRecord` (`Resume`'s own lookup) never had this
  bug — it just walks the log and keeps the last positional match, with no
  `SaveSeq`/`SavedAt` comparison to fall out of sync with the log's actual
  order. `LatestPerID`'s tiebreak now prefers the later-or-equal `SavedAt`
  (not strictly later) on an equal `SaveSeq`, so a tie now falls through to
  physical log position exactly like `findLatestRecord` already does.
- A rebuild's `result.LiveApplied` is reset to `false` if the rebuild it
  guards is attempted and fails. `reconfigureProcesses` sets it `true` as
  soon as it moves llama-server or the embedder, before the rebuild that
  follows is even attempted; if that rebuild then fails,
  `undoProcessReconfigure` and `restoreMemoryAndAPI` revert everything it
  did, and the result must say so rather than report success for a
  configuration that was just rolled back.
- `rt.cfg` is not committed to the loaded value until the component it
  describes has actually been rebuilt to match — immediately for the very
  first start (nothing to protect yet), inside the quiesce-and-shutdown
  success branch right before the rebuild it authorizes, and in the plain
  reconfigure path when no rebuild was needed at all. On the abort path
  (either quiesce step failing) `rt.cfg` stays at the old value; the same
  rollback applies if the rebuild itself is attempted but fails and
  `restoreMemoryAndAPI` puts the previous generation back. Reading `rt.cfg`
  live is common — `taskRunnerAdapter.RunTask` resolves sandbox roots from
  `rt.cfg.Project.ActiveProjectSlug` directly, not through the (unrebuilt)
  service graph — so committing the new value ahead of the rebuild it
  requires would let such a reader observe a project or prompt config the
  still-running old generation was never built for: a task started right
  after an aborted project switch would resolve sandbox roots for the new
  project while every memory operation still ran against the old project's
  repo.

**Cross-agent reads:** explicit only. An agent may request episodes from another agent's directory. Not automatic.

**Planned M12 semantic-write gate:** after M11 and MR0 closure, session summaries,
promoted facts, notes, and `memory_propose` use a project-local append-only event log and
immutable proposal payloads. Session logs, conversation sidecars, and vector/FTS indexes
remain outside the semantic gate as evidence, operational state, or derived projections.

### Project Store (`internal/project`)
Defines project identity and validation rules. SQL persistence lives in `internal/db`, while this package owns typed project values, slugs, directory metadata, effective model overrides, and lifecycle status such as hidden or system projects.

`Workflow.Create` and `Workflow.Update` are serialized by a package-level `workflowMu`, not a mutex on the `Workflow` value: a handler builds a fresh `*Workflow` per request, so a value-level lock would serialize nothing between two concurrent requests. One process-wide lock, not identity-keyed the way `internal/git`'s and `internal/memory`'s own locks are, is deliberate — project creation and editing are rare, human-driven actions, not a hot path — but it is what keeps two concurrent "create project" or "edit project" calls naming the same `MemoryRepoPath` from both reaching `EnsureProjectRepo`/`MoveProjectRepo` against that destination at once.

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
- Create and open index directories, addressed by a directory the caller has
  already pinned through `internal/rootfs` — `internal/memoryops` resolves the
  index's repo-relative location through the project memory repo's own handle
  rather than by an absolute `<repo>/index/_episodes` pathname, which can
  already be a link leading somewhere else before the index ever opens it.
- Append vectors idempotently by content SHA.
- Perform cosine-similarity flat scans for top-K search.
- Keep the on-disk format isolated from prompt and memory logic.

`Upsert` never opens `vectors.bin` read-write and mutates it in place. It
assembles the whole new file — the bytes already on disk plus this call's
addition — in memory and publishes it in one `WriteStreamAtomic` call, which
replaces the directory entry rather than writing through whatever it names.
An in-place append-then-truncate-on-failure was tried first and reopens the
hard-link corruption class this migration exists to close: if the `vectors.bin`
*entry* is a hard link to a file outside the repo — a different name for the
same inode, which no containment check can distinguish from the genuine file,
because a hard link is not a link a root can refuse — appending writes through
it, and a rollback truncate can then shorten that outside file too. Publishing
by rename removes the need for a rollback entirely: if the manifest write that
follows fails, the newly published bytes are simply unreferenced by any
manifest chunk, which `validateManifest` already tolerates and `Search` never
reads — harmless trailing data, not a corruption to undo.

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

First run: the row is seeded with defaults and `saved_at` is NULL. The status page shows a "Set up your harness" CTA until the user saves at least once. Only `ui.port` and `queue.max_depth` require a harness restart (`ApplyConfig` reports them as `RestartNeeded`); everything else — including model/embedder binaries, model paths, and ports — is reconfigured live when the retry callback fires, per the generation-lease machinery described under Runtime above.

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
