# Harness Tool Roadmap — M10

Milestone position: **this document is the M10 design record**, between M9 (Layout V2,
shipped) and M11 (Pipeline DSL, planned). M10.1, M10.2, and M10.4 have shipped. M10.3
code has landed, but its MR0 acceptance gate remains open: trace-sink failures are silent,
the sink is never closed, the trace and evaluation contracts are incomplete, and no
ten-query baseline has been recorded.
M11 binds tool calls against this surface, so the M10.3 closure work is part of making
the surface observable rather than a reason to rename tools again.

This supersedes the standing deferral of file edit/patch (deferred at M4 and again
beyond M7 in roadmap.md): `edit` lands in M10.1.

> The **current tool surface** — the registry, descriptors, every built-in tool, the
> approval mechanism, the sandbox, output provenance, and the checklist for adding a
> tool — is documented in [tools.md](tools.md). This roadmap records only the remaining
> planned work, implementation decisions that still constrain future work, and
> acceptance criteria. Where a claim about present behavior appears here, the
> canonical statement lives in tools.md.

Every claim is tagged:

- **[P]** Present — true of the repo today.
- **[S]** Shipped — implemented by M10 and present in the repo today.
- **[R]** Repair — implementation exists, but the stated acceptance contract is not yet met.
- **[X]** External — belongs to another roadmap (memory), referenced but not built here.

The X tags exist because the memory layer needs its own roadmap and its own evaluation
method before schema work starts — the retrieval instrumentation in D3 is the first piece
of that evaluation method and is deliberately built early so memory changes are measured
rather than assumed beneficial. That roadmap now exists: [memory_roadmap.md](memory_roadmap.md)
(M12), whose MR0 gate is this document's M10.3.

---

## Naming convention [S]

Tool ids are sent verbatim as function names on the inference path, which enforces the
charset `^[a-zA-Z0-9_-]{1,64}$`. Dots are out of spec; kebab-case is legal but the
existing surface is snake_case. Therefore: **snake_case everywhere**, prefix separated by
underscore.

Three classes. The prefix encodes capability, not language.

- **`ast_*`** — backed by a real parser. Deterministic, no model involvement. Output is
  extraction-class provenance by construction. Each tool declares its supported
  languages, and that declaration is generated from the registered parser front-ends at
  construction time — never hand-written. Registering a front-end with no working parser
  fails startup.
- **`<toolchain>_*`** — wrappers over an external toolchain: `go_*`, `git_*`, `gh_*`.
  Deterministic but not parser-backed; output shape is toolchain-specific, so the
  toolchain belongs in the name.
- **unprefixed** — native operations: raw-text, process, retrieval, memory.

Adding a language to `ast_*` means adding a front-end, not tools. Adding a toolchain
means adding tools, because the output shape differs.

The current inventory (all ids in `BuiltinDescriptors`) is in [tools.md](tools.md);
the table below lists only the tool surface items that are still planned, repairing, or
belong to another roadmap.

| Tool | Class | Status | Remaining work |
|---|---|---|---|
| `memory_query` | native | S / R | Retrieval entry point is shipped. Its production trace/evaluation contract remains MR0 closure work — see D3. Richer return records are [X]: they require the memory data model. |
| `memory_propose` | native | X | Write path through M12's persistent memory proposal/decision gate. It does not reuse the manual-action `Result.Proposal` boolean. |

---

### `exec` and the approval contract [S]

Commands are argv arrays, not `sh -c` strings. This drops pipes, globs, and redirection;
if that proves too limiting in practice, shell-string mode can be revisited as a
deliberate decision — argv is the safer and simpler starting point, not a permanent vow.

The allowlist is a **deny filter, not an authorization**. It rejects known-bad argv
outright; anything that survives still goes through the existing per-call approval
prompt (`internal/approvals`). This preserves the shipped M7 decision that no remembered
allow ever lets a command skip the per-call prompt. The allowlist shrinks the gray zone;
the human remains the security boundary.

### Structural enforcement of recon-first [S]

Edits to existing files require an anchor hash emitted by the parser-backed discovery
tools. Whole-file mode is reserved for new-file creation. The model cannot safely skip
recon because it cannot fabricate a matching anchor. This is encoded in the tool schema,
not left as prompt guidance.

### Toolchain preconditions [S]

The Harness targets machines that build Go — the `go` binary is a stated precondition,
not a hidden dependency: an environment that cannot build the agent's output is not a
target environment. `golangci-lint` (v2) is the same class of precondition: `go_lint`
invokes the binary as a subprocess and returns a clear tool error when it is absent.
It is deliberately **not** imported as a library — golangci-lint is GPL-3.0, so linking
it would place the shipped binary under copyleft; it would also graft its ~230-module
require graph onto `go.mod` and lean on an API surface the maintainers explicitly do
not support. Subprocess invocation carries none of those costs.

### Version control tools [S]

Tiered by reversibility. The tier determines the gate.

| Tier | Tools | Treatment |
|---|---|---|
| Read | `git_status`, `git_diff`, `git_log`, `gh_pr_wait` | Agent-callable, no gate. High-value B2 folder targets. |
| Local write | `git_commit`, `git_branch`, `git_checkout` | Agent-callable, scope-checked, gated. Undo via recorded ref SHA. |
| External | `git_push`, `gh_pr_create`, `gh_pr_merge` | Never autonomous. Validates inputs and emits a manual-action proposal; it performs no network mutation. |

Each tool is atomic. Compound sequences (merge → return to main → delete branch) are not
collapsed into single tools: a compound tool with an irreversible step in the middle has
no correct return value for partial failure. Sequencing belongs above the tool layer —
in M11's DSL runner once it can call tools, and until then in the agent loop under
per-step gates.

#### Manual-action proposal contract [S]

`git_push`, `gh_pr_create`, and `gh_pr_merge` do not implement an approve-and-execute
workflow. They return explanatory command text with `Result.Proposal = true`; the agent
loop currently injects that result like any other tool result and the user executes the
action outside Harness. The boolean is metadata, not a persisted approval state machine.

This distinction matters for future consumers: M11 may surface these results more richly,
but must not treat them as completed actions, and M12's memory commit gate must define its
own persistent proposal/decision workflow rather than claiming to reuse this boolean.

#### `gh_pr_wait` [S]

Read-only CI poller over the GitHub Checks API with exponential backoff under the loop's
cancellation context; red carries the failing check names and log handles. Two guards
keep a blocking tool honest inside an agent loop: a configurable wait ceiling so it
cannot outlive the task, and an expected-blocking flag in its schema so loop watchdogs
distinguish a legitimate long wait from a hung tool. It closes the tier-3 workflow as the
only non-proposal step in it: `gh_pr_create` (proposal) → `gh_pr_wait` → `gh_pr_merge`
(proposal). Behavior details live in [tools.md](tools.md).

### Repo scoping [S]

Memory repositories are also git — **one per project, paths in the projects table,
optionally user-supplied**. There are two go-git consumers with disjoint scopes:
`internal/git` exists today with the *memory repos* as its charter (init, open, commit
specific files, harness fallback identity), driven by the session lifecycle. The
agent-facing `git_*` tools are the second consumer, scoped to workspace repos only. A
`git_*` tool that can resolve into a memory repo would let the agent write memory outside
the lifecycle path, silently voiding the audit direction the memory roadmap is heading
toward.

Therefore scoping is a **predicate evaluated at call time, not a config path list**: a
`git_*` call is rejected if its resolved repository root is the memory repo of any
project row. Workspace targets are the active project's sandbox roots — already a list
enforced by the tool sandbox, so `git_*` tools take an explicit root argument rather than
assuming a singular workspace.

Package shape: `internal/git` remains the single go-git wrapper and grows the workspace
operations; the scope predicate lives at the tool boundary in `internal/tools`, not in
the wrapper — the wrapper stays policy-free so the memory writer can keep using it.

### Undo for tier 2 [S]

Two records per tier-2 write, different jobs:

- **Provenance ref SHA — authoritative.** Pre-operation ref SHA recorded before every
  tier-2 write. This is the recovery path the Harness relies on; it does not depend on
  git-library behavior.
- **Reflog entry — ergonomics.** go-git v6 (now pinned in go.mod) exposes reflog append
  through the storer, writing `.git/logs/<refname>` in the standard format, so the git
  CLI reads it and `git reset --hard HEAD@{1}` behaves as a human expects. No porcelain
  operation in the library appends to the reflog — every ref update bypasses it — so the
  Harness wires this itself, using the library's encoder (a human's CLI shares that
  file; malformed entries corrupt it). v6 is alpha: pin the exact tag and re-verify the
  storer surface on each bump. The library has no `gc`, so nothing expires these logs.

### Tool admission rule [S]

A new tool must belong to an existing class and must not overlap the charter of an
existing tool in that class. A new class requires architecture-level justification of the
same weight as admitting an MCP server. Version control is admitted as a new class
deliberately.

---

## B. Governor-side transforms [S]

**Naming:** the Governor is this component — the tool-result and context layer between
execution and context assembly. The run-limit / loop-detection concept currently called
"governor" in DSL.md is a separate entity with separate responsibilities and gets its own
name (**watchdog** proposed); DSL.md is amended when M11 touches it. One name, one
component, in both documents.

Not model-callable. These fire on results the model never sees raw. The shipped
transforms (B1, B2, B3, B5) are documented in [tools.md](tools.md); the table below
lists only the deferred or contracted ones.

| # | Transform | Notes |
|---|---|---|
| B4 | Observation mask | Deferred; stale-output masking needs real long-running task data before admission. |
| B6 | MCP result normalizer | Deferred — no MCP servers admitted. If admitted, results enter this same chain. No bypass path. |

Shipped package homes are `internal/governor` for B1/B2/B3/B5,
`internal/parser` for language front-ends, `internal/git` for go-git operations, and
`internal/tools` for the callable surface including GitHub proposals/polling.

---

## C. Memory commit layer [X — memory roadmap]

Everything in this section presumes origin-aware retrieval records, stable record IDs,
append-only proposal/decision events, and supersede relations — **which do not exist**.
Memory today is markdown + `vectors.bin`/`manifest.json` in per-project git repos, with
SQLite holding config/metrics/projects. The current memory system is documented in
[memory.md](memory.md).

These are therefore target contracts for the memory roadmap, recorded here so the tool
layer doesn't contradict them:

- **C1 — semantic-write gate.** Session summaries, facts, notes, and `memory_propose` use
  a persistent project-local proposal/decision workflow. Decisions are
  `{accept, reject, hold}`; supersession is a relation on an accepted record, not a
  verdict. Immutable payloads and `memory_events.jsonl` are committed to the project repo.
- **C2 — hard-lock predicate.** The memory-repo scoping predicate above **is the M9
  slice of C2** and ships in M10 phase 2 with the tier-2 git tools — it needs only the
  projects table, not the record model. The fuller hard-lock set follows the memory
  roadmap.
- **C3 — origin class.** The M10 slice records how tool output was produced. M12 MR1 adds
  the origin of each underlying memory hit, fails unknown origins closed to inference,
  and treats origin as metadata rather than authorization.

The sequencing rule: **no memory schema work starts before M11 and before MR0 reports**
under the current one-milestone-at-a-time policy.

---

## D. Offline passes

| # | Pass | Status | Notes |
|---|---|---|---|
| D3 | Retrieval quality harness | R | Binary, startup sink wiring, and trace types exist, but sink error/close handling, separate versioned trace and label schemas, and the ten-query baseline are still required. |
| D1 | Failure aggregator | Deferred | Groups recurring failures across runs into evidence-backed issues, surfaced to a human. No auto-apply. |
| D2 | Replay-as-regression | X | Requires run material in a queryable store — depends on what the memory roadmap decides to persist. Not "already held" anywhere today. |
| D4 | Invariant CI checks | Deferred | Scope-predicate completeness, allowlist deny-by-default. Plain Go tests. Supersede-chain acyclicity joins when supersede chains exist [X]. |

### D3 / MR0 closure contract [R]

Retrieval today is a two-signal weighted blend:
`semantic_weight * similarity + recency_weight * exp_decay`. Startup already installs the
trace sink when construction succeeds. MR0 is not complete until construction/emission
failures are surfaced, shutdown closes the sink, and each artifact has its own versioned
contract: runtime/tests/docs share the trace schema, while evaluator/tests/docs share the
separate labeled-query schema.

```
{
  version,
  record_type,           // "call" | "candidate"
  project_slug,
  query_id,              // full SHA-256 hex; never the raw query
  candidate_id,          // project-relative episode path on candidate rows
  semantic,
  recency,
  semantic_weight,
  recency_weight,
  final_score,
  rank,                  // one-based final-score rank
  returned,              // selected into the configured top-K
  outcome,               // scored | unscoreable | error on call rows
  timestamp
}
```

Each invocation emits one call row. A scoreable invocation with candidates additionally
emits one candidate row per candidate; an unscoreable or failed invocation records its
outcome without raw query text or error payloads. Emission happens at
`retrieval.ScoreEpisodePaths`, so the prompt-assembler and `memory_query` paths are both
measured. Both callers pass trace context containing `project_slug` and their requested
top-K; this namespaces otherwise-identical paths and gives `returned` one unambiguous
meaning.

The canonical labeled set is one NDJSON file per project at
`~/.harness/eval/retrieval/<project-slug>.ndjson`:

```json
{"version":1,"query":"the Go AST package discussion","relevant":["episodes/coder/2025-01-15T10:30:00Z.md"]}
```

The evaluator reports Precision@3 and Recall@3 for semantic-only, recency-only, and the
configured blend; MRR may be reported as an additional diagnostic. It rejects fewer than
ten valid labeled rows for the MR0 baseline mode and writes a machine-readable result
under `~/.harness/eval/retrieval/results/`. The full acceptance gate is specified once in
memory_roadmap.md MR0.

D3 is the evaluation method the memory roadmap is gated on.

---

## Build order — M10 phases

**M10.1 — auditable edit loop**
`ast_map` (single-file tier), `ast_find`, `read` (replaces `file_read`), `edit` (replaces
`file_write`), `git_status`, `git_diff`, `git_log`, B1, B3, C3 M10-slice.
Label collection was intended to begin here, but no accepted baseline was recorded.

**M10.2 — execution, compression, local VC writes**
`exec` (replaces `shell_exec`, allowlist as deny filter + existing approval prompt),
`go_test`, `go_lint`, `git_commit`, `git_branch`, `git_checkout`, B2, B5, **C2 predicate,
ref-SHA recording, reflog wiring** — the write tools and their scoping/undo ship
together; write tools first and lock later is the one ordering that must not happen.
M10.3 closure now owns collecting the first ten real labels and recording the baseline;
implementation checkboxes do not imply those observations happened.

**M10.3 — retrieval instrumentation (implementation landed; MR0 closure pending)**
`memory_query`, trace types at `ScoreEpisodePaths`, startup sink installation, and the D3
binary have landed. Sink error/close handling, separate versioned trace and label schemas,
evaluator alignment, a ten-query labeled set, and a recorded baseline remain required.
This phase **is** memory_roadmap.md's MR0 gate and is not accepted until those items pass.

**M10.4 — external VC**
`git_push`, `gh_pr_create`, and `gh_pr_merge` as manual-action proposals, plus
`gh_pr_wait` closing the create → wait-green → merge workflow. GitHub
token from environment variable only — never persisted, never in config, never rendered
by `/config`, never in model context. D1 lands here or after M11, whichever the failure
volume justifies.

**Then M11** — Pipeline DSL runner (already specced in roadmap.md/dsl_roadmap.md),
binding tool calls against this surface; M10 joins M7 and M9 as its dependencies. Under
the one-milestone-at-a-time rule, M10.3/MR0 closure completes before M11 begins; M12 then
starts only after M11 and consumes the recorded MR0 baseline as evidence.

**Deferred:** B6, D2, D4's supersede check, `memory_propose`/C1 (memory_roadmap.md MR3),
and the memory roadmap itself (memory_roadmap.md, M12) — whose schema work starts only
after M11 under the current one-milestone-at-a-time policy and after MR0 produces numbers.

---

## Deferred pending real usage

Additional retrieval signals beyond the present two-signal blend — reranking
(cross-encoder: second resident model in remaining VRAM, or ONNX and the CGO question),
graph/associative retrieval (requires typed weighted edges, a real schema addition).
Both gated identically: D3 reports a deficiency the free fix (weight tuning on existing
signals) did not close, **and** the deficiency appears in traces from real work, not the
labeled set alone.

---

## Implementation decisions: version control

**go-git v6** (pinned in go.mod; alpha — pin exact tag, re-verify on bump). Supported and
sufficient for the tier list: status, diff, log, show, blame, grep, add, commit, reset,
rm, mv, clean, branch, checkout, push, fetch; gitignore/gitattributes honoured; the
`file://` transport is native in v6 (the v5 shell-out to the git binary is gone).
Unsupported and irrelevant: rebase, revert, apply, bisect, stash; cherry-pick is partial
(ort strategy) and merge and pull are fast-forward only — all moot, since merge is
server-side via the gh API and agent-initiated history rewriting is forbidden regardless.
Constraints: linked worktrees are partial and live in an experimental
`x/plumbing/worktree` package, so parallel runs use separate clones as a deliberate
choice rather than a library limit; revision resolution exists but verify which syntaxes
resolve before depending on them; global git config is read-only (author identity already
falls back to a stable harness identity in `internal/git`); no gc.

**GitHub reads via REST API over `net/http`.** `gh_pr_wait` uses the Checks API without
requiring the `gh` binary. The proposal-only create/merge tools may render a `gh` command
for the human to run, but Harness does not execute it. The `gh_` prefix names the
toolchain, not a guaranteed transport for manual proposals.
