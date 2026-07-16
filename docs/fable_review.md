# Codebase review — duplication, simplifications, legacy

Full-repo review (all `internal/` + `cmd/` production sources, ~16k lines) focused on
three axes: drift-prone duplicated logic, simplification opportunities, and legacy
surfaces. Findings are ranked within each section. `go vet ./...` is clean.

---

## 1. Drift-prone duplicated logic

### 1.1 Agent-file fallback resolution exists twice — UI can disagree with the actual prompt

The rule *"project file overrides global; persona/rules fall back to the global
library; notes never fall back"* is implemented independently in:

- `internal/prompt/prompt.go` — `resolveAgentFile` (feeds the real prompt)
- `internal/runtime/adapters.go` — `uiAgentRegistryAdapter.List/Get/getProjectAgent/buildAgentInfo`
  (feeds the /agents page)

If the fallback policy changes in one place, the UI displays content the assembler
does not send. Additionally, *within* adapters.go the project-only-agent hydration
block appears verbatim twice: in `List()` (project-only branch) and in
`getProjectAgent`. This is the single most drift-sensitive duplication in the repo.

**Fix:** one shared resolver (natural home: `internal/agent` or `internal/prompt`),
consumed by both the assembler and the UI adapter.

### 1.2 Blended retrieval scoring is computed two ways

`prompt.loadEpisodes` and `memoryops.EpisodeScorer.ScoreEpisodes` both do:
embed query → `Search(len*2)` → `retrieval.BestSemanticScores` →
`retrieval.BlendEpisodeScores`. The /memory page's score column is supposed to
explain what the assembler retrieves; two implementations can silently diverge
(they already differ subtly — the scorer sorts paths itself, the assembler relies
on glob order).

**Fix:** extract one shared "score these episodes for this query" function in
`retrieval` or `memoryops`; both call it.

### 1.3 `proc.LlamaArgs` — 9 positional same-typed args, three call sites

`internal/runtime/lifecycle.go`, `internal/runtime/config.go`, and
`internal/runtime/project_switch.go` each spell out the full argument list, plus
the health-URL `fmt.Sprintf` four times and `EmbedderArgs` twice. Four consecutive
`int` params and two consecutive `string` params mean a swapped pair compiles fine
and fails at runtime.

**Fix:** `LlamaArgs(cfg config.ModelConfig)` (project_switch already holds one via
`EffectiveModel`), plus a `healthURL(port)` helper.

### 1.4 `cosineSimilarity` implemented twice

`internal/index/index.go` (float32) and `internal/memoryops/memoryops.go`
(float64). Same math, different precision — quiet-drift pair.

**Fix:** one exported helper serving both.

### 1.5 Episode-layout contract lives in two packages

`episodes/`, `.md`, and the "agent name must not contain separators" guard are
declared independently in `internal/session/session.go` (writer side:
`episodesRootRel`, `episodeFileSuffix`, `validAgent`, `validID` — the latter two
have identical bodies) and `internal/ui/memory.go` (browser side: `episodesRoot`,
`episodeFileSuffix`, `validAgentName`). If the writer changes the episode path
shape, the browser stops finding episodes with no compile error.

**Fix:** path constants + traversal guard in one place (e.g. `internal/memory`).

### 1.6 `index/_episodes` + `vectors.bin`/`manifest.json` paths stitched in three places

`internal/runtime/memory_api.go`, `memoryops.AfterSaveEmbed`, and
`memoryops.EpisodeRebuilder.Rebuild`. The `index` package already owns
`vectorsFile`/`manifestFile` as private constants.

**Fix:** export `index.EpisodesIndexDir(repoPath)` / `index.CommitPaths()` (or
equivalent) helpers.

### 1.7 Chat vs Task SSE plumbing is a near-clone

`handleChatEvents`/`broadcastChatSSE` (`internal/ui/chat.go`) and
`handleTaskEvents`/`broadcastTaskSSE` (`internal/ui/task.go`) differ only in the
`sync.Map` used and the task version's "reliable" send mode — behavior that has
already drifted between the two (eviction on stall exists only on the task side).
Related duplications in `internal/runtime/adapters.go`: the session mint-or-attach
block appears in both `chatRunnerAdapter.Run` and `taskRunnerAdapter.RunTask`, and
the assistant-flush block appears twice inside the chat token goroutine.

**Fix:** one parameterized SSE subscriber/broadcaster; one `mintOrAttachSession`
helper; one assistant-flush closure.

### 1.8 `rt.projectStore` stored too narrow, asserted back up in three places

The field is `projectDirectoryStore` (`internal/runtime/runtime.go`) but
`SetProjectStore` takes a full `project.Store`. `memory_api.go`,
`project_switch.go`, and `adapters.go` all do `rt.projectStore.(project.Store)`
with a runtime failure path that cannot happen in production. The UI repeats the
trick with an anonymous interface assertion in
`internal/ui/projects_page.go` (`countDirectories`).

**Fix:** store `project.Store` in the field; tests stub the full interface. The
assertions, `ok` checks, and anonymous interface disappear.

### 1.9 Promotion handlers share a copy-pasted append-and-commit block

`handlePromoteFact` and `handleAppendNote` (`internal/ui/promotion.go`) — the
"read existing, ensure trailing newline, append, write, commit" sequence is
identical except for path and commit message.

**Fix:** one `appendAndCommit(store, committer, path, text, msg)` helper.

---

## 2. Legacy

### 2.1 Dead single-repo layout API in `internal/memory/layout.go`

`ExpectedLayout`, `MissingItems`, `ValidateRepo`, and `ProjectLayout` describe the
old monolithic layout (`global/rules.md`, `projects/<slug>/...`). Their only
callers are their own tests in `layout_test.go` and a stale doc-comment reference
in `internal/session/session.go` (`NewManager` comment still says
"memory.ValidateRepo"). Superseded by
`ExpectedProjectRepoLayout`/`MissingProjectRepoItems`/`ValidateProjectRepo` when
memory went project-scoped (#239); survived the #241 legacy purge. ~150 lines of
production code + ~250 lines of tests. Deleting them also removes
`MissingProjectRepoItems`'s twin loop (`MissingItems` is a near-exact copy).

### 2.2 "layout-v2" terminology in comments

PR #242 dropped layout-v2 from the docs, but comments still use the term in
`internal/memory/layout.go`, `internal/memory/project_repo.go`,
`internal/prompt/prompt.go`, `internal/ui/status.go`, `internal/ui/ui.go`,
`internal/runtime/setup.go`, and two test files. There is no layout-v1 anymore;
the docs now say "project memory repo".

### 2.3 Pre-htmx JSON branches in chat session handlers

`handleChatSave` and `handleChatSessionResume` (`internal/ui/sessions.go`) each
carry a dual path: htmx fragment vs raw JSON. `chat.html` drives both endpoints
exclusively via `hx-post`/`hx-get`; no `fetch()` exists anywhere in assets. The
comments themselves say the fragments "replace the browser's JS transcript
rebuild" — the JSON branches are the replaced path. They also sit on the UI port,
which the architecture says is not an API surface. Removing them also removes
`decodeSaveRequest`, `chatSaveRequest`, and `chatSessionResponse`.

### 2.4 Small dead items

- `ui.ErrTaskQueueFull`, `ui.ErrTaskCancelled` (`internal/ui/task.go`) — defined,
  never returned or checked.
- `session.Manager.LiveCount` — zero callers; its comment claims the UI status
  page uses it, which is no longer true.
- `memory.Reader.Exists` — no production caller; only tests use it. Removing it
  narrows the core interface every fake must implement.
- `prompt.NewDiskAssembler` — production only uses `NewProjectDiskAssembler`; the
  single-repo constructor is test-only convenience (see also §4).

---

## 3. Simplifications

- **`EpisodeRebuilder.Rebuild` chunks every episode twice**
  (`internal/memoryops/memoryops.go`): once to filter empties, again to build
  `allChunks`. Store `chunks` in the `pending` struct instead of `content`; the
  second pass and the `chunkCounts` bookkeeping disappear.
- **`RetrievalScorer.ScoreEpisodes` takes `projectSlug` and `agent` that the only
  implementation ignores** (both `_`). Narrow the interface in
  `internal/ui/memory.go`; three call sites stop threading
  `s.activeProjectSlug()` through for nothing.
- **AgentNames population duplicated** in `handleMemory` and
  `handleMemoryEpisodes` (`internal/ui/memory.go`) — one `agentNames()` helper.
- **`[]ChatMessage → []inference.Message` conversion** repeated in `Run` and
  `RunTask` (`internal/runtime/adapters.go`) — trivial helper alongside
  `appendUserSide`.
- **Consistency observation:** `sameOrigin` CSRF checking applies only to project
  activate/hide/unhide (`internal/ui/projects_page.go`) but not to project
  create/edit, agent create/delete/edit, memory save/promote, config save, or
  shutdown — all state-changing POSTs on the same server. Either apply it
  uniformly (small mux wrapper) or drop it; the halfway state is the worst of
  both.

---

## Priorities

If only three things get fixed: **2.1** (delete the dead layout-v1 API and its
tests), **1.1** (unify agent-file fallback resolution), and **1.3** (make
`LlamaArgs` take `ModelConfig`). Those remove the largest legacy mass and the two
duplications most likely to produce a user-visible inconsistency.
