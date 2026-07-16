# Codebase review

**Reviewed:** 2026-07-16  
**Scope:** whole repository review of package responsibilities, boundary adherence,
duplication, simplification opportunities, legacy/dead code, and test quality.  
**Status:** read-only review; no product code was changed.

## Executive summary

The codebase has a strong set of small infrastructure packages, and the test suite
is broadly healthy. The biggest concern is not isolated low-level code quality: it
is duplicated ownership of stateful application behavior. Runtime lifecycle,
conversation persistence, semantic-index identity, project-aware agents, config
translation, and project repository mutation all have multiple implementations.
Several of those copies have already drifted into user-visible data loss, incorrect
behavior, or security problems.

The immediate priorities are:

1. Preserve data correctly: repair session IDs, save/commit serialization, semantic
   index identity, index freshness, and metrics retention.
2. Make local boundaries safe: fix file-write symlink traversal, malformed writes,
   cross-origin UI access, and approval response races.
3. Replace the runtime's diff-based lifecycle branches with one idempotent service
   graph reconciler.
4. Move project-aware agent and memory operations into shared application/domain
   services, then reduce `runtime` and `ui` to orchestration and presentation.

## Verification performed

- `go test -count=1 ./...` passed.
- `go vet ./...` passed.
- The worktree was clean before the documentation-only change.
- `go test -race` could not run: the environment is Windows/386, which Go does not
  support for the race detector.

The passing suite does not cover several of the most important graph transitions:
runtime retry/reload, concurrent saves, save-twice indexing, project create/edit
rollback, symlinked write parents, repeated metrics retention, and cross-origin
mutation attempts.

## Priority 1: correctness, data safety, and security

### 1. Episode-index identity is wrong and a live index never refreshes

`memoryops.AfterSaveEmbed` uses the mutable session ID as an index `SHA`.
`index.Add` treats an existing `SHA` as a successful no-op, but later saves overwrite
the same episode file. Consequently the first saved summary's vectors remain forever
after a re-save. Basename-only IDs can also alias between agents.

Relevant code:

- `internal/memoryops/memoryops.go:60`
- `internal/index/index.go:88-115`
- `internal/session/session.go:357-381`
- `internal/retrieval/retrieval.go:12-22`

There is a second freshness bug. Runtime opens one index instance for prompt
retrieval, scoring, and rebuilding, but the post-save hook opens and mutates another
instance. The index manifest is cached in memory, so new saved episodes are not
retrievable through the already-open prompt/scorer index until restart or rebuild.

Relevant code:

- `internal/runtime/memory_api.go:65-80`
- `internal/memoryops/memoryops.go:46-62`
- `internal/index/index.go:118-148`

**Recommendation:** give one project-scoped index service ownership of a synchronized
handle. Store `source path + content hash` in each entry, replace obsolete vectors
when the source changes, and make prompt retrieval, scoring, save, and rebuilding use
that service.

### 2. Session IDs collide and persistence is not serialized

Session IDs have one-second resolution. `Start` only avoids an ID collision with
currently live sessions, and `End` removes that guard. The API ends each request, so
two sequential API requests in the same second can reuse an ID and overwrite an
episode/sidecar. The acceptance test's short sleeps do not advance this timestamp
format and therefore hide the issue.

Relevant code:

- `internal/session/session.go:47-50, 213-232, 316-320`
- `internal/api/api.go:315-318`
- `internal/session/acceptance_test.go:208-215`

`Manager.Save` says it holds the manager mutex for I/O, but releases it before
summarization, writes, commits, logging, and the post-save hook. Concurrent saves can
write the same episode, calculate the same save sequence, interleave JSONL records,
and stage unrelated changes in one go-git commit. `git.Repo.Commit` itself is a
multi-step shared-worktree operation with no serialization.

Relevant code:

- `internal/session/session.go:328-347, 357-432`
- `internal/git/git.go:78-100`

**Recommendation:** use UUID/ULID (or a durable monotonic ID), reserve IDs beyond the
live map, add a per-session save lock/state machine, and serialize commits at the
repository boundary. Add concurrent Save and Save-versus-promotion tests.

### 3. Runtime reconfiguration abandons work and has several drifting config paths

The runtime has no single reconcile operation. On reload, `stopMemoryAndAPI` drops
the session manager and task runner without cancelling tasks, awaiting approvals, or
flushing sessions. The full shutdown path does correctly cancel and flush, which
proves this behavior is inconsistent.

Relevant code:

- `internal/runtime/config.go:97-110`
- `internal/runtime/memory_api.go:289-307`
- `internal/runtime/lifecycle.go:87-123`

Retry clears startup errors, but once `rt.started` is true, unchanged failed
subsystems are not restarted. A fixed repo path or freed API port can therefore make
the UI look healthy while memory/API services remain absent.

Relevant code:

- `internal/runtime/config.go:18-60`
- `internal/runtime/lifecycle.go:210`

Llama configuration is independently assembled on startup, global-config changes,
and project switch. Only project switch applies effective project overrides; an active
project after process restart can start the global model. Endpoint changes also leave
captured inference/embedder clients inside session, retrieval, deduplication, rebuild,
and post-save services pointed at their old ports.

Relevant code:

- `internal/runtime/lifecycle.go:151-171`
- `internal/runtime/config.go:62-95`
- `internal/runtime/project_switch.go:37-95`
- `internal/runtime/memory_api.go:68-119, 242-251`

**Recommendation:** model readiness per subsystem and make `ApplyConfig` an
idempotent reconciliation operation. Build the effective model and all manager args
in one place. Rebuild every dependent service on endpoint changes, or inject a safe
delegating client. Reload must quiesce intake, cancel/wait tasks, flush sessions while
inference remains alive, stop old services, then publish the replacement graph.

### 4. Conversation and streaming contracts have drifted

`appendUserSide` expects the complete conversation and computes a delta. Chat sends
the live transcript; task sends only the current message. A task follow-up is therefore
assembled without earlier context and can fail to append the new user turn.

Relevant code:

- `internal/runtime/adapters.go:375-393, 630-642`
- `internal/ui/chat.go:186-194`
- `internal/ui/task.go:79-83`

Text events are deliberately lossy when an agent-loop consumer is slow. Runtime builds
the persisted assistant transcript from precisely those events, so backpressure causes
permanent loss of assistant text rather than merely an imperfect stream. UI streaming
also drops text frames and uses a non-turn-specific SSE swap event, which can update
multiple historical assistant spans.

Relevant code:

- `internal/agentloop/loop.go:514`
- `internal/runtime/adapters.go:834`
- `internal/ui/chat.go:290-307`
- `internal/ui/task.go:176-191`
- `assets/templates/chat.html:123-131`
- `assets/static/htmx-ext-sse.js:87-128`

**Recommendation:** one conversation service should own a lossless transcript and
accept a single new turn. Chat, task, and API should call it. SSE should transmit a
turn-specific stream and may coalesce display snapshots, but must not be the source of
persisted text.

### 5. Project-aware agents are resolved and mutated inconsistently

Prompt assembly has project/global fallback per file. The runtime UI adapter separately
constructs agent information, displays global persona/rules in cases where prompt uses
project content, and delegates activation, persona/rules writes, and deletion to the
global-only registry. Project-only agents can be listed but cannot be correctly
activated, edited, or deleted; edits to an extending agent can alter the global file
while the model continues to use the project override.

Relevant code:

- `internal/prompt/prompt.go:455-489`
- `internal/runtime/adapters.go:74-179, 197-227`
- `internal/agent/agent.go:147-151`

**Recommendation:** create a project-aware resolver/editor in `internal/agent` with
explicit per-file origin and mutation scope. Prompt and UI should share it; runtime
should only adapt domain types to UI types.

### 6. File writes can escape the sandbox and silently truncate data

When a requested target does not yet exist, `validatePath` falls back to lexical root
checking. An in-sandbox parent symlink/junction targeting outside the sandbox passes
that check; `MkdirAll`/`WriteFile` then follows it and writes outside the allowed root.

Malformed or missing `content` is decoded as an empty string, so a bad `file_write`
call can truncate an existing file. The shell tool's advertised 64 KiB output limit is
also enforced only after `CombinedOutput` has already buffered all command output.

Relevant code:

- `internal/tools/tools.go:110-137`
- `internal/tools/tools.go:275-289`
- `internal/tools/tools.go:301-334`

**Recommendation:** resolve the deepest existing ancestor and validate its physical
path (or use no-follow/open-relative primitives), require a string content field, use
atomic write-and-rename, and stream subprocess output through a capped drain.

### 7. Local UI and approval security policies are incomplete

`/events` sends wildcard CORS while exposing harness and child-process logs. The UI has
a same-origin helper but uses it only on a few project routes; config, project edits,
memory writes, agent mutations, approvals, and shutdown are not consistently covered.
A website can read localhost logs and issue simple cross-origin form posts.

Relevant code:

- `internal/ui/sse.go:37-56, 109-132`
- `internal/ui/projects_page.go:126-166`

Approval responses look up a pending channel, unlock, remember an "always" rule, and
only then send the decision. A late or duplicate response can mutate policy even if
the approval was already timed out or handled.

Relevant code:

- `internal/agentloop/loop.go:170`

**Recommendation:** remove wildcard CORS; enforce origin/host/CSRF policy as shared
middleware on all mutations; atomically claim a pending approval before accepting it,
and remember a rule only after the response is accepted.

### 8. Metrics retention can destroy historical aggregates

Retention aggregates a partial cutoff hour, deletes raw rows, and later replaces that
hour's aggregate with only the newly eligible remainder. Earlier count/min/max/average
and ordering information are lost.

Relevant code:

- `internal/db/metrics_store.go:88-117`

**Recommendation:** downsample only complete hours, or merge aggregates with weighted
counts and correct min/max/last-value semantics. Add a test that runs retention twice
across the same hour.

## Priority 2: design, reliability, and simplification

### 9. Index persistence is non-transactional and turns corruption into a fresh index

`index.Create` claims to overwrite but does not truncate old vectors or persist an
empty manifest. `Add` appends vectors before writing the manifest; a manifest failure
leaves orphan bytes. `AfterSaveEmbed` and rebuilder treat any `index.Open` error as
"missing" and recreate an index, including malformed/permission-denied cases.

Relevant code:

- `internal/index/index.go:72-85, 98-114, 209-217`
- `internal/memoryops/memoryops.go:49-55, 173-177`

Use explicit create/replace semantics, create only for `fs.ErrNotExist`, validate
manifest offsets/count/dimension on open, and publish vector/manifest updates via
temporary files and atomic rename.

### 10. Project/repository workflows sit in HTTP handlers and are non-atomic

Project creation persists the database row before repository initialization; an init
failure leaves an orphaned project. Edit moves/initializes filesystem state before
updating the database; a database failure leaves filesystem state changed. UI also owns
memory scaffolding, promotion writes/commits, and a case-insensitive path helper that
is incorrect on Linux.

Relevant code:

- `internal/ui/projects_page.go:228-247, 297-347`
- `internal/ui/status.go:189-206, 291-315`
- `internal/ui/promotion.go:37-143`

Move these workflows into project/memory application services with validation,
compensation, and OS-correct path identity. Keep handlers responsible for parsing,
calling a service, and rendering results.

### 11. Configuration persistence and UI are high-drift parallel schemas

The same roughly 45 fields are hand-maintained in migrations, seed values, load/scan,
save arguments, defaults, validation, form rendering, and form parsing. Existing tests
assert only subsets, so a newly added field can silently be omitted. The architecture
also says SQL defaults mirror Go defaults, while the migration has no SQL defaults and
the store correctly seeds in Go.

Relevant code:

- `migrations/0001_init.up.sql:17-59`
- `internal/db/config_store.go:28-84, 92-156, 167-268`
- `internal/config/config.go`

UI controls have drifted: `Project.LlamaOnSwitch`, semantic weight, and recency weight
exist in typed config but cannot be edited; status directs users to a setting that does
not exist on the form. Numeric parsing silently retains prior values or produces nil
overrides on malformed input. `Prompt.CtxSize` is persisted, validated, rendered, and
tested but is not read by production prompt wiring; effective `Model.CtxSize` wins.

Relevant code:

- `internal/config/config.go:102-142`
- `internal/ui/config.go:130-215`
- `assets/templates/status.html:126-130`

Use one codec/column definition or generation step, add exhaustive all-fields
round-trip and schema-parity tests, make parse errors field-visible, and remove or make
`Prompt.CtxSize` authoritative.

### 12. Targeted config updates can incorrectly complete first-run setup

`ConfigStore.Save` always writes `saved_at`. It is also used by targeted operations
such as project activation, which can mark first-run setup complete without a valid
model/embedder configuration.

Relevant code:

- `internal/db/config_store.go:167`
- `internal/ui/projects_page.go:358`

Split full setup save/mark-configured from targeted preference updates.

### 13. Future database upgrades are blocked

The migration runner requires an existing database version to equal the bundled
version before calling `Up`. Once migration 2 exists, every valid version-1 database
will be rejected and told to delete its state.

Relevant code:

- `internal/db/db.go:135-159`

Accept older clean versions, run `Up`, then check the final version. Reject only dirty
or newer-than-supported databases. Derive the expected version from migrations rather
than a second manually maintained constant.

### 14. Request and tool policy models are duplicated

OpenAI completion state is independently modeled by API, queue, and inference, with
manual mappings in queue/runtime. This has semantic drift already: `omitempty` means
an explicitly requested `temperature: 0` is discarded and the backend default wins.

Relevant code:

- `internal/api/api.go:135`
- `internal/queue/queue.go:15, 205`
- `internal/inference/inference.go:78-84`

Tool IDs, default enablement, risk class, approval defaults, disabled rules, config,
database, and UI each have separate sources of truth. Give the registry descriptor
stable policy metadata and derive default approval/config behavior from it.

Relevant code:

- `internal/agentloop/loop.go:528`
- `internal/approvals/approvals.go:258`

### 15. Other reliability contract gaps

- `shell_exec` destructive-command classification is raw textual prefix matching;
  equivalent shell forms can evade it. Keep `Ask` for all shell commands unless
  normalization/tokenization is reliable on the actual platform.
- SQLite connection-pool/DSN settings do not state a deliberate write-concurrency
  policy despite concurrent config, metrics, and project writes. Add busy timeout and
  WAL/single-writer policy as appropriate. `foreignKeysDSN` treats any `_pragma` as
  sufficient, not specifically foreign keys. (`internal/db/db.go:38, 94`)
- `logbuf` can grow an unterminated line without bound, gives partial lines a new read
  timestamp, and can fan out concurrent writes out of order. (`internal/logbuf/logbuf.go:55-87`)
- Process "exponential backoff" resets as soon as spawning succeeds, so repeatedly
  crashing processes wait one second each time. (`internal/proc/proc.go:294`)
- Tray Quit invokes the shutdown callback twice. Linux treats every flock error as an
  existing instance. (`internal/tray/tray_desktop.go:21`, `internal/tray/tray_linux.go:41`)
- API discards unexpected `Serve` failures and loses partial transcripts on client/token
  errors. (`internal/api/api.go:101, 315-318`)
- The UI-first invariant is broken before UI creation if binary/home setup fails.
  (`cmd/harness/main.go:42-75`)

## Legacy, dead, and test-only surface

### Pre-layout-v2 memory API

`ExpectedLayout`, `ProjectLayout`, `MissingItems`, and `ValidateRepo` implement the
old global/projects tree and have no production callers. Much of
`internal/memory/layout_test.go` exists only for these obsolete functions. Keep the
generic scaffolding helper, but remove the old wrappers and migrate its useful tests
to layout-v2 expectations.

Relevant code:

- `internal/memory/layout.go:26-51, 146-208, 291-313`
- `internal/memory/layout_test.go:14-276`

### Dead or currently test-only APIs

The following have no production caller or are maintained only for tests:

- `session.Manager.LiveCount` and `session.SortByNewest`
- `prompt.WithLogger` and the legacy single-reader `prompt.NewDiskAssembler`
- exported/test-only `memory.Reader.Exists` and mutable `DirReader.Root`
- `Runtime.loopRegistry`
- `project.Store.AddDirectory` / `RemoveDirectory`
- `queue.Request.ID`
- `tools.Context.Ctx`; `tools.Context.HTTPClient` is populated only by tests
- inference/embedder `Health`, non-streaming completion parsing, and the runtime
  queued-client health plumbing
- `metrics.Store.Query` is currently used only by tests
- `memoryops.EpisodeRebuilder.Slug` and a pending path field are assigned but unread

Not every test seam should be deleted: deterministic options such as prompt tokenizer
or approval timeout injection are reasonable. They should, however, be unexported or
kept narrowly scoped when not part of a supported package contract.

### Legacy presentation/documentation

- `assets/templates/memory.html` still says `global/facts.md` and "every prompt",
  while the handler writes active-project facts.
- `internal/git` package documentation says repositories are never initialized even
  though it has `Init` and project repos are created in the current architecture.
- `internal/session/session.go` still mentions old `memory.ValidateRepo` wording.
- Hidden `chat-state`, `MessagesJSON`, and `chat-dirty` markup/tests preserve a
  custom-JavaScript contract that no longer exists.
- The default summarizer prompt is duplicated verbatim in session and config with a
  comment demanding manual synchronization.

## Tests to delete, rewrite, or add

### Tests that preserve weak or obsolete behavior

- `TestIndexRebuilderCreatesMissingEpisodeIndex` in runtime duplicates the owning
  `memoryops` rebuilder test nearly exactly.
- Task resume repeats the same message rather than a distinct follow-up, masking the
  conversation-delta defect.
- UI/agentloop tests explicitly assert text-frame dropping rather than completeness.
- `TestDestructiveToolsRegisteredButDisabledByDefault` only checks registry presence,
  not default disablement.
- One approval test checks values just placed in a struct literal; it cannot catch a
  production regression.
- The config detect cross-root dedup test creates distinct files and therefore does
  not test its stated behavior.
- Two M3 "integration" acceptance tests exercise a memory walk or synthetic closure,
  not production UI/runtime wiring.

### Missing high-value tests

Add tests for:

1. Save → End → Start collision under a fixed clock.
2. Concurrent Save and Save-versus-promotion/commit behavior.
3. Save twice and retrieve the updated episode; cross-agent basename isolation.
4. Corrupt/permission-denied index versus missing index, vector mismatch, partial
   write, and commit failure.
5. `ApplyConfig` retry, reload during chat/task/approval, active project on restart,
   and endpoint client replacement for every consumer.
6. Project-only and layered-agent CRUD for persona/rules/notes/activation/delete.
7. Symlink/junction parent write rejection and malformed content rejection.
8. Cross-origin route coverage and approval duplicate/timeout races.
9. Project create/edit rollback and Linux path-case behavior.
10. Repeated retention pass over the same aggregation hour.

`internal/home`, `internal/reqid`, and `pkg/httpclient` have no direct test file,
despite the repository rule requiring one per package. Current low coverage in
`memoryops` (29.6%), runtime (29%), project (58.3%), and command bootstrap (4%) lines
up with the high-risk gaps above.

## Package responsibility assessment

| Assessment | Packages | Notes |
| --- | --- | --- |
| Focused boundaries | `assets`, `home`, `reqid`, `retrieval`, `index`, `git`, `metrics`, `proc`, `embedder`, `inference`, `queue`, `approvals`, `logbuf`, `tray`, `migrations`, `pkg/httpclient` | Each has a recognizable primary responsibility. This does not mean the reliability defects above are benign. `pkg/httpclient` has no external consumer and should likely become `internal/httpclient`. |
| Mostly focused, needs contract tightening | `api`, `agentloop`, `session`, `tools`, `config`, `db`, `project`, `agent` | API duplicates completion/transcript semantics; tools combines registry, filesystem, shell, and web implementations; project validation is incorrectly in DB; agent only models a global registry despite project-aware behavior. |
| Boundaries currently too broad or inverted | `cmd/harness`, `runtime`, `ui`, `memory`, `memoryops`, `prompt` | Runtime is a valid composition root but its 900+ line adapters file spans unrelated application behavior. UI performs domain transactions. Memory mixes filesystem adapter, layout, and repository lifecycle. Prompt does storage/retrieval plus rendering. Memoryops imports UI, reversing layering. |

## Recommended implementation order

1. **Safety and persistence:** fix symlink writes, malformed writes, UI origin policy,
   approval claim semantics, session IDs/locks, git commit serialization, semantic
   index identity/freshness, and metrics retention.
2. **Runtime graph:** introduce one effective-config builder and idempotent reconcile/
   teardown flow; cover retry, reload, project switching, and endpoint replacement.
3. **Application services:** extract conversation/session orchestration, project-aware
   agent resolution/editing, and project-scoped memory/index/promotion services.
4. **Simplification:** centralize config/request/tool codecs and policy metadata;
   move project validation into `project`; make UI handlers thin.
5. **Prune and strengthen:** delete pre-layout-v2 APIs and stale markup/tests, then add
   the regression and concurrency tests listed above.

This order prevents refactoring from merely relocating the current drift and makes the
new boundaries enforceable through focused tests.

## Consolidated structural findings

This section retains the additional concrete cleanup findings from the Fable review. They complement the priority findings above; none should be implemented in a way that reintroduces the lifecycle, persistence, or security defects already described.

### Remaining drift-prone copies

- The project-only agent hydration block is repeated in internal/runtime/adapters.go in List and getProjectAgent. It should disappear when the project-aware resolver is extracted.
- Prompt episode retrieval and memory-page scoring both execute embed, Search, BestSemanticScores, and BlendEpisodeScores. They differ in ordering policy; use one score-episodes operation.
- cosineSimilarity exists in internal/index using float32 and internal/memoryops using float64. Make the mathematical primitive single-owner.
- Session writing and UI browsing each define the episodes path, markdown suffix, and agent traversal guards. The episode-layout contract belongs in one package.
- Runtime, after-save indexing, and rebuilding independently stitch index/_episodes, vectors.bin, and manifest.json. The index package should expose episode-index directory and commit-path helpers.
- Chat/task SSE handling is a near-clone. The task version has different stall behavior, evidence of drift. Use one parameterized broadcaster/subscriber.
- Runtime stores projectStore as a directory-only interface then type-asserts it back to project.Store; UI has a similar optional ListDirectories assertion. Store the interface actually required by production.
- Fact promotion and note append have an identical read, newline-normalize, append, write, commit sequence. The shared operation should live in a memory service.
- Rebuilder chunks every episode twice; memory handlers duplicate agent-name lists; chat/task duplicate ChatMessage conversion; RetrievalScorer threads unused project/agent arguments. These are safe mechanical cleanups once service ownership is clear.
- LlamaArgs has same-typed positional arguments at three call sites and duplicate health-URL construction. A typed model-manager configuration prevents swapped values that compile but fail at runtime.

### Legacy and test-only details

- Remove remaining layout-v2 wording in comments; the current term is project memory repo and no v1 alternative remains.
- Chat save and session resume retain htmx-fragment and raw-JSON branches although shipped assets use htmx and no custom fetch. Remove decodeSaveRequest, chatSaveRequest, and chatSessionResponse if no external API contract needs them.
- The old layout APIs/tests are dead surface and their removal also eliminates the near-identical MissingItems and MissingProjectRepoItems loops.
- ui.ErrTaskQueueFull and ui.ErrTaskCancelled have no producer or consumer.
- Health on inference.Client and embedder.Client has no production caller; proc.Manager owns process health. Removing it removes queuedInferClient.raw and fake-method boilerplate.
- project.Store.AddDirectory/RemoveDirectory, metrics.Store.Query, and memory.Reader.Exists have no production caller. Keep concrete implementations only if a future feature needs them; do not widen active interfaces preemptively.
- Request.ToolChoice and CompletionRequest.ToolChoice are plumbed but never produced by API or agent loop. Remove speculative plumbing until tool-choice input exists.
- NewDiskAssembler is a single-repo compatibility constructor used only in tests. Migrate tests to the project-aware constructor.
- SortByNewest is tested but Records hand-rolls a different sort without its ID tie-break. Reuse it in Records or delete it and the test.
- WithApprovalTimeout, WithTokenizer, and process breaker threshold/window are sound deterministic test seams. Keep them with honest comments.
- tools.Context.HTTPClient is a valid hermetic test seam; production should explicitly wire the fallback client if it is part of the task-runtime contract.

### Boundary refinements

- memoryops imports ui only to return ui.RetrievalScore. Define a domain score type and translate it in runtime.
- UI chat uses UI-owned DTOs, but UI task exposes agentloop.Event directly to templates. Add UI task-event DTOs and map them in runtime.
- UI imports prompt solely for EstimateTokens. Move that heuristic to a leaf tokens or memory package.
- ui/metrics.go serializes Prometheus exposition itself. Move wire-format encoding to metrics.
- DB decides default memory-repo paths. Path policy belongs in home/config, not persistence.
- proc mixes generic supervision with model-specific argument construction. Keep proc generic and build typed llama/embedder configuration at the runtime boundary.
- pkg/httpclient has no external consumer. Rename it internal/httpclient; do not promote other packages to pkg without an actual external consumer.

### Tests to remove or correct

- The test named destructive-tools-disabled checks registration only, not disabled-by-default behavior.
- One approvals test verifies values just written into a literal and cannot catch a product regression.
- The config cross-root dedup test creates distinct files and cannot prove deduplication.
- Two claimed M3 integration tests exercise a memory walk or synthetic closure, not production wiring.
- Health and non-streaming inference tests should be removed or replaced if the test-only production APIs are removed.
