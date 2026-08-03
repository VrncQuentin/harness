# Tool System

> **Current reference.** This document describes what `main` implements now. It
> is the single canonical home for the tool registry, execution flow, approval
> mechanism, sandbox, output provenance, and the checklist for adding a tool.
> Planned tool work and acceptance criteria live in
> [tool_roadmap.md](tool_roadmap.md); the filesystem primitives the sandbox
> relies on are documented in [filesystem-security.md](filesystem-security.md).

## 1. Registry, descriptors, schemas, and `CallInfo`

`internal/tools` owns the tool layer. A **tool** implements the `Tool`
interface: a unique `ID`, a JSON Schema for its parameters (`Schema`), an
`Execute(ctx, call, args) Result` function, and a short `Description`. The
**registry** (`Registry`) resolves tool calls to execution; `RegisterBuiltins`
registers every built-in tool and fails startup if a descriptor has no
implementation.

**Descriptors** are the source of built-in *default* enablement and default
approval posture. Each `Descriptor` carries `ID`, `DefaultEnabled`,
`DefaultApproval` (`allow` or `ask` — there is no descriptor-level "deny"; deny
comes from config or the evaluator), and `DefaultApprovalSource`.
`config.Defaults()` seeds each enable toggle from `BuiltinDefaultEnabled`, and
the approvals `DefaultLayer()` is generated from `BuiltinDescriptors()` — so
the defaults track the tool inventory without a second hand-maintained copy.
They are not the sole source of tool IDs in policy: `config.ToolEnabled`,
`config.Defaults`, `tools.RegisterBuiltins`, and the runtime's user-config
deny-rule list all still enumerate the 20 ids explicitly (the checklist below
documents each of those touch points).

**`CallInfo`** is the typed context handed to every tool: active project slug,
sandbox roots, the C2 memory-repo check, the toolout spill directory, session
id, caller identity, an HTTP client, an optional `MemoryQuery` closure, and an
optional `GHTokenFn` closure. The token function reads `GITHUB_TOKEN` from the
environment at call time — the function is held, never the value — so a token is
never stored, never persisted, and never rendered.

## 2. Execution flow

1. **Config enablement.** `config.LoopConfig.ToolEnabled(id)` switches over the
   per-tool `*Enabled` bools; unknown ids return false. `Defaults()` seeds each
   flag from `tools.BuiltinDefaultEnabled`. Disabled tools are neither offered
   to the model nor executed.
2. **Registration.** The runtime builds a fresh `tools.Registry`, registers the
   built-ins, and wires it into each generation. Registration rejects
   duplicates.
3. **Agent-loop dispatch.** `internal/agentloop` caches all tool schemas up
   front, filters the schema list per request by enablement, JSON-decodes the
   model's tool-call arguments, and dispatches. An unknown or disabled tool
   returns a "tool not available" result; otherwise the loop evaluates approval,
   executes, applies the governor transforms, emits a tool-result event with its
   origin, and injects the result as a `tool` message.
4. **Approval evaluation.** The loop consults the layered evaluator (see
   section 4). An `Ask` decision pauses the loop until the UI applies an
   allow/reject/always decision, bounded by an approval timeout; duplicate or
   late responses are rejected.
5. **Execution.** The tool runs with cancellation propagated via context.
6. **Output bounding and governor.** The governor applies B1/B2/B3/B5
   transforms between execution and context injection (see section 6).
7. **Result/session/audit propagation.** Loop events — text, tool calls, tool
   results with origin, and approval events — stream to the UI and are
   persisted by the task adapter: tool calls/results as `assistant`/`tool`
   messages and approvals as `system` messages, so the session history and
   audit trail record the whole turn.

## 3. Current tool inventory

The inventory below is verified against `tools.BuiltinDescriptors()` in
registration order. Column meaning: **class** (read-only / local mutation /
external-network / proposal-only), **default** (enabled by default),
**approval** (built-in default posture), and **coord** (whether the tool holds
the repository-wide memory-repository write lock).

| Tool | Class | Default | Approval | Path handling | Coord |
| --- | --- | --- | --- | --- | --- |
| `read` | read-only | on | allow | rooted (`rootfs.Set`) | no |
| `file_list` | read-only | on | allow | rooted | no |
| `ast_map` | read-only (parser) | on | allow | rooted | no |
| `ast_find` | read-only (parser) | on | allow | rooted | no |
| `git_status` | read-only (toolchain) | on | allow | pathname-validated | no |
| `git_diff` | read-only (toolchain) | on | allow | pathname-validated; worktree pinned | no |
| `git_log` | read-only (toolchain) | on | allow | pathname-validated | no |
| `edit` | local mutation | off | ask | rooted | no |
| `exec` | local mutation (subprocess) | off | ask | pathname-validated cwd | no |
| `go_test` | local mutation (subprocess) | off | ask | pathname-validated cwd | no |
| `go_lint` | local mutation (subprocess) | off | ask | pathname-validated cwd | no |
| `git_commit` | local mutation (git write) | off | ask | pathname + C2 scope check | yes |
| `git_branch` | local mutation (git write) | off | ask | pathname + C2 scope check | yes |
| `git_checkout` | local mutation (git write) | off | ask | pathname + C2 scope check | yes |
| `web_search` | external / network | off | ask | network via HTTP client | no |
| `memory_query` | read-only (retrieval) | off | allow | memory query closure | no |
| `git_push` | proposal-only | off | allow | pathname + C2 scope check | no (no write) |
| `gh_pr_create` | proposal-only | off | allow | none | no |
| `gh_pr_merge` | proposal-only | off | allow | none | no |
| `gh_pr_wait` | read-only (network CI poll) | off | allow | network via HTTP client + token | no |

### Notable per-tool behaviors

- **`read`** — range- and locator-addressed reads over the rooted handle,
  including `toolout:<id>` paging for spill files (32 KB pages, byte-offset
  continuation, rune-boundary cuts, rejects addressing inside a multibyte
  character).
- **`ast_map` / `ast_find`** — parser-backed; output is extraction-class by
  construction. Each tool's supported-language declaration is generated from
  the registered parser front-ends, never hand-written. `ast_find` returns
  stable locators plus content hashes — never bare line numbers.
- **`edit`** — hash-anchored line operations. The anchor hash is only emitted
  by `ast_map`/`ast_find`, so recon-first is enforced by type: the model cannot
  fabricate a matching anchor. Whole-file mode uses `CreateExclusive` for
  new-file creation only. Verify-after-mutate is mandatory, not a flag.
- **`exec`** — argv-array subprocess (no `sh -c` strings; no pipes, globs, or
  redirection), a deny filter for known-destructive executables and recursive
  deletes, a 30-second timeout, and a 64 KB inline cap with the full output
  preserved on failure for the B3 spill. `exec` can never be auto-allowed.
- **`go_test` / `go_lint`** — subprocess wrappers. `go_test` runs `go test
  -json` and preserves the full NDJSON on failure; `go_lint` runs the
  `golangci-lint` binary (v2) — a stated toolchain precondition, not a library
  dependency. Both time out at 3 minutes.
- **`git_status` / `git_diff` / `git_log`** — read-only workspace-repo
  inspection. `git_diff` pins the worktree through `rootfs` for content and
  caps inline output at 64 KiB by slicing the byte string; a UTF-8 boundary
  can be split at the cut, the remainder is discarded, and no `FullOutput` is
  set.
- **`git_commit` / `git_branch` / `git_checkout`** — local git writes carrying
  the C2 scope check (rejected if the resolved repo path is inside a project
  memory repo) and ref-SHA recording for reflog-based undo. They execute
  through `git.Repo.WithMutation`, which holds the repository-wide mutation
  coordinator across the write and the commit.
- **`web_search`** — DuckDuckGo Instant Answer API over the injected HTTP
  client; network use is disclosed in the output. Off by default.
- **`memory_query`** — blended semantic + recency retrieval over the active
  project's episode store; requires the embedder. Returns up to `k` hits
  (default 5, max 20) with excerpts.
- **`git_push` / `gh_pr_create` / `gh_pr_merge`** — proposal-only. They return
  explanatory command text with `Result.Proposal = true`; they perform no
  network mutation and no side effect. The agent loop does not currently
  persist or approve/execute these proposals — the user runs the command
  outside the harness. The boolean is metadata, not a persisted approval state
  machine. `git_push` still runs the C2 scope check.
- **`gh_pr_wait`** — read-only CI poller over the GitHub Checks API with
  exponential backoff (10 s → 60 s) under the loop's cancellation context. It
  returns JSON only: `{"state":"green"}`, `{"state":"timed_out"}`, or
  `{"state":"red","failed":[...]}` where `failed` lists the failing check
  names. It does not fetch logs, does not produce B3 spill handles, and does
  not add the network-use disclosure text that `web_search` emits — red is
  returned as successful `Content`, so the B3 tee cannot attach to it
  implicitly. It reads `GITHUB_TOKEN` at call time and has a configurable wait
  ceiling. Its schema marks the tool `x-expected-blocking`, but that flag is
  currently inert metadata: the agent loop does not read it and no blocking
  watchdog consumes it.

## 4. Approval mechanism

`internal/approvals` evaluates layered rules in order, last-match-wins across
layers:

1. **Built-in defaults** — generated from `tools.BuiltinDescriptors()`, so the
   default posture tracks the tool inventory automatically.
2. **User config layer** — every tool has a `LoopConfig` enable toggle, and the
   runtime maintains an explicit list of 13 tool ids for which it appends a
   `Denied` rule with source `"user: <id> disabled in config"`: `edit`, `exec`,
   `go_test`, `go_lint`, `git_commit`, `git_branch`, `git_checkout`,
   `web_search`, `memory_query`, `git_push`, `gh_pr_create`, `gh_pr_merge`, and
   `gh_pr_wait`. Note that this list is not a read-only-versus-mutating
   classification — it includes read-only `memory_query` and `gh_pr_wait`. The
   other seven default-enabled read-only tools (`read`, `file_list`, `ast_map`,
   `ast_find`, `git_status`, `git_diff`, `git_log`) are not in the list: their
   disabling is enforced by the agent loop's `ToolEnabled` filtering before
   approval evaluation, so they never reach the evaluator at all. The two
   enforcement paths are separate — enablement filtering gates whether a tool
   is offered and dispatched; the approval layer gates the listed tools that
   pass enablement.
3. **Session remembered rules** — appended as the user applies decisions
   during a session; evaluated last. A fresh evaluator is built per task
   engine, so "always" rules never leak across sessions.

**Semantics.** `allow` lets a tool run without asking; `ask` pauses the loop
for a human decision; `deny` refuses. UI decisions map to `allow` (once),
`reject` (deny once), and `always` (allow + remembered for the session). There
is no "always reject" path. `Remember` on the response is the only signal;
the loop records the approval scope on the audit event.

**The conservative `exec` rule.** For the `exec` tool, only a matching `Denied`
rule is honored; every other outcome — including `Allowed` from a session
"always" rule — is forced back to `Ask`. No remembered allow can ever let a
command skip the per-call prompt; the human remains the security boundary. The
exec deny filter narrows the gray zone but is a deny filter, not an
authorization.

**Proposal-only tools** perform no proposed side effect. `git_push`,
`gh_pr_create`, and `gh_pr_merge` return command text for a human to run; the
loop treats the result like any other tool result, and no approve-and-execute
path exists.

## 5. Sandbox

The sandbox has four distinct layers; none is a generic OS sandbox.

- **Rooted content operations.** Tools that read or write file content
  (`read`, `file_list`, `ast_map`, `ast_find`, `edit`) operate through an open
  handle on the owning sandbox root via `rootfs.Set.Open`, which pins each
  candidate root and resolves the target alongside that pin (pin-before-
  authorize), closing the check/use race. The threat model and primitives live
  in [filesystem-security.md](filesystem-security.md).
- **Pathname validation for subprocess and go-git.** Tools that hand a path to
  something outside the package — a subprocess working directory (`exec`,
  `go_test`, `go_lint`), a go-git repository (`git_*`) — validate it with
  `pathid` (`Resolve` + `ID.Contains`) instead, because there is no handle to
  give them. A path that cannot be resolved is a refusal.
- **Subprocess validation is not an OS sandbox.** The working directory check
  is the whole subprocess boundary; command containment is a separate,
  documented problem.
- **Memory-repository hard lock (C2).** `git_commit`, `git_branch`,
  `git_checkout`, and `git_push` carry a scope predicate evaluated at call
  time: the resolved repository root is checked against the memory repo of
  every project row, and a match is rejected. The predicate fails closed when
  the memory-repo list or store is unavailable. The memory-repository write
  lock for workspace repos is the repository-wide mutation coordinator held by
  `WithMutation`.
- **Token and HTTP-client injection.** Network tools use the injected HTTP
  client; `gh_pr_wait` reads the token at call time through `GHTokenFn`. No
  token value is ever stored.

## 6. Output provenance, truncation, and spill

- **`OriginClass`** records how a tool result was produced: `extraction` for
  parser-backed or deterministic output (`ast_map`, `ast_find`, `memory_query`
  set it), `inference` for model-generated content. The loop propagates origin
  onto tool-result events. Origin is metadata and never bypasses approvals,
  sandboxing, or verification.
- **In-tool bounding.** `exec` and `go_test` cap inline text (64 KiB) with
  rune-safe cuts. `Result.FullOutput` is set only for **failed** output that
  the B3 tee may spill — `exec` carries it when the inline failure text is an
  excerpt of the full output, and `go_test` preserves the raw NDJSON on its
  failure paths. Successful truncated output is intentionally discarded: a
  successful `exec`/`go_test` call returns only the bounded inline `Content`
  and no `FullOutput` (`TestExec_OutputIsCappedWhileCommandCompletes` enforces
  this for `exec`).
  `git_diff` is a separate case: it slices the byte string at 64 KiB — which can
  split a UTF-8 boundary — discards the remainder, and sets no `FullOutput`
  even on error.
- **Governor transforms** run between execution and context injection:
  - **B1 — query-aware skeletonizer:** reduces `read` output for
    parser-supported files, keeping full bodies for spans relevant to the
    active query and emitting signatures for the rest.
  - **B2 — tool-output folder:** per-tool content caps with head/tail elision
    for high-volume tools (`exec`, `go_test`, `go_lint`, `git_diff`, `git_log`).
  - **B3 — tee-on-failure:** spills the full unfiltered output of a failed call
    to `~/.harness/cache/toolout/` and injects a compact `toolout:<id>` handle
    into the conversation. The spill content is `FullOutput` when the tool
    preserved one, else `Error`; it spills only when that content is at least
    4096 bytes (`b3Threshold`) — short failures stay inline and receive no
    toolout handle. B3 is side-effectful and deliberately outside the
    B5 token gate: the spill file is already written when the gate would
    inspect it, so discarding the value would orphan the file and drop the only
    route back to otherwise-unreachable output.
  - **B5 — token gate:** auto-reverts B1 or B2 when a transform increases the
    estimated token count, using the same counter as prompt budgeting.
- **Proposal results** are ordinary `Result` values with `Proposal: true`;
  they carry no origin and no spill handle.

## 7. Checklist for adding a tool

1. **Descriptor.** Append to `builtinToolDescriptors`: `ID`, `DefaultEnabled`,
   `DefaultApproval` (`allow`/`ask`), `DefaultApprovalSource`. Order in the
   slice is registration and `List()` order.
2. **Implementation.** Add `internal/tools/<name>.go` implementing `Tool`
   (`ID`, `Schema`, `Execute`, `Description`). Follow the tier comment
   convention and the `var _ Tool = (*<name>Tool)(nil)` assertion.
3. **Registration.** Add the implementation to the `builtins` map in
   `RegisterBuiltins`; a descriptor without an implementation fails startup.
4. **Config.** Add an `XEnabled bool` to `LoopConfig`, a case in `ToolEnabled`,
   and a `BuiltinDefaultEnabled("<id>")` default in `Defaults()`. Make an
   explicit deny-layer decision: either add the tool id to the runtime's
   user-config approval-layer list so disabling is enforced at the approval
   layer too, or rely on enablement filtering alone (the seven default-enabled
   read-only tools take the latter path).
5. **Approvals.** Nothing extra beyond the descriptor — `DefaultLayer()`
   derives automatically. If the tool must never auto-allow (like `exec`), the
   conservative rule is hardcoded in the evaluator.
6. **Sandboxing.** Content read/write ⇒ rooted handle (`openTarget` via
   `rootfs.Set`); subprocess or go-git paths ⇒ `validatePath` (pathname) plus a
   comment noting the boundary; git write tools ⇒ the C2 scope check plus
   mutation through `git.Repo.WithMutation`; network ⇒ the injected HTTP
   client; secrets ⇒ a call-time token function, never a stored value.
7. **Output.** Set `Origin: extraction` for parser-backed/deterministic tools;
   set `FullOutput` on **failed** output whose inline text is truncated, so B3
   spills the whole thing — successful truncated output is intentionally
   discarded; add a B2 content cap for high-volume output.
8. **fsaudit.** Direct `os`/`path/filepath` filesystem calls must be routed
   through `internal/rootfs`, or each added to `cmd/fsaudit/allowlist.json` as a
   permanent boundary exception with a justification. Blocked patterns include
   dot-imports, extracting watched functions as values, `os.Root` references
   outside rootfs, and `os.OpenRoot` outside rootfs.
9. **Discriminator tests.** Ship a handle-bound test proving the sandbox and
   identity checks are handle-bound rather than pathname-bound: a same-name
   directory replacement, a link that escapes the root, and the coordinator
   binding where a mutation is involved.
