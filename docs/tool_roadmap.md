# Harness Tool Roadmap — M10

Milestone position: **this document is M10**, slotting between M9 (Layout V2, shipped) and M11 (Pipeline DSL, planned).
M11's stated dependencies are M7 and M9; it gains M10 as a third, since the DSL runner
binds tool calls against this surface. The DSL does not currently
support tool calling, so tool renames here break no `.hp` files.

This supersedes the standing deferral of file edit/patch (deferred at M4 and again
beyond M7 in roadmap.md): `edit` lands in M10.1.

Every claim is tagged:

- **[P]** Present — true of the repo today.
- **[T]** Target — designed here, built in M10.
- **[X]** External — belongs to another roadmap (memory), referenced but not built here.

The X tags exist because the memory layer needs its own roadmap and its own evaluation
method before schema work starts — the retrieval instrumentation in D3 is the first piece
of that evaluation method and is deliberately built early so memory changes are measured
rather than assumed beneficial.

---

## Naming convention [T]

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
- **unprefixed** — native operations: raw-text, process, retrieval, memory. `web_search`
  [P] already fits this class unchanged.

Adding a language to `ast_*` means adding a front-end, not tools. Adding a toolchain
means adding tools, because the output shape differs.

---

## Reconciliation with the existing surface

The current tools [P]: `file_read`, `file_list`, `file_write`, `shell_exec`,
`web_search`. Disposition:

| Existing | Disposition |
|---|---|
| `file_read` | **Replaced by `read`** — adds locator addressing and range reads. |
| `file_write` | **Replaced by `edit`** — adds hash anchoring and verify-after-mutate. Whole-file write remains as a mode for new-file creation. |
| `shell_exec` | **Replaced by `exec`** — argv migration, see below. |
| `file_list` | **Retained.** Directory listing is not `ast_map`'s job. |
| `web_search` | **Retained**, unprefixed native class. |

Each replacement removes its predecessor in the same milestone phase it ships —
no long-lived aliases, no dual surface for the model to choose between.

---

## A. Agent-facing tools

Deny-by-default applies to the tool list itself, not only to command argv. The surface is
bounded per capability class — see the admission rule at the end of this section.

| Tool | Class | Status | Purpose |
|---|---|---|---|
| `ast_map` | ast (Go) | T | Structural outline of a file or package. Single-file mode uses `go/parser` only. Cross-package type resolution uses `go/packages`, which shells to the `go` binary — see toolchain preconditions. |
| `ast_find` | ast (Go) | T | Symbol- and content-anchored locate. Returns stable locators + content hashes. Never bare line numbers. |
| `go_test` | toolchain | T | `go test -json` wrapper. Failures-only by default; full NDJSON teed to disk. |
| `go_lint` | toolchain | T | Wraps the `golangci-lint` binary (v2): `run --output.json.path=stdout`, parse the JSON report, group issues by linter then file. Binary presence is a toolchain precondition — see below. |
| `read` | native | T | Range- and locator-addressed read. Returns raw bytes; skeletonization is applied downstream by B1. |
| `edit` | native | T | Hash-anchored line operations; whole-file mode for new files. Rejects when the anchor hash does not match. Verify-after-mutate is mandatory, not a flag. |
| `exec` | native | T | Structured command execution — see the approval contract below. |
| `file_list` | native | P | Directory listing. Unchanged. |
| `web_search` | native | P | Unchanged. |
| `memory_query` | native | T | Retrieval entry point. Emits a per-signal trace row on every call — see D3. Richer return records (supersede state, trust, contradiction flags) are [X]: they require the memory data model. |
| `memory_propose` | native | X | Write path via proposal to a commit gate. Requires the memory roadmap's gate to exist; until then, memory writes continue through the present writers (session lifecycle, UI promotion, dedup, index rebuild). |

### `exec` and the approval contract [T]

Commands are argv arrays, not `sh -c` strings. This drops pipes, globs, and redirection;
if that proves too limiting in practice, shell-string mode can be revisited as a
deliberate decision — argv is the safer and simpler starting point, not a permanent vow.

The allowlist is a **deny filter, not an authorization**. It rejects known-bad argv
outright; anything that survives still goes through the existing per-call approval
prompt (`internal/approvals`). This preserves the shipped M7 decision that no remembered
allow ever lets a command skip the per-call prompt. The allowlist shrinks the gray zone;
the human remains the security boundary.

### Structural enforcement of recon-first [T]

`edit` requires an anchor hash that only `ast_find` can produce. The model cannot skip
recon because it cannot fabricate a valid input. Encoded as a type-level dependency in
the tool schema, not as prompt guidance.

### Toolchain preconditions [T]

The Harness targets machines that build Go — the `go` binary is a stated precondition,
not a hidden dependency: an environment that cannot build the agent's output is not a
target environment. `golangci-lint` (v2) is the same class of precondition: `go_lint`
invokes the binary as a subprocess and returns a clear tool error when it is absent.
It is deliberately **not** imported as a library — golangci-lint is GPL-3.0, so linking
it would place the shipped binary under copyleft; it would also graft its ~230-module
require graph onto `go.mod` and lean on an API surface the maintainers explicitly do
not support. Subprocess invocation carries none of those costs.

### Version control tools [T]

Tiered by reversibility. The tier determines the gate.

| Tier | Tools | Treatment |
|---|---|---|
| Read | `git_status`, `git_diff`, `git_log`, `gh_pr_wait` | Agent-callable, no gate. High-value B2 folder targets. |
| Local write | `git_commit`, `git_branch`, `git_checkout` | Agent-callable, scope-checked, gated. Undo via recorded ref SHA. |
| External | `git_push`, `gh_pr_create`, `gh_pr_merge` | Never autonomous. Emits a proposal for human approval; the approval requirement is enforced by the tool's return type, not by policy. |

Each tool is atomic. Compound sequences (merge → return to main → delete branch) are not
collapsed into single tools: a compound tool with an irreversible step in the middle has
no correct return value for partial failure. Sequencing belongs above the tool layer —
in M11's DSL runner once it can call tools, and until then in the agent loop under
per-step gates.

#### `gh_pr_wait` [T]

Blocks until the given PR's CI reaches a terminal state, then returns
`{green, red, timed_out}`; red carries the failing check names and log handles
(B3-style locators) so the model can react without scraping. Read-only — tier-1
treatment, no gate — but network-using (Checks API polled with backoff under the loop's
cancellation context), so it is disclosed like `web_search`. Two guards keep a blocking
tool honest inside an agent loop: a configurable wait ceiling so it cannot outlive the
task, and an expected-blocking flag in its schema so loop watchdogs distinguish a
legitimate long wait from a hung tool. It closes the tier-3 workflow as the only
non-proposal step in it: `gh_pr_create` (proposal) → `gh_pr_wait` → `gh_pr_merge`
(proposal).

### Repo scoping [T]

Memory repositories are also git — **one per project, paths in the projects table,
optionally user-supplied** [P]. And there are already two go-git consumers with disjoint
scopes: `internal/git` [P] exists today with the *memory repos* as its charter (init,
open, commit specific files, `harness/harness@local` fallback identity), driven by the
session lifecycle. The agent-facing `git_*` tools are the second consumer, scoped to
workspace repos only. A `git_*` tool that can resolve into a memory repo would let the
agent write memory outside the lifecycle path, silently voiding the audit direction the
memory roadmap is heading toward.

Therefore scoping is a **predicate evaluated at call time, not a config path list**: a
`git_*` call is rejected if its resolved repository root is the memory repo of any
project row. The comparison hooks exist [P]: `project.ValidateMemoryRepoPath` and the
workflow's `SameProjectRepoPath`. Workspace targets are the active project's sandbox
roots — already a list enforced by the tool sandbox [P], so `git_*` tools take an
explicit root argument rather than assuming a singular workspace.

Package shape: `internal/git` remains the single go-git wrapper and grows the workspace
operations; the scope predicate lives at the tool boundary in `internal/tools`, not in
the wrapper — the wrapper stays policy-free so the memory writer can keep using it.

### Undo for tier 2 [T]

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

### Tool admission rule [T]

A new tool must belong to an existing class and must not overlap the charter of an
existing tool in that class. A new class requires architecture-level justification of the
same weight as admitting an MCP server. Version control is admitted as a new class
deliberately.

---

## B. Governor-side transforms [T]

**Naming:** the Governor is this component — the tool-result and context layer between
execution and context assembly. The run-limit / loop-detection concept currently called
"governor" in DSL.md is a separate entity with separate responsibilities and gets its own
name (**watchdog** proposed); DSL.md is amended when M11 touches it. One name, one
component, in both documents.

Not model-callable. These fire on results the model never sees raw.

| # | Transform | Notes |
|---|---|---|
| B1 | Query-aware skeletonizer | Full bodies for spans relevant to the active task, signatures elsewhere. Consumes `read` output and the parser front-end. The active query is an input an external proxy structurally cannot have. Query-aware from the start. |
| B2 | Tool-output folder | Per-tool output compression for the tools in section A. One folder per toolchain tool, not a dispatcher. |
| B3 | Tee-on-failure | Full unfiltered output to disk on non-zero exit; context receives the compressed form plus a retrieval handle (stable locator). |
| B4 | Observation mask | Stale tool outputs replaced with placeholders; reasoning preserved. |
| B5 | Token gate | Recount after every transform with the **same counter used for budgeting**, auto-revert when a transform increased the count. Correct against the present rune-quarter heuristic [P] because both sides use the same counter; swapping in a real tokenizer via the existing `WithTokenizer` hook is a separate accuracy improvement, not a B5 prerequisite. |
| B6 | MCP result normalizer | Deferred — no MCP servers admitted. If admitted, results enter this same chain. No bypass path. |

Package homes are documented in architecture.md as each lands (proposed:
`internal/governor` for B1–B5, `internal/git` growing workspace ops, `gh` under `internal/gh` or beside it, `internal/tools` continuing to
own the callable surface). Documentation follows code by at most the same change.

---

## C. Memory commit layer [X — memory roadmap]

Everything in this section presumes a record-based memory data model — stable IDs,
supersede chains, trust/origin scores, contradiction flags, rejected-alternatives-as-rows
— **which does not exist**. Memory today is markdown + `vectors.bin`/`manifest.json` in
per-project git repos, with SQLite holding config/metrics/projects [P]. Present writers:
session lifecycle (summarize → episode → commit), UI promotion, dedup, index rebuild.

These are therefore target contracts for the memory roadmap, recorded here so the tool
layer doesn't contradict them:

- **C1 — commit gate.** Single writer to memory, verdicts `{accept, reject, supersede,
  hold}`, append-only audit contract. Consolidating the four present writers behind it is
  memory-roadmap work.
- **C2 — hard-lock predicate.** The memory-repo scoping predicate above **is the M9
  slice of C2** and ships in M10 phase 2 with the tier-2 git tools — it needs only the
  projects table, not the record model. The fuller hard-lock set follows the memory
  roadmap.
- **C3 — origin class.** The M10 slice: `ast_*` output is extraction-class, model output
  is inference-class, and the tool layer records which. Attaching origin to memory
  records is memory-roadmap work.

The sequencing rule: **no memory schema work starts before D3 reports**, because the
project has no way to know whether a memory change helps without it.

---

## D. Offline passes

| # | Pass | Status | Notes |
|---|---|---|---|
| D3 | Retrieval quality harness | T | See contract below. Built natively. |
| D1 | Failure aggregator | T (late) | Groups recurring failures across runs into evidence-backed issues, surfaced to a human. No auto-apply. |
| D2 | Replay-as-regression | X | Requires run material in a queryable store — depends on what the memory roadmap decides to persist. Not "already held" anywhere today. |
| D4 | Invariant CI checks | T (late) | Scope-predicate completeness, allowlist deny-by-default. Plain Go tests. Supersede-chain acyclicity joins when supersede chains exist [X]. |

### D3 contract [T]

Retrieval today [P] is a two-signal weighted blend:
`semantic_weight * similarity + recency_weight * exp_decay`. The trace contract is
**per-signal contribution**, not per-list ranks:

```
{query, candidate_id, signal_values{similarity, recency_decay}, weights, final_score, returned}
```

one row per candidate per call, emitted by `memory_query` as part of its contract, not a
debug flag. This is implementable against the present blend and survives retrieval
becoming an N-signal fusion later — new signals add keys, the schema holds. Measurement:
precision@k / recall@k against a labeled query set, whose labels accumulate from real
work starting in phase 1.

D3 is the evaluation method the memory roadmap is gated on.

---

## Build order — M10 phases

**M10.1 — auditable edit loop**
`ast_map` (single-file tier), `ast_find`, `read` (replaces `file_read`), `edit` (replaces
`file_write`), `git_status`, `git_diff`, `git_log`, B1, B3, C3 M10-slice.
Begin the D3 labeled query set.

**M10.2 — execution, compression, local VC writes**
`exec` (replaces `shell_exec`, allowlist as deny filter + existing approval prompt),
`go_test`, `go_lint`, `git_commit`, `git_branch`, `git_checkout`, B2, B5, **C2 predicate,
ref-SHA recording, reflog wiring** — the write tools and their scoping/undo ship
together; write tools first and lock later is the one ordering that must not happen.
Continue D3 labels.

**M10.3 — retrieval instrumentation**
`memory_query` trace emission, D3 harness. Ships against the present two-signal blend;
requires no memory schema change.

**M10.4 — external VC**
`git_push`, `gh_pr_create`, `gh_pr_merge` behind the proposal-return-type gate, plus
`gh_pr_wait` closing the create → wait-green → merge workflow. GitHub
token from environment variable only — never persisted, never in config, never rendered
by `/config`, never in model context. D1 lands here or after M11, whichever the failure
volume justifies.

**Then M11** — Pipeline DSL runner (already specced in roadmap.md/dsl_roadmap.md),
binding tool calls against this surface; M10 joins M7 and M9 as its dependencies.

**Deferred:** B6, D2, D4's supersede check, `memory_propose`/C1 (memory roadmap), and the
memory roadmap itself — which starts only after M10.3 produces numbers.

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
falls back to a stable harness identity in `internal/git` [P]); no gc.

**gh via REST API over `net/http`**, not the `gh` CLI — no external binary, structured
responses. Tools keep the `gh_` prefix; it names the toolchain, not the transport.
