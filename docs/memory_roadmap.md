# Memory Roadmap — M12

Milestone position: **this document is M12**, scheduled after M11 Pipeline DSL. It plans
the evolution of Harness memory from its current shape toward measured retrieval,
explicit content provenance, and a persistent semantic-write gate.

Sequencing against the other milestones:

- **MR0 is the current M10.3 closure work, not M12 proper.** Trace/eval code exists, but
  production sink wiring, one canonical schema, a ten-query labeled set, and a recorded
  baseline are still required before MR0 passes.
- **M11 starts after MR0 closes; M12 starts after M11.** Reprioritizing either milestone
  requires an explicit roadmap change; the documents must not silently authorize
  parallel milestone work.
- **MR1** introduces an origin-aware retrieval record after MR0's schema is accepted.
- **MR2** is the conditional FTS5 signal. It proceeds only if MR0 shows keyword misses
  that weight tuning of the existing signals does not close; otherwise it is recorded as
  skipped.
- **MR3** introduces its own persistent proposal/decision gate. It does not reuse the
  manual-action `Result.Proposal` boolean from M10.4.
- **MR4** follows MR3 and adds supersede-aware reads plus possible-conflict surfacing.

Phases run in order; a conditional phase counts as complete when its evidence-backed skip
decision is recorded. **No memory schema work starts before MR0 reports.**

---

## Current state [P]

**Storage:**
- `markdown + vectors.bin + manifest.json` in per-project git repos
- SQLite (`harness.db`): config, metrics, projects table — no memory content
- One repo per project at `~/.harness/projects/<id>/` (M9, shipped)

**Layout per repo:**
```
rules.md, user.md, facts.md
agents/<name>/notes.md, agents/<name>/persona.md, agents/<name>/rules.md
sessions.jsonl
episodes/<agent>/<timestamp>.md
index/_episodes/vectors.bin
index/_episodes/manifest.json
artifacts/
```

**Retrieval (`internal/retrieval`):**
Two-signal weighted blend over episodes only:
```
score = semantic_weight * similarity + recency_weight * exp_decay(distance, n)
```
No FTS5, no ranked-list fusion, no RRF. `semantic_weight` and `recency_weight` are
`PromptConfig` fields, user-tunable.

**Semantic writers (no common gate):**
1. Session lifecycle: summarize → write episode → commit (via `internal/session`)
2. UI promotion: `PromoteFact` / `AppendAgentNote` (via `internal/memory.PromotionService`)

Supporting writes are separate: session conversation sidecars and `sessions.jsonl` are
operational/session evidence, while vector index update and rebuild are derived data.
MR3 gates semantic memory records; it must not emit semantic decisions for these supporting
or rebuildable writes.

**Dedup:** embedding cosine similarity against existing `facts.md` lines before
`PromoteFact` commits — no dedup on episodes or notes.

**Origin classes (M10.1, shipped):** the tool layer records how a tool result was
produced — `tools.OriginClass` with values `extraction` and `inference`. Memory hits do
not yet carry the origin of their underlying content. `memory_query` currently returns
one outer extraction-class result even though episode summaries are inference content;
MR1 separates those concepts and makes origin per-hit.

**What does not exist:** origin-aware retrieval records, stable memory record IDs,
supersede chains, a persistent proposal/decision gate, FTS5, or cross-file retrieval.

---

## Instrumentation gate — MR0 (= M10.3)

**Prerequisite for everything else. No memory schema work starts before this reports.**

MR0 **is** tool_roadmap.md's M10.3 closure gate. `memory_query`, trace types, the NDJSON
sink, and `cmd/eval-retrieval` exist, but the runtime does not install the sink and the
implementation, evaluator, and documentation currently use incompatible schemas and
metrics. The main roadmap must keep M10.3 unchecked until this section passes.

MR0 lives in `internal/retrieval`, `internal/memoryops`, and runtime wiring. It changes no
authoritative memory schema. The evaluator remains a developer-side binary and is not a
Harness CLI.

**Canonical trace schema:**

```go
type RetrievalTrace struct {
    Version        int       // schema version
    RecordType     string    // "call" | "candidate"
    ProjectSlug    string    // namespaces project-relative paths
    QueryID        string    // full SHA-256 hex; never raw query text
    Candidate      string    // project-relative episode path; candidate rows only
    Semantic       float64   // raw similarity
    Recency        float64   // raw exp_decay value
    SemanticWeight float64   // semantic_weight at call time
    RecencyWeight  float64   // recency_weight at call time
    Score          float64   // final blended score
    Rank           int       // one-based final-score rank
    Returned       bool      // selected into the configured top-K
    Outcome        string    // scored | unscoreable | error; call rows only
    Timestamp      time.Time
}
```

Every invocation emits one call row. A scoreable invocation with candidates additionally
emits one candidate row per candidate. Blank queries, missing embedders/indexes, empty
results, and scoring errors still emit a call outcome so evaluation does not silently
discard the failure modes it is meant to expose. Raw queries, raw errors, and episode
content never enter trace files.

Emission remains at `ScoreEpisodePaths`, the shared choke point for prompt assembly and
`memory_query`. Both callers pass a `TraceContext` containing the active `project_slug`
and their requested top-K so `Returned` has one unambiguous meaning. The runtime installs
and closes the sink after the harness home is known. Sink creation or write failures are
surfaced through the existing setup/log path rather than silently discarded.

Storage: append-only NDJSON under `~/.harness/logs/retrieval/<date>.ndjson`. Not in
the memory repo, not in SQLite. Log rotation on date boundary; keep 30 days.

**Measurement harness (D3):**

One canonical NDJSON file per project:

`~/.harness/eval/retrieval/<project-slug>.ndjson`

```json
{"query":"the Go AST package discussion","relevant":["episodes/coder/2025-01-15T10:30:00Z.md"]}
```

The binary replays each query against the selected project repo and reports Precision@3
and Recall@3 for semantic-only, recency-only, and the configured blend. MRR may remain as
an additional diagnostic, not as a substitute. Baseline mode rejects fewer than ten valid
rows and writes a machine-readable result under
`~/.harness/eval/retrieval/results/<project-slug>-<timestamp>.json`.

**Acceptance gates for MR0:**
- Runtime startup installs the production sink and graceful shutdown closes it.
- Every invocation emits one call row; every scoreable candidate emits one candidate row
  with correct final rank, weights, and `Returned` state.
- Project identity prevents equal episode paths in different projects from colliding.
- Runtime, evaluator, tests, and docs use the same versioned trace and label schemas.
- The evaluator runs against at least ten real labeled queries and produces per-signal
  and combined Precision@3 and Recall@3 plus a machine-readable baseline artifact.
- M10.3 is checked only after that baseline run is observed, not merely after unit tests.

---

## MR1 — Origin-aware retrieval records (C3)

**Gated on:** MR0 complete and its versioned trace schema accepted. MR1 does not require
a retrieval-quality deficiency; it repairs the provenance contract that later memory
phases consume.

**What:** Distinguish the origin of underlying memory content from the mechanism that
returned it. A deterministic `memory_query` operation does not turn model-generated
episode summaries into extraction-class content.

Move the shared spellings to a neutral `internal/provenance` package and have the tool
and memory layers use that type at the memory read boundary:

```go
type MemoryRecord struct {
    ProjectSlug string
    ID          string // transitional: project slug + normalized repo-relative path
    Path        string
    Kind        string // episode | fact | note
    Origin      provenance.OriginClass
    Score       *float64 // non-nil only for ranked retrieval candidates
}
```

MR1 does not broaden `memory_query` beyond episodes. Episode hits receive scores; static
facts and notes carry the same provenance envelope as prompt layers but are not invented as
ranked query candidates. Cross-file retrieval remains a separate, unscheduled capability.

MR1's IDs are retrieval addresses, not the final stable record IDs introduced by MR3.
They must still be project-scoped so identical relative paths in two projects cannot
collide.

**Initial origin rules:**

- Session summaries and their episode files are `inference`.
- Promoted facts and agent notes are `inference` under the current writers.
- Deterministic parser/file content is `extraction` only when the stored record carries a
  deterministic source reference and content hash. Path shape alone never upgrades
  content to `extraction`.
- Unknown origins fail closed to `inference`.

`memory_query` returns origin per hit. If the outer `tools.Result` still needs one origin,
it is `inference` when any returned hit is inference-class and `extraction` only when all
hits are extraction-class. The formatted result exposes each hit's origin explicitly.

Origin is provenance metadata, not authorization. It never bypasses tool approvals,
sandbox checks, M11 verify/gate commands, or human review. Governor transforms preserve
the field; they do not silently treat extraction as permission to act.

**Trace addition:** candidate rows gain `origin` using the same vocabulary.

**Acceptance gates for MR1:**
- Every ranked memory hit carries project, ID, kind, path, score, and origin.
- Episode hits and the current static fact/note prompt records are inference-class.
- MR1 does not add facts or notes to `memory_query` or claim cross-file retrieval.
- Unknown content fails closed to inference; path naming alone cannot claim extraction.
- Mixed-origin `memory_query` results retain origin per hit and use the conservative outer
  result rule.
- Trace rows and UI/tool rendering preserve origin.
- Tests prove origin metadata cannot bypass approval or verification behavior.

---

## MR2 — Conditional FTS5 retrieval signal

**Gated on:** MR1 complete and MR0 showing keyword-heavy queries (tool names, function
names, exact error text) are missed after tuning the existing semantic/recency weights.
If that deficiency is absent, record MR2 as skipped and preserve the two-signal system.

**What:** Add SQLite FTS5 as a third retrieval signal over episode text. The FTS table is
a disposable projection; git-backed markdown remains authoritative.

**Storage:** one shared table in `harness.db`, explicitly scoped by project:

```sql
CREATE VIRTUAL TABLE episode_fts USING fts5(
    project_slug UNINDEXED,
    path UNINDEXED,
    content_sha UNINDEXED,
    content,
    tokenize = 'porter ascii'
);
```

The columns are stored rather than contentless so hits map back to project/path and stale
rows can be deleted. `(project_slug, path)` is the logical key; rebuild deletes/reinserts
changed rows using `content_sha`. Equal paths in different projects never share results.

Rebuild the active project's projection on activation and after each episode commit;
delete rows for missing files. Rebuild is idempotent, and loss of `harness.db` loses no
memory content.

**Score normalization:** SQLite FTS rank/BM25 values are ordering signals, not values on
the same scale as cosine similarity and recency. Never add raw `rank` to the blend.
Order matching candidates best-first and map their ordinal rank to `[0,1]`:

```go
ftsScore = 1                                      // one matching candidate
ftsScore = 1 - float64(rank-1)/float64(matches-1) // otherwise; rank is one-based
```

Non-matches receive zero. Extend the blend with the normalized value:

```go
score = sw*similarity + rw*decay + fw*ftsScore
```

`fts_weight` remains **0 by default** for the entire MR2 rollout, preserving existing
behavior. D3 determines a recommended opt-in weight. Changing the product default to a
nonzero value is a separate, evidence-backed config decision; this roadmap does not both
promise zero and infer another default.

**Trace row addition:**

```go
FTS       float64 // normalized score in [0,1]
FTSWeight float64 // configured weight at call time
```

**Package:** `internal/fts` owns insert/delete/search/rebuild. `internal/retrieval` keeps
the blend pure and receives FTS candidates through an injected interface.

**Acceptance gates for MR2:**
- MR0 contains a documented keyword-miss cohort that weight tuning did not fix; otherwise
  MR2 is explicitly skipped.
- Rebuild is idempotent and removes stale rows.
- Project switching and equal relative paths prove there is no cross-project leakage.
- `fts_weight = 0` produces byte-for-byte identical ranking to the pre-MR2 blend.
- Raw FTS rank is never blended; normalized scores are bounded and tested.
- With an opt-in nonzero weight, D3 improves Precision@3 or Recall@3 on the documented
  keyword cohort without regressing the overall agreed threshold.
- Deleting `harness.db` and rebuilding from the project repos loses no memory content.

---

## MR3 — Persistent semantic-write gate (C1)

**Gated on:** MR1 complete and MR2 complete or explicitly skipped.

**Scope:** one gate owns semantic records created by session summaries, fact promotion,
agent notes, and `memory_propose`. It does **not** gate `sessions.jsonl`, conversation
sidecars, vector/FTS indexes, cache files, scaffolding, or index rebuilds. Those are
session evidence, operational state, or derived projections rather than semantic claims.

The external VC tools' `Result.Proposal` boolean is not a dependency: it creates no
persistent proposal state and never executes an approved action. MR3 defines its own
idempotent proposal/decision workflow and UI surface.

**Authoritative storage in each project memory repo:**

```
memory_events.jsonl              # append-only state transitions
proposals/<proposal-id>.md       # immutable proposed payload, including rejected/held
```

SQLite may hold a rebuildable projection for joins and UI queries. The git repo remains
the backup and audit boundary.

**IDs and events:** `proposal_id`, `record_id`, and `event_id` are separate opaque IDs
generated once from cryptographic randomness. Content hashes detect equality; timestamps
do not create identity. Re-proposing identical text later creates a new proposal and, if
accepted, a new record.

One event per line:

```json
{
  "event_id": "01...",
  "proposal_id": "01...",
  "event": "proposed",
  "record_id": "01...",
  "kind": "fact",
  "target_path": "facts.md",
  "origin": "inference",
  "payload_path": "proposals/01....md",
  "content_hash": "sha256:...",
  "decision": null,
  "supersedes": [],
  "at": "2026-07-26T12:00:00Z"
}
```

Allowed events are `proposed` and `decided`. Decisions are `accept`, `reject`, or `hold`.
`hold` may later transition to `accept` or `reject`; any other second terminal decision is
rejected as non-idempotent. The latest valid decision is derived by replay.

`supersede` is not a verdict. An accepted decision may carry `supersedes: [record_id...]`;
the referenced older records then have derived state `superseded`. This separates the
decision on the new proposal from the lifecycle state of old records and makes cycle
checking well-defined.

**Record addressing:** episode files already map one path to one semantic record. The
aggregate `facts.md` and `agents/<name>/notes.md` files need machine-addressable records
before supersession can work. MR3 prefixes each fact/note paragraph with a non-rendered
marker:

```markdown
<!-- harness-memory-record:01... -->
The user prefers table-driven tests.
```

Memory readers strip markers from prompt/UI text. A one-time migration assigns IDs to
existing blank-line-delimited paragraphs and appends accepted baseline events in one
explicit git commit. Hand-edited unmarked paragraphs are surfaced for import; they are not
silently granted accepted/extraction state.

**Commit protocol:**

- Auto-accepted session and UI writes append `proposed` and `decided(accept)` events,
  store the immutable payload, mutate the target memory file, and commit those paths in
  one git commit.
- Held/rejected proposals commit the payload and audit events but do not mutate the target
  memory file. “Reject does not write” means no target-memory mutation; the audit write is
  intentional.
- Approving a held proposal appends the accept decision and mutates the target in one
  later commit.
- There is no mutable `committed_at` field. The git commit containing the decision and
  target mutation is the commit evidence. Failed commits roll back the working tree;
  startup reconciliation detects and surfaces any interrupted dirty state.
- Gate operations serialize per project repo and are idempotent by `proposal_id`.

**Writer migration:** session summaries and UI promotion call the gate instead of writing
semantic files directly. Existing dedup runs before the decision. After-save embedding and
FTS projection run only after the accepted semantic commit and remain outside the gate.

**`memory_propose`:** validates a semantic proposal, stores it through the gate, and
returns its persistent proposal ID. Deterministic rules may accept or reject; ambiguous,
duplicate, or possible-conflict cases hold for the Memory UI. The agent never receives a
filesystem bypass.

**Acceptance gates for MR3:**

- Every semantic writer uses the gate; supporting/derived writes do not emit fake semantic
  decisions.
- Proposed payloads survive restart for accept, reject, and hold decisions.
- Reject/hold never mutate target memory; accept commits payload, events, markers/content,
  and decision atomically.
- Repeating a proposal decision is idempotent; conflicting terminal decisions fail.
- A fresh clone replays full proposal, decision, record, origin, and supersede state with
  no `harness.db` present.
- Aggregate fact/note records have stable IDs, and prompt rendering strips markers.
- Supersede references exist, stay project-local, and form an acyclic graph (D4).
- Crash tests cover failure before commit, commit failure/rollback, and startup recovery.

---

## MR4 — Supersede-aware reads + possible-conflict surfacing

**Gated on:** MR3 complete. Before claiming a quality improvement, the labeled set must
include superseded/live examples for every record kind ranked by retrieval. Static
fact/note filtering is validated with deterministic replay and prompt fixtures, not by
pretending an episode-only D3 set measures an unranked path.

**Live-record reads:** replay `memory_events.jsonl` (or its disposable SQLite projection)
to derive each record's current state. Both `memory_query` and prompt assembly exclude
rejected and superseded records by default. This is essential for facts/notes because
their physical markdown remains append-only even when an older record is superseded.

`memory_query` gains an `include_superseded` JSON boolean, default false. The Memory UI
gets the equivalent inspector toggle. There is no Harness CLI flag.

**Possible-conflict surfacing:** embedding similarity identifies related candidates; it
does not prove logical contradiction. A high-similarity candidate may cause a proposal
to hold for review, but the UI labels the pair “possible conflict.” Only deterministic
code rules over explicit structured fields may call something a contradiction. No model
judgment autonomously supersedes an accepted record.

Candidate trace rows gain current record state:

```go
RecordState string // accepted | rejected | held | superseded | untracked
```

**Acceptance gates for MR4:**

- Superseded/rejected records appear in neither default `memory_query` output nor prompt
  assembly; accepted live records do.
- Deterministic fixtures validate live-view filtering for static facts and notes, including
  fresh replay with no SQLite projection.
- `include_superseded: true` and the UI toggle expose historical records without changing
  default prompt behavior.
- Similarity can hold and surface a possible-conflict pair but never labels or resolves a
  contradiction by itself.
- Acyclic/project-local supersede checks run in CI and at event replay.
- D3 includes labeled supersession cases across the ranked record kinds and shows the
  agreed Precision@3 improvement without violating the overall regression threshold.
- Trace rows capture record state at query time so the result is reproducible.

---

## Deferred (requires D3 evidence before scheduling)

**RRF fusion:** Reciprocal Rank Fusion over N ranked lists. Only meaningful when N ≥ 3
signals exist (semantic, FTS5, recency). Conditional MR2 brings it to 3. Schedule only
after D3 confirms weighted normalized signals remain insufficient.

**Spreading-activation retrieval:** Graph traversal over typed edges as a fourth signal.
Requires a typed-edge schema in the git repo or event log — a real schema addition.
Schedule only after multi-hop recall failure is confirmed in D3 traces.

**Stable URI addressing:** MR3 assigns separate stable record IDs. Exposing those IDs as
`memory_query` read addresses rather than project-relative paths is low-cost after MR3;
schedule it when retrieval and the tool layer need direct record addressing.

**Cross-run failure aggregation (D1):** Grouping failures across runs into evidence-
backed issues. The episode commit history in git has the raw material; the event log
adds structured provenance. Schedule after MR3 when both sources are queryable.

---

## Non-goals (permanently out of scope)

- **Self-modifying memory schema:** No memory milestone may rewrite the event-log
  schema autonomously. Schema changes are code changes with a migration.
- **Autonomous supersede/conflict judgment:** Similarity and code rules may surface or
  hold candidates, but no model judgment autonomously supersedes an accepted record.
- **Python dependencies:** FTS5 is SQLite built-in; the eval binary is Go. No external
  process for any memory operation.
