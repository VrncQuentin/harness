# Memory System

> **Current reference.** This document describes what `main` implements now. It
> is the single canonical home for the memory layer's layout, read path,
> session lifecycle, promotion, ownership, indexing, durability, and current
> limitations. Planned memory work, evidence gates, and open decisions live in
> [memory_roadmap.md](memory_roadmap.md). The filesystem primitives the layer
> relies on are documented in [filesystem-security.md](filesystem-security.md).

## 1. Terminology

- **Global versus active project.** `global` is a first-class project repo that
  is the active project by default; it behaves like any other project. The
  **active project** is selected by `config.project.active_project_slug`. Prompt
  memory (rules, user facts, promoted facts, agent notes, episodes) is always
  read from the active project's repo. The global repo's `agents/` directory
  additionally serves as the fallback *definition* library — `persona.md` and
  `rules.md` only; notes never fall back.
- **Project memory repository.** One plain git repo per project, default
  `~/.harness/projects/<slug>/`. A user-provided git directory is used as-is; a
  non-git directory is initialized with go-git; an omitted directory creates the
  default and initializes it with go-git. No git binary is required. Repository
  *file I/O* goes through rooted `rootfs` handles; *git operations* (init,
  commit, workspace ops) go through the go-git wrapper.
- **Operational state versus semantic memory versus derived indexes.**
  - *Operational/session evidence:* `sessions.jsonl`, conversation sidecars,
    and SQLite (`harness.db` holds config, metrics, and the projects table — no
    memory content).
  - *Semantic memory records:* session episodes (summarize → write → commit) and
    UI promotion (`PromoteFact` / `AppendAgentNote`).
  - *Derived projections:* the episode vector index
    (`index/_episodes/{vectors.bin, manifest.json}`) and index rebuilds.

  The distinction matters because the planned semantic-write gate will gate the
  semantic records only, never the supporting or derived writes.

## 2. Canonical repository layout

Each project memory repo holds:

```
rules.md                    ← project rules
user.md                     ← facts about the user (this project)
facts.md                    ← promoted facts (this project)
agents/<name>/{persona.md, rules.md, notes.md}
sessions.jsonl              ← append-only session log
episodes/<agent>/<timestamp>.md
index/_episodes/{vectors.bin, manifest.json}
artifacts/
```

Layout entries are validated against the canonical `ExpectedProjectRepoLayout`.
Scaffolding creates missing entries (`.gitkeep` for directories, empty files)
without ever overwriting or deleting an existing entry, routes every write
through pinned `os.Root` handles, and holds the repository-wide mutation
coordinator across the scaffold write and its commit. The layout is validated
before the repo is used.

## 3. Prompt read path and layer ordering

The prompt assembler (`internal/prompt`) renders, in order:

1. `# Rules` — `rules.md` in the active project repo (never trimmed)
2. `# User` — `user.md` in the active project repo (never trimmed)
3. `# Persona` — resolved agent persona (active project overrides the global
   library)
4. `# Agent Rules` — resolved agent rules (active project overrides the global
   library)
5. `# Facts` — `facts.md` in the active project repo (never trimmed; keep lean
   by design)
6. `# Notes` — agent notes in the active project repo (never falls back to the
   global library)
7. `# Episodes` — retrieved episodes, active-project top-K by blended score,
   trimmed oldest-first
8. Conversation turns — current session history

Assembly receives **two physical repo readers**: the active project repo (rules,
user, facts, notes, episodes) and the global repo (fallback agent-definition
library only). When both configure to the same repo, one generation owns a
single handle.

**Budgeting.** The sum of layers 5–7 must not exceed `memory_token_budget`
(default 6144); episodes are trimmed oldest-first to fit, and when blended
retrieval is active, lowest-scored first. A `conversation_reserve` (default
8192) is always guaranteed for live turns: if memory + conversation would exceed
ctx_size, episode count is reduced further. Layers 1–4 are never trimmed; if
even zero episodes exceed the budget, facts/notes are not silently dropped — a
warning is emitted instead. Token estimation is a rune-quarter heuristic shared
with the governor's B5 gate.

## 4. Session lifecycle

`internal/session` owns the conversation lifecycle. A session has a stable ID
(an RFC3339 UTC timestamp sanitized for use as a filename), an agent, a
project, a started-at time, and the conversation. The lifecycle:

- **Live session** — `Start` (fresh or resumed), `Append`, `End`.
- **Sidecar** — `Save` first durably publishes the raw conversation sidecar
  `episodes/<agent>/<id>.json` (working-tree only, intentionally uncommitted)
  so a crashed save can be resumed.
- **Episode publication** — the summarizer (a fresh inference call that
  deliberately bypasses the prompt assembler and the request queue) writes
  `episodes/<agent>/<id>.md`, which is committed to git with a structured
  message (`[agent:<n>] [type:episode] ...`).
- **Explicit monotonic attempts.** A save is an explicit, durable recovery
  transaction, not a correctness signal inferred from timestamps or log order.
  One save attempt runs under the manager-wide save lock:
  1. allocate a monotonic attempt identifier before any fallible work;
  2. durably publish the sidecar through the rooted writer;
  3. append and fsync an explicit `pending` record for that attempt;
  4. summarize;
  5. publish and commit the episode;
  6. append and fsync an explicit `complete` record for the same attempt.

  Recovery selects the winning record per session by attempt identifier first,
  then state precedence: for one attempt, `complete` deterministically
  supersedes `pending`. Wall-clock timestamps, empty paths, and physical log
  order never decide correctness; timestamps remain display/sort-only. A
  summarizer, episode-publication, or commit failure leaves a discoverable
  `pending` session whose raw sidecar can be resumed.
- **Compatibility handling for legacy records.** Records without the explicit
  fields are legacy records from before the explicit-state format — they were
  only ever appended after a fully successful save, so they normalize to
  `complete` ordered by `save_seq`. The log is never rewritten; new-format
  state is never inferred from `episode_path`. Reading and selection accept
  fully legacy records or valid explicit records; appending accepts only
  explicit records (a recognized typed `state` with a positive `attempt`), so a
  malformed hybrid can neither be written nor influence recovery.
- **Flush.** `FlushAll` saves every live session with a non-empty conversation,
  in deterministic order, and joins per-session errors.

## 5. Promotion

`internal/memory`'s promotion service owns the two semantic writers driven by
the UI:

- **`PromoteFact(text)`** — trims the text, appends it to `facts.md`, and
  commits with a structured message.
- **`AppendAgentNote(agent, text)`** — validates the agent name, appends to
  `agents/<name>/notes.md`, and commits.

Both follow the same append/commit shape: read the existing file, append the
new paragraph, write through the pinned rooted writer, commit. **Rollback:** on
commit failure the previous file content is restored — an existing file is
rewritten with the prior bytes; a newly-created file is removed. Dedup
(embedding similarity against existing `facts.md` lines above the promotion
threshold) runs before fact promotion; there is no dedup on episodes or notes.

## 6. Ownership

- **Generation-owned readers.** Each runtime generation owns the global and
  active memory readers, the session manager, index handles, and the immutable
  UI snapshot. The session manager is wired directly to the same
  generation-owned active reader — no separately opened session handle — so the
  `sessions.jsonl` append goes through the same pinned root as episode reads.
  Readers and handles close only when the generation's lease count returns to
  zero. See [runtime-lifecycle.md](runtime-lifecycle.md).
- **Repository identity validation.** `memory.DirReader` opens and identifies a
  repo in one step (`OpenIdentified` + `NewAnchorFromRoot`) and compares its
  retained identity against the git repository's retained boundary with
  `os.SameFile` at runtime construction. Every read re-opens the anchor, so a
  same-name replacement is refused. Scaffolding and moves bind every write to
  the retained git boundary before writing.
- **Shared physical-identity mutation coordinator.** All mutations of one
  physical repository — git commits, index publication, scaffolding, moves —
  serialize on one `internal/coord` gate keyed by physical identity. Index
  publication and the following git commit run inside one repository
  transaction (`git.Repo.WithMutation`); a project-repo move's copy and its
  commit, and a project-repo scaffold write and its commit, each run inside one
  transaction held across both. See [filesystem-security.md](filesystem-security.md).

## 7. Indexing and retrieval

- **After-save embedding.** `memoryops.AfterSaveEmbed` indexes the rendered
  episode body (the exact bytes written to disk). It computes **one SHA-256
  over the complete rendered episode body**, splits the body into paragraph
  chunks, embeds every chunk through the embedder, and upserts the chunk
  vectors into the episode index under that single source/body hash — all
  inside one repository transaction with the commit.
- **Episode index.** `index/_episodes/vectors.bin` + `manifest.json`, managed by
  `memoryops.EpisodeIndex` over `internal/index`. Flat cosine scan for the
  current corpus sizes.
- **Content hashes and idempotence.** The manifest stores one entry per episode
  source carrying that whole-body hash. Idempotence is per *source*, not per
  chunk: upsert starts from the committed on-disk state and skips the entire
  episode when its source and whole-body hash are already present; empty hashes
  never match. Episode indexes use the repo-relative source path as the index
  entry SHA.
- **Rebuild.** Walking the episode files and re-embedding any hash missing from
  the manifest. Idempotent and safe on a fresh clone; rebuild publishes and
  commits inside one repository transaction.
- **Semantic/recency blending.** Retrieval blends
  `semantic_weight * similarity + recency_weight * exp_decay(distance, n)`
  over episodes only, requiring the episode paths oldest-first. The prompt
  assembler and `memory_query` both reach it through the same scoring path.
- **Deduplication.** Embedding-cosine comparison against existing `facts.md`
  lines before fact promotion, above the configured promotion threshold.
- **Trace/evaluation status.** Every retrieval invocation emits one versioned
  call record (`record_type: "call"`, `outcome: scored | unscoreable | error`)
  to date-bucketed NDJSON with 30-day retention, with deletion restricted to
  identity-verified entries; a scoreable invocation additionally emits one
  `candidate` record per scored episode carrying project slug, full SHA-256
  query ID, semantic/recency scores, the configured weights, the final blended
  score, a one-based final post-sort rank, and whether the candidate was
  selected into the caller's requested top-K. Emission happens inside
  `ScoreEpisodePaths`, so the prompt-assembler path is measured as well as
  explicit `memory_query` calls; both callers pass a `TraceContext` carrying
  the active project slug and requested top-K. Present limitations: sink
  construction failures are silently ignored, emission errors are discarded,
  and shutdown never closes the sink. These are documented closure items in
  [memory_roadmap.md](memory_roadmap.md).

## 8. Durability, recovery, and shutdown-retention invariants

- **Atomic writes.** File publication goes through rooted temp-file-plus-rename;
  a failed write never leaves a half-written target under its final name.
- **Append-only log.** `sessions.jsonl` is appended with a rooted append
  primitive that cannot truncate or rewrite the log; a failed append never
  cleans up by name.
- **Save order durability.** The sidecar is durable and the `pending` record
  fsynced *before* summarization; the `complete` record is appended only after
  the episode is committed. A crash at any point leaves either a resumable
  `pending` session or a fully committed episode — never a false `complete`.
- **Shutdown retention.** On shutdown the runtime cancels tasks and flushes
  live sessions with explicit timeouts. A drain timeout is not termination: the
  runtime retains the complete generation — readers, session manager, and task
  runner — so a later shutdown retry can save through them. At most one
  in-flight session flush runs; retries join it rather than stacking another
  (no duplicate durable saves), and a new flush starts only after a previous one
  completed with a retryable failure. See [runtime-lifecycle.md](runtime-lifecycle.md).
- **Crash posture.** Interactive queued requests are lost on a hard kill, but
  saved sessions and committed memory remain intact.

## 9. Current limitations and explicit non-goals

What does not exist today: origin-aware retrieval records (memory hits do not
yet carry the origin of their underlying content), stable memory record IDs,
supersede chains, a persistent proposal/decision gate, FTS5, or cross-file
retrieval. `memory_query` returns one outer extraction-class result even though
episode summaries are inference content.

- Retrieval is a two-signal weighted blend over episodes only.
- Dedup exists only for fact promotion, not episodes or notes.
- Directory/attached-repo indexing is deferred; only the episode index exists.
- `memory_query` requires the embedder running and is off by default in config.
- go-git resolves storage by pathname; the wrapper pins and verifies the
  repository boundary at open but does not claim go-git itself is
  handle-relative.
- There is no CLI; all memory-repo management is through the `/projects` UI.

Explicit non-goals (permanently out of scope): self-modifying memory schema
(schema changes are code changes with a migration); autonomous supersede or
conflict judgment by the model; and any Python dependency in the memory
pipeline.
