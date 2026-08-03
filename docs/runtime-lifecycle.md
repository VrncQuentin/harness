# Runtime Lifecycle

> **Current reference.** This document describes the lifecycle as implemented
> after the runtime-generation ownership work. It is the single canonical home
> for how the service graph is composed, re-applied, and torn down.
> [architecture.md](architecture.md) keeps the startup sequence and
> cross-cutting invariants; this document describes the mechanism.

## 1. Runtime as composition root

`internal/runtime` is the composition root of the harness. `cmd/harness/main.go`
creates the UI first, then constructs the runtime and hands it the config
store, project store, log rings, and the UI server. The runtime wires every
subsystem behind the browser surface: process managers, queue, inference
client, memory readers, prompt assembler, session manager, agent registry,
tool registry, approvals, API server, and log rings.

`main` installs the runtime's callbacks onto the UI server: `SetRetry` points
at `rt.ApplyConfig`, `SetProjectEditor` at `rt.EditProject`, `SetProcRestarts`
at the process restarts, and `SetQuit` at the tray's quit path. On quit, `main`
calls `rt.Shutdown(rootCancel, timeout)` and nothing else.

The runtime keeps its state behind locks because UI handlers, process events,
metrics, and retry callbacks run concurrently.

## 2. Generation construction and sole ownership

A **generation** is the unit of installed runtime state. Each generation is
constructed as an unpublished **candidate** that owns, from construction:

- global and active memory readers (`memory.DirReader`);
- the agent registry (`agent.DiskRegistry`);
- the prompt assembler (`prompt.DiskAssembler`);
- the session manager (`session.Manager`);
- the task runner adapter;
- index handles;
- the immutable UI snapshot (`ui.ServiceDeps`).

Ownership is assigned at construction: the candidate's resources become the
generation's, so retirement never has to transfer readers into an old
generation after the fact. The runtime keeps **no duplicate references** to
generation resources — its fields include the installed generation itself, not
the readers, assembler, session manager, or task runner directly. The one
deliberate exception is the task-runner adapter, which retains a pointer to the
runtime solely for the live C2 memory-repo predicate that must read the shared
project store at call time.

A failed candidate is discarded wholesale (`candidate.close`), which stops any
API server it built and closes its readers and handles — the installed
generation and recorded applied state are untouched.

## 3. Publisher and request/snapshot leases

Access to generation resources goes through lease counting (`acquire`/`release`
on the generation). While a lease is held, the generation's readers and handles
stay open; when the count returns to zero, the readers close and every handle
closes.

- **`AcquireUISnapshot`** — under the runtime lock, pins the generation,
  copies its immutable `ui.ServiceDeps`, and resolves the active agent under
  the same lock (so `/agents/active` switches it without a rebuild). It returns
  the snapshot and a release function safe to transfer to a detached goroutine.
  The UI server holds the runtime as a `ui.SnapshotProvider`; every handler
  calls `acquireSnapshot` once, uses only fields of that snapshot, and releases
  on every completion/error path.
- **`AcquireRequestGeneration`** — the API server's lease. It pins the
  generation and captures the assembler, session manager, and active agent as
  static adapters bound to that generation. The API server alone keeps a
  dynamic assembler because API requests legitimately use the current
  generation; it consumes the request lease per request via `WithGenLease`.

There is no separate release method: the release *is* the returned function
bound to the generation.

## 4. Atomic snapshot acquisition and retirement

Acquisition and publication share the runtime lock, so a handler cannot select
an old snapshot after its generation was retired and its readers closed — there
is no load-before-increment window.

- **Publication:** the candidate and its snapshot are built locally, bound to
  the candidate generation, and installed by swapping the live generation under
  the same lock acquisition uses. The new generation's publisher lease is taken
  before the swap; the old publisher lease is retired next. Old readers and
  handles close only after the last acquired snapshot on the old generation is
  released.
- **Handlers:** synchronous handlers release via `defer`. `/chat/send` and
  `/task/send` acquire before reading the runner or session store and transfer
  the release to the detached goroutine, which releases after the entire
  run/stream ends; every pre-launch error path releases exactly once.
  Long-lived SSE subscription handlers do not pin a generation merely for
  remaining connected — the spawned chat/task operation owns the lease.
- **Old references:** a held old snapshot keeps its generation's readers open
  and usable for real rooted operations after a reload, until the release runs.

## 5. Applied runtime state as the authority

`Runtime` owns one explicit applied-state record containing the facts needed to
compare, publish, and roll back the live system: the committed config, the
active project, the preferred/effective model, and the actually-running
llama/embedder process configuration.

The old/live state is read **exclusively** from this record — never
reconstructed from the mutable config store or the mutable project store — so a
store edit is compared against what was actually committed, not against a
store-derived guess.

The applied state distinguishes the **preferred** model from the
**running** model. Under `project.llama_on_switch=keep`, llama-server is never
reconfigured during a config apply or project switch; the prompt context
ceiling and the inference client track the running model's port/ctx, and the
status UI renders the running-versus-preferred mismatch honestly from the two
recorded values.

## 6. The ApplyConfig transaction

`ApplyConfig` runs as one transaction serialized end-to-end by a dedicated
apply lock (`applyMu`); two concurrent applies cannot interleave validation,
preparation, process changes, generation publication, or retirement. The
transaction phases are explicit:

- **prepare** — the candidate and its API server are built locally and left
  unpublished; the API listener is bound (reserving its port) but does not
  accept requests until commit, so a request on the candidate's port can never
  run against a generation that is not the one it was prepared for. A failed
  candidate is discarded wholesale, leaving the installed generation and
  recorded applied state untouched.
- **quiesce** — task loops are cancelled and sessions flushed when a rebuild
  will drop the old generation. These waits run *without* the runtime lock so
  session summarization can read live config without deadlocking; the lock is
  reacquired before commit.
- **commit** — the generation and one coherent applied state are installed
  atomically under the runtime lock, process reconfigurations are issued from
  that state (never re-derived from the stores), and the bound API listener is
  activated. Commit is structured to be infallible so the recorded applied
  state always describes the live processes.
- **retire** — the old generation's publisher lease is released under the same
  lock acquisition uses, and the previous API server is retired under a timeout
  ownership protocol: a server whose shutdown does not confirm termination
  within the timeout keeps a retained slot until a later shutdown confirms it,
  so the runtime never clears or replaces the pointer to a still-serving
  component.

The API-listener build decision is live-aware: a rebuild can be forced when the
applied config wants the API running but no listener exists, so an apply
rebuilds a missing listener even when the recorded config is unchanged. The
`/agents/active` write takes the same apply lock as `ApplyConfig`, so an apply
that has loaded one agent and is preparing cannot be overwritten by a
concurrent active-agent save, or vice versa; the live config, the recorded
applied config, and the store always agree.

`ui.ApplyResult` carries an explicit `Err`, so a failed apply is distinguishable
from a successful no-op: `LiveApplied=false` plus `Err=nil` means "nothing
needed changing", while a non-nil `Err` means the apply could not commit and the
installed generation and recorded applied state are untouched.

## 7. Active-agent and project-edit serialization

`/projects/edit` never constructs and executes `project.Workflow` directly.
`Runtime.EditProject` is the single runtime-owned project-update surface for
the UI: it serializes the edit end-to-end with `applyMu`, so an edit cannot
interleave with a config apply, an active-agent write, or a shutdown.

Inside the transaction:

- The active project's memory-repository boundary cannot be moved while the
  installed generation still targets it. The edit refuses before any metadata
  or filesystem mutation. The repository identity is settled once as a
  handle-bound proof (`Workflow.SettleUpdate` pins the destination via
  `PinRepoIdentity` and `OpenIdentified`), and that proof is re-verified at the
  moment of mutation (`Workflow.ApplyUpdate`): `SameAs` opens the current path
  and compares the retained handles with `Root.SameDir` (`os.SameFile`), so a
  repointed alias or a same-name physical replacement fails closed even when it
  reuses the pathid key. The settlement is produced only by `SettleUpdate` —
  the decision is private and a forged or zero settlement is rejected — so an
  old "same" result can never persist a path that no longer identifies the
  installed reader.
- Active-project display and model-override edits proceed; their live apply
  runs through the same transaction boundary, so the reload decision compares
  the freshly-mutated store contents with the recorded applied state — never
  with an "old" value derived from the store the edit already changed. If the
  re-apply fails (config load, validation, or candidate-preparation failure),
  the edit reports failure and restores the captured project row, so the store
  never silently diverges from the live generation.
- Inactive-project repository moves continue through the rooted
  `MoveProjectRepo` workflow, preserving its rollback behavior on
  initialization or move failure. The pre-edit active model or repository is
  never derived from the mutated project store.

## 8. API-server publication and retained retirement

The runtime's live API pointer is runtime-owned, not generation-bound. A
candidate builds its API server bound to the candidate generation; prepare
binds the listener (port reserved, no serving); commit transfers the previous
server to pending-retirement, activates the new listener, and moves servers
whose shutdown did not confirm termination into a retained slot. The terminal
drain used by shutdown re-attempts pending and previously-retained servers;
unconfirmed servers keep their slot, and the runtime only clears the live
pointer when the current server confirms termination.

## 9. Shutdown

`Runtime.Shutdown` is the one cohesive shutdown lifecycle, serialized with the
apply transaction so a shutdown cannot interleave with a config apply or a
project edit. `main` calls `rt.Shutdown(rootCancel, timeout)` and nothing else;
tests drive the same lifecycle without a root cancel. The lifecycle is
explicit:

1. **stop admissions** — the request queue closes its intake, so new UI/API
   chat or task work is refused before anything is drained;
2. **cancel root/task contexts** — the root cancel stops process managers, the
   queue worker, and UI request contexts before any wait begins;
3. **bounded drain** — task loops are cancelled and live sessions flushed with
   explicit timeouts. The runtime owns exactly one in-flight session flush: a
   save can block on the manager-wide save lock or a hung summarizer, so the
   flush runs detached from any single attempt, retries join the in-flight
   flush instead of stacking another (blocked flushes cannot accumulate waiters
   or duplicate durable saves), and a new flush starts only after a previous
   one completed with a retryable failure. The flush result is published under
   the flush lock before a broadcast completion channel is closed, so an
   immediate retry can never miss a completion. The summarizer's token loop is
   itself context-aware so a stream that never sends or closes cannot hang it;
4. **stop API/queue/process components** — API servers stop under the timeout
   ownership protocol; the queue is waited on with a bounded, context-aware
   wait, never an unbounded stop. Queue cancellation is terminal: every
   accepted request — in-flight or buffered — is resolved (failed and closed),
   so consumers ranging a response channel never hang;
5. **release only resources proven idle**;
6. **retain ownership for anything whose termination is unconfirmed**.

A drain timeout is not termination. When a bounded wait expires, the runtime
retains ownership and a later `Shutdown` retries: the queue stays owned, and
the session manager and task runner stay owned together with the complete
generation they are bound to — its readers and handles stay open so a retry can
save through them. API ownership is preserved to termination for every class of
server: active, pending-retired, and previously timed-out. A stopped queue is
never called after a failed bounded drain.

Only a fully completed attempt releases and clears ownership: the generation is
dropped, the readers close, and `started`/`applied`/`queue` are cleared.

## 10. Retry and idempotence expectations

- The snapshot provider is installed before anything else in the apply path, so
  a retry-only startup still reaches handlers with generation-backed
  dependencies.
- Retrying an apply after a failed memory/API reload restores the existing
  services rather than rebuilding from a stale guess; a retry can also rebuild a
  missing API listener when the applied config wants one running.
- `ApplyResult.LiveApplied` is true when at least one component was
  reconfigured in place; `RestartNeeded` lists tier-3 changes with no live apply
  path (UI port, queue max depth); `Err` is set only when the apply could not
  commit.
- Shutdown is idempotent: a timed-out shutdown leaves a retained, retryable
  state, and a later shutdown picks up exactly where the prior one left off.
