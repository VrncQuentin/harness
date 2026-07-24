# Memory Roadmap — M12

Milestone position: **this document is M12**, the memory layer that follows the M10 tool
surface (tool_roadmap.md). It plans the evolution of Harness memory from its current
shape toward the architecture outlined there (C1, C2, C3, `memory_query`,
`memory_propose`).

Sequencing against the other milestones:

- **MR0 is not part of M12 proper — it is M10.3.** One implementation satisfies both
  documents. The D3 labeled query set accumulates during M10.1–M10.2, before any M12
  phase starts.
- **MR1 and MR2** gate only on MR0's numbers; they may run in parallel with M10.4 and
  with M11 (disjoint packages — retrieval/memory vs. git/gh/tools vs. dsl/pipeline).
- **MR3** additionally waits for M10.4, because `memory_propose` reuses the
  proposal-return-type pattern the external VC tools introduce.
- **MR4** follows MR3. M11 neither depends on nor blocks any M12 phase.

Every phase is gated on the previous one completing and on instrumentation from
D3/MR0 confirming the direction. **No memory schema work starts before MR0 reports.**

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

**Write paths (4 distinct, no gate):**
1. Session lifecycle: summarize → write episode → commit (via `internal/session` + `internal/memoryops`)
2. UI promotion: `PromoteFact` / `AppendAgentNote` (via `internal/memory.PromotionService`)
3. Index update: embed-on-commit, triggered by AfterSaveEmbed (via `internal/memoryops`)
4. Index rebuild: UI-triggered full walk (via `internal/memoryops.EpisodeRebuilder`)

**Dedup:** embedding cosine similarity against existing `facts.md` lines before
`PromoteFact` commits — no dedup on episodes or notes.

**Origin classes (M10.1, shipped):** the tool layer records a C3 origin class on tool
results — `tools.OriginClass` with values `extraction` (parser-backed output) and
`inference` (model-generated content). Memory records do not carry it yet; that is MR2.

**What does not exist:** stable IDs, supersede chains, trust/origin scoring on memory
records, contradiction flags, commit verdicts, FTS5 index, cross-episode or cross-file
retrieval.

---

## Instrumentation gate — MR0 (= M10.3)

**Prerequisite for everything else. No memory schema work starts before this reports.**

MR0 **is** tool_roadmap.md's M10.3 — one implementation, referenced by both documents.
It lives in `internal/retrieval` and `internal/memoryops`; no new runtime package, no
schema change. (The eval binary below is a separate developer-side tool, not part of
the harness binary.)

**What to add to `ScoreEpisodePaths`:**

Emit one trace row per candidate per call:
```go
type RetrievalTrace struct {
    QueryID   string    // hash of query text; no raw query text or PII in the row
    Candidate string    // episode path
    Semantic  float64   // raw similarity (0 if embedder miss)
    Recency   float64   // raw exp_decay value
    SWeight   float64   // semantic_weight at call time
    RWeight   float64   // recency_weight at call time
    Score     float64   // final blended score
    Returned  bool      // was this candidate in the top-K returned?
}
```

Emitting at `ScoreEpisodePaths` — the shared choke point — measures every retrieval,
both the prompt-assembler path and the `memory_query` tool, which inherits emission by
construction. This is deliberate: an emitter inside `memory_query` alone would leave
production (assembler-path) retrieval unmeasured.

Storage: append-only NDJSON under `~/.harness/logs/retrieval/<date>.ndjson`. Not in
the memory repo, not in SQLite. Log rotation on date boundary; keep 30 days.

**Measurement harness (D3):**

A labeled query set: JSON files under `~/.harness/eval/retrieval/`, one query per file:
```json
{
  "query": "the Go AST package discussion",
  "expected": [
    "episodes/coder/2025-01-15T10:30:00Z.md",
    "episodes/coder/2025-01-14T09:00:00Z.md"
  ]
}
```
(JSON, not YAML: the standard library parses it, and the no-new-dependency rule holds.)

Labels accumulate from real work during M10.1–M10.2 (start building them now, before
the harness runs any nontrivial tool calls). The eval binary reads the labeled set,
replays each query against the live index, and reports precision@k and recall@k per
signal and combined. The binary is `cmd/eval-retrieval/main.go`, separate from the
harness binary — a deliberate exception to the no-CLI rule: it is a developer-side
measurement tool, never shipped to users, and the harness itself gains no CLI.

**Acceptance gates for MR0:**
- Every `ScoreEpisodePaths` call emits a trace row; the row schema matches the struct above.
- The eval binary runs against a 10-query labeled set and produces precision@3 and recall@3.
- D3 output exists before MR1 design is finalized.

---

## MR1 — FTS5 retrieval signal

**Gated on:** MR0 showing episodes are being missed by the semantic signal on
keyword-heavy queries (tool names, function names, error messages). If MR0 shows
semantic alone is sufficient across the labeled set, skip MR1.

**What:** Add SQLite FTS5 as a third retrieval signal over episode text. Episodes are
already markdown text; FTS5 indexing is a projection, not a schema change to the
authoritative git content.

**Storage:** New table in `harness.db` (derived, rebuildable from the git episode files):
```sql
CREATE VIRTUAL TABLE episode_fts USING fts5(
    path,              -- stored: hits must map back to episode files
    content,
    tokenize="porter ascii"
);
```
`path` and `content` are deliberately **stored**, not contentless. A contentless table
(`content=""`) stores no column values at all — even `UNINDEXED` ones — so matches
cannot be mapped back to episode paths and `DELETE … WHERE path = ?` is a silent no-op
(verified against the pinned `modernc.org/sqlite` v1.34.1, which otherwise supports
FTS5 fully: create, `MATCH`, `rank`, and the `porter ascii` tokenizer). Duplicating
episode text inside `harness.db` is the accepted cost; the index stays a derived view.

The FTS index is a derived view of the git repo. It is never authoritative. Rebuild it
from the git memory repo the same way the vector index is rebuilt: walk episodes, insert
missing, delete stale. Rebuild on startup (fast, FTS5 is in-process) and after each
episode commit.

**Retrieval change:** extend `BlendEpisodeScores` to accept a third signal:
```go
score = sw*similarity + rw*decay + fw*fts_rank
```
where `fw` (fts_weight) defaults to 0 and is tunable in `PromptConfig`. This makes
FTS5 additive and backward-compatible: default config is unchanged, users opt in by
setting `fts_weight > 0`.

**Trace row addition:**
```go
FTS   float64   // raw FTS rank (0 if not indexed or no match)
FWeight float64 // fts_weight at call time
```

**MR0 D3 data determines the default weight.** Do not guess.

**Package:** `internal/fts` — FTS index open/close/insert/delete/search. Separate from
`internal/retrieval` so the blend function stays pure and the FTS store is injected.

**Acceptance gates for MR1:**
- FTS index rebuilds from the git episode files; rebuild is idempotent.
- `fts_weight = 0` (default) produces identical retrieval output to the pre-MR1 blend.
- D3 eval shows improvement on keyword queries with `fts_weight > 0`.
- `harness.db` loss causes no data loss (episode content is in the git repo; FTS is reconstructed on next startup).

---

## MR2 — Origin class (C3 slice)

**Gated on:** MR0 complete. Does not depend on MR1. Can run in parallel with MR1
if MR1 is in progress, but MR2 is lower-effort and probably should land first.

**What:** Attach an origin class to every record the retrieval layer surfaces. The
classes and their spellings are the ones already shipped in the tool layer
(`tools.OriginClass`, M10.1) — one vocabulary across the codebase:

- `extraction` — content derived deterministically from source artifacts (AST output,
  committed file content). The governor trusts this without re-verification.
- `inference` — content produced by model inference (episode summaries, promoted facts,
  agent notes). Must clear verify/gate before being acted on.

**Implementation:** Episodes are always `inference` (the session summarizer produced
them). Promoted facts are `inference`. The origin class is carried on the retrieval
result, not stored in the markdown file.

Short-term: derive the class per candidate at scoring time and carry it on the
retrieval result record alongside the score (today `ScoreEpisodePaths` returns a bare
score map; MR0's trace work introduces the per-candidate record this field rides on).
Derivation is path-based: `episodes/*` and `facts.md` → `inference`, everything else
defaults to `inference` until `extraction`-class content exists. The interesting
transition is when `ast_map` output starts being stored — that content earns
`extraction`. Until then, the field is present and populated, even if both classes
point to the same treatment.

**Trace row addition:**
```go
Origin string  // "extraction" | "inference"
```

**Acceptance gates for MR2:**
- Every retrieval result carries an Origin field.
- Path-based derivation rule is tested and documented.
- The governor's tool-result handling reads the Origin field and routes accordingly
  (this is M10 work in `internal/agentloop` / governor layer, not memory work).

---

## MR3 — Commit gate (C1)

**Gated on:** MR0 complete **and M10.4 shipped**. `memory_propose` is built **here**,
together with the gate — tool_roadmap.md defers the tool to this milestone, and the
gate is what gives it meaning. It reuses the proposal-return-type pattern the M10.4
external VC tools introduce (a proposal the human approves is the only path to a
side effect), which is why MR3 cannot land before M10.4.

**What:** Consolidate the four present write paths behind a single commit gate that
emits a verdict for every write.

Verdicts: `{accept, reject, supersede, hold}`
- `accept` — write proceeds as proposed.
- `reject` — write is refused; the rejected proposal is stored as a verdict entry for
  provenance.
- `supersede` — write replaces an existing record; the old record is marked
  superseded, not deleted.
- `hold` — write is deferred for human review.

**Storage:** an append-only verdict log in the project memory repo, committed like
`sessions.jsonl`:

```
~/.harness/projects/<slug>/verdicts.jsonl
```

One JSON object per line:
```json
{
  "id": "…",             // sha256(path + content + proposed_at) — unique per proposal event
  "path": "facts.md",     // repo-relative path
  "verdict": "accept",    // accept|reject|supersede|hold
  "supersedes": null,      // id of the record this supersedes (null when none)
  "origin": "inference",   // extraction|inference
  "proposed_at": "…",    // ISO8601
  "committed_at": null,    // ISO8601, null until the write lands
  "content_hash": "…"    // sha256 of the proposed content
}
```

The log is **authoritative and lives with the memory it governs**: the git memory repo
remains the single thing to back up, satisfying C1's append-only audit contract, and
`harness.db` keeps the disposability property MR1's acceptance gate relies on. The
harness may maintain a derived, rebuildable index of the log in `harness.db` for joins;
losing that index costs nothing.

The `id` includes `proposed_at` because each proposal is a distinct event: the same
content re-proposed at the same path after a supersession (a fact deleted, then
re-learned verbatim months later) must not collide with its earlier incarnation.

**Migration:** The four existing write paths each become callers of the gate. They
continue to do the same filesystem + git operations; the gate wraps them and appends a
verdict entry. No protocol change for the existing session lifecycle or promotion paths
during MR3 — the gate is additive.

**`memory_propose` (agent write path):** The only write path the agent can touch goes
through the gate. The agent never calls filesystem operations directly. The gate
evaluates the proposal (dedup check, origin class, supersede detection) and emits a
verdict. On `hold`, the proposal surfaces to a human before the write proceeds.

**Acceptance gates for MR3:**
- Every write — from any of the four paths plus `memory_propose` — appends a verdict entry.
- `reject` does not write to the repo. `supersede` marks the previous entry's record,
  does not delete the file.
- The verdict log is committed to the memory repo; a fresh clone reproduces full
  verdict state with no `harness.db` present.
- Supersede chains are acyclic (CI check, D4).

---

## MR4 — Supersede chains + contradiction surfacing

**Gated on:** MR3 complete. D3 data must be present; improvement in retrieval
precision from supersede-aware filtering is the confirmation gate.

**What:** Extend retrieval to use the verdict log.

- Retrieval filters out `rejected` and `superseded` records by default.
- Retrieval surface exposes a `--include-superseded` flag for the human inspector.
- Contradiction detection: when a new `memory_propose` arrives, embed the candidate
  and compare against recent `inference` records above a similarity threshold. If a
  semantic conflict is detected, the verdict for the new record is `hold` pending
  human review, and the conflicting pair is surfaced.

**Retrieval change:** `memory_query` consults the verdict log (or its derived index)
to filter candidates before scoring. The trace row gains:

```go
Verdict  string  // "" if no entry, else the verdict at query time
```

**Acceptance gates for MR4:**
- Superseded records do not appear in default retrieval output.
- Contradicting a live fact causes the new proposal to `hold`.
- D3 shows measurable precision improvement over MR3 baseline.

---

## Deferred (requires D3 evidence before scheduling)

**RRF fusion:** Reciprocal Rank Fusion over N ranked lists. Only meaningful when N ≥ 3
signals exist (semantic, FTS5, recency). MR1 brings it to 3. Schedule after D3 confirms
that per-signal weighting is insufficient and rank ordering across lists matters.

**Spreading-activation retrieval:** Graph traversal over typed edges as a fourth signal.
Requires a typed-edge schema in the git repo or verdict log — a real schema addition.
Schedule only after multi-hop recall failure is confirmed in D3 traces.

**Stable URI addressing:** Provenance IDs serving as the read-path address for
`memory_query`. The verdict log in MR3 already assigns each proposal a stable id
(`sha256(path + content + proposed_at)`), which serves as a URI. The step is exposing
it as the retrieval address rather than the file path. Low-cost after MR3; schedule
when retrieval and the tool layer need it.

**Cross-run failure aggregation (D1):** Grouping failures across runs into evidence-
backed issues. The episode commit history in git has the raw material; the verdict log
adds structured provenance. Schedule after MR3 when both sources are queryable.

---

## Non-goals (permanently out of scope)

- **Self-modifying memory schema:** No memory milestone may rewrite the verdict log
  schema autonomously. Schema changes are code changes with a migration.
- **Autonomous supersede promotion:** The gate decides `supersede` verdicts based on
  code-written rules, not a model judgment without a human in the loop.
- **Python dependencies:** FTS5 is SQLite built-in; the eval binary is Go. No external
  process for any memory operation.
