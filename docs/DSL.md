# Harness Pipeline DSL

A declarative language for defining agent pipelines in the Harness. One
pipeline spec describes one unit of work: the personas involved, the steps
executed, how data flows between them, and what happens when things go wrong.

File extension: `.hp`. Encoding: ASCII. The harness sanitizer hard-fails on
bidi override and zero-width characters, and warns on any non-ASCII content.

---

## Grammar

```ebnf
file        ::= import* pipeline+

import      ::= "from" STRING "use" IDENT ("," IDENT)*
                ; path relative to project root, must end in .hp
                ; import graph must be acyclic (load-time check)

pipeline    ::= "pipeline" IDENT "(" [params] ")"
                ["->" export ("," export)*]
                "{" header* persona* step+ anyblk? "}"

params      ::= param ("," param)*
param       ::= IDENT ":" type
export      ::= IDENT "=" IDENT "." IDENT      ; alias = step.output

type        ::= "text"
              | "dir"
              | "json" [STRING]                ; optional schema file path

header      ::= "budget" DURATION
              | "max_iterations" INT

persona     ::= "persona" IDENT "{"
                "as" STRING                    ; exact match against agents.md
                "uses" STRING                  ; must be in model registry
                ("escalates" "to" STRING)*     ; ordered ladder, same registry
                "}"

step        ::= "step" IDENT "(" [bindings] ")" ["->" output ("," output)*]
                "{" body route+ "}"

bindings    ::= binding ("," binding)*
binding     ::= IDENT "=" source
source      ::= IDENT "." IDENT                ; step.output (same pipeline)
              | IDENT                          ; pipeline parameter
              | STRING                         ; literal path, read-only
output      ::= IDENT ":" type

body        ::= field*                         ; model or verify-only step
              | "runs" IDENT "(" [bindings] ")" ; sub-pipeline step

field       ::= "with" IDENT ("," IDENT)*      ; persona ref(s), run in order
              | "prompt" TEXT                  ; triple-quoted, interpolates {param}
              | "verify" STRING                ; repeatable; sequential, fail-fast
              | "retries" INT                  ; default 0
              | "retry_prompt" TEXT            ; follow-up turn on retry
              | "timeout" DURATION

route       ::= outcome guard? "->" action
outcome     ::= "ok" | "fail" ["(" INT ")"]
guard       ::= "[" metric op INT "]"
metric      ::= "files" | "iterations" | "tokens"
op          ::= ">" | ">=" | "==" | "<" | "<="
action      ::= IDENT                          ; bare step name = goto
              | "escalate" [IDENT]             ; walk persona ladder; optional step
              | "done"
              | "surface" STRING               ; checkpoint + notify + human gate

anyblk      ::= "any" "{" (signal "->" action)* "}"
signal      ::= "loop" | "budget"              ; governor-emitted only
```

Reserved words (illegal as pipeline, step, persona, or binding names):
`from`, `use`, `pipeline`, `persona`, `step`, `runs`, `with`, `prompt`,
`verify`, `retries`, `retry_prompt`, `timeout`, `budget`, `max_iterations`,
`as`, `uses`, `escalates`, `to`, `ok`, `fail`, `escalate`, `done`, `surface`,
`any`, `loop`, `text`, `dir`, `json`.

---

## Execution Semantics

### Attempts, retries, and failure

One entry into a step is a sequence of attempts:

1. The model runs `prompt` (first attempt) in a fresh session.
2. The `verify` commands run in declaration order. First nonzero exit stops
   the chain and fails the attempt. A timeout (model call or verify command
   exceeding the step `timeout`) also fails the attempt.
3. On a failed attempt, if attempts used < `retries`: the harness sends
   `retry_prompt` as a follow-up turn in the same session. The builtins
   `{last_verify.cmd}` and `{last_verify.output}` interpolate the failing
   command and its output tail. Step parameters remain available.
4. Retries exhausted: the step has failed. Fail routes resolve.

A "fail" for routing purposes always means "failed after exhausting its
retries". `fail(N)` counts consecutive such failures and fires on exactly the
Nth; the counter resets on `ok`. Resolution order: exact `fail(N)` match,
then guarded routes top-down, then bare `fail`, then implicit
`surface "unhandled failure in <step>"`. There are no silent dead ends and
no blind retries: `retries > 0` on a model step without `retry_prompt` is a
load-time error.

Retry vs escalate, pinned:

- retry  = same session, `retry_prompt`. The model keeps the context of what
  it just attempted; the verify output is the repair signal.
- escalate = next model on the persona ladder, fresh session, original
  `prompt`. The new model has no context to continue from.

### Multi-persona steps

`with a, b, c` runs the personas sequentially, each in its own session.
Persona k receives the step prompt plus the tagged outputs of personas
1..k-1, so each can build on (and avoid repeating) the previous work. The
verify chain runs once, after the final persona. Escalation attribution: a
failure during persona y's turn (model error, timeout) escalates y; a verify
failure after the full sequence escalates the final persona, since it had
the last word. `escalate <step>` from another step targets that step's
attribution rule, not a specific persona.

### Escalation

`escalate [step]` bumps the attributed persona of the named step (default:
the current step) one rung up its ladder. Ladder exhausted: implicit
`surface`. The ladder is the bound; there is no separate escalation limit.
State is keyed `(run_id, pipeline_path, step, persona)` in SQLite, scoped
per run. `escalate` on a `runs` step is a load-time error: escalation
happens inside the child pipeline, under its own personas.

### Types and bindings

Every pipeline parameter and step output declares a type: `text`, `dir`, or
`json` with an optional schema path. `json` outputs are validated (parse,
then schema if given) before the verify chain runs, as an implicit verify
step zero. Bindings are type-checked at load: binding a `dir` output to a
parameter used in `{param}` prompt interpolation is an error (directories
interpolate as listings, not contents -- see TBD 4).

A binding to a step that has not yet executed in this run resolves to empty
(`text`: empty string, `json`: `null`, `dir`: empty directory). Cyclic data
references across a route loop are legitimate and rely on this rule.

`{param}` in a prompt interpolates file contents. `$name` in a verify
command is the file path of one of the step's own outputs. A prompt or
retry_prompt referencing an undeclared binding is a load-time error.

### Sub-pipelines

`runs child(bindings)` invokes an imported or sibling pipeline as a step.
The caller sees only the child's exports: `harden.report` works because the
child exports `report`; the parent never learns the child's step names, and
reaching into child internals is not expressible. Outcome mapping: child
`done` resolves as the step's `ok`; child failure resolves as the step's
`fail` (the `fail(N)` ladder applies; re-running a failed sub-pipeline is
legitimate). `surface` does not map: it propagates straight to the human
regardless of nesting depth. Child budgets run under
`min(declared, parent_remaining)`; a child budget signal is handled by the
child's `any` block first and propagates up as the step's `fail` if
unhandled. The pipeline call graph must be acyclic (load-time check).

### Load-time validation (fail fast)

A spec that parses but cannot run safely is rejected at load, before the
human gate: persona `as` must match exactly one agents.md heading (zero or
two-plus matches is an error, never a fuzzy pick); every `uses` /
`escalates to` model must be registered; every route target must be a real
step or reserved action; every binding source must exist and type-check;
every prompt placeholder must be a declared binding or builtin; import and
call graphs must be acyclic; `retries > 0` on a model step requires
`retry_prompt`; `with` on a step without `prompt` is an error (no prompt
means no model call: the step is a pure verify gate).

### Artifacts and run state

Outputs materialize at `artifacts/<run>/<pipeline-path>/<step>/<cycle>/<name>`.

- `<run>` is the run id (one invocation of the top-level pipeline).
- `<pipeline-path>` is the invocation chain, e.g.
  `full_feature/harden:security_pass`.
- `<cycle>` is the step's entry counter within the run: routes form loops,
  so a step can execute many times per run, and each entry via a route
  increments its cycle so artifacts never overwrite. Retries within an entry
  share the cycle -- same session, same attempt, the artifact is that
  conversation's final state.

A cross-step binding resolves to the source step's latest cycle; the
consumed cycle is recorded in SQLite as the audit trail. Run state (fail
counters, escalation rungs, cycle counters, consumed-cycle records, spec
SHA) lives in SQLite; specs, prompts, and artifacts live in git.

### Surfacing and notification

`surface` checkpoints the run, freezes it, and notifies the human. Delivery
is harness configuration, not DSL:

```toml
[surface]
discord_webhook = "https://discord.com/api/webhooks/..."
summarizer      = "qwen3-coder-30b"
```

The message contains the pipeline path, step, fail counts, escalation rungs,
the last verify output tail, and a short summary of the step transcript
produced by the local summarizer model. Resume semantics: the harness
re-runs the surfaced step's verify chain first and re-enters the model loop
only if it still fails -- the human may have fixed the problem by hand, and
re-running the step blind would clobber the fix.

---

## Example

Two files. `pipelines/security.hp` defines a reusable sub-pipeline;
`pipelines/feature.hp` imports and invokes it.

`pipelines/security.hp`:

```
pipeline security_pass(input: text, rework: text)
    -> report = adjudicate.summary {

  budget 1h
  max_iterations 10

  persona critic {
    as "Security Reviewer"
    uses "qwen3-coder-30b"
    escalates to "gpt-5.5"
  }

  step scan(changes = input, reworked = rework) -> findings: json {
    with critic
    prompt """
      Review the changes below for security issues. Write each
      finding as a JSON object to $findings: include severity,
      location, and a concrete remediation. If <reworked> is
      non-empty it supersedes the corresponding parts of <changes>.

      <changes>{changes}</changes>
      <reworked>{reworked}</reworked>
    """
    verify "./scripts/findings-valid.sh"
    retries 1
    retry_prompt """
      Your findings failed validation:

      <cmd>{last_verify.cmd}</cmd>
      <output>{last_verify.output}</output>

      Rewrite $findings so every entry passes.
    """
    ok   -> adjudicate
    fail -> surface "scan output malformed"
  }

  step adjudicate(raw = scan.findings)
      -> summary: json "schemas/security-summary.json" {
    with critic
    prompt """
      Given these raw security findings, produce a single JSON
      object in $summary:
        { "pass": bool, "blockers": [...], "warnings": [...] }

      <findings>{raw}</findings>
    """
    retries 1
    retry_prompt """
      Your summary failed schema validation:

      <output>{last_verify.output}</output>

      Rewrite $summary conforming to the schema.
    """
    ok   -> done
    fail -> surface "adjudication malformed"
  }

  any {
    loop   -> surface "governor caught a loop in security_pass"
    budget -> surface "security_pass budget exhausted"
  }
}
```

`pipelines/feature.hp`:

```
from "pipelines/security.hp" use security_pass

pipeline full_feature(plan: text) {
  budget 4h
  max_iterations 40

  persona coder {
    as "Go Engineer"
    uses "qwen3-coder-30b"
    escalates to "deepseek-v4"
    escalates to "claude-opus-4-8"
  }

  persona critic {
    as "Code Reviewer"
    uses "qwen3-coder-30b"
    escalates to "gpt-5.5"
  }

  step build(plan = plan) -> diff: text {
    with coder
    prompt """
      Implement the item below on a feature branch. Work
      incrementally and commit as you go. Do not consider
      yourself finished until build, vet, and tests pass.

      <plan>{plan}</plan>
    """
    verify "go build ./..."
    verify "go vet ./..."
    verify "go test ./..."
    retries 2
    retry_prompt """
      Verification is still failing:

      <cmd>{last_verify.cmd}</cmd>
      <output>{last_verify.output}</output>

      Fix the root cause. Do not weaken or skip tests to pass.
    """
    ok      -> review
    fail(2) -> escalate
    fail    -> surface "build stuck"
  }

  step review(changes = build.diff, reworked = refactor.diff)
      -> critiques: json "schemas/critique.json" {
    with critic
    prompt """
      Review the changes below for correctness and design.
      Write each finding as JSON to $critiques. If <reworked>
      is non-empty it supersedes the corresponding parts of
      <changes>.

      <changes>{changes}</changes>
      <reworked>{reworked}</reworked>
    """
    retries 1
    retry_prompt """
      Your critique output failed validation:

      <output>{last_verify.output}</output>

      Rewrite $critiques so every entry passes.
    """
    ok      -> harden
    fail(2) -> escalate refactor
    fail    -> refactor
  }

  step refactor(findings = review.critiques) -> diff: text {
    with coder
    prompt """
      Address every blocking finding below. Do not introduce
      new features while doing so.

      <findings>{findings}</findings>
    """
    verify "go build ./..."
    verify "go test ./..."
    retries 2
    retry_prompt """
      Still failing after your changes:

      <cmd>{last_verify.cmd}</cmd>
      <output>{last_verify.output}</output>

      Revisit the findings and fix the root cause.
    """
    ok      -> review
    fail(3) -> surface "refactor thrashing"
  }

  step harden(changes = build.diff, reworked = refactor.diff) {
    runs security_pass(input = changes, rework = reworked)
    ok      -> cleanup
    fail(2) -> surface "security pass failing"
  }

  step cleanup() {
    with coder
    prompt """
      The work on this branch is complete and reviewed. Create
      the PR, and once it is approved and merged, return to
      main, delete the feature branch, and remove any leftover
      scratch files or artifacts from the work tree.
    """
    verify "./scripts/clean-tree.sh"
    retries 1
    retry_prompt """
      The tree is not clean:

      <output>{last_verify.output}</output>

      Finish the cleanup.
    """
    ok   -> done
    fail -> surface "cleanup failed"
  }

  any {
    loop   -> surface "governor caught a loop"
    budget -> surface "budget exhausted"
  }
}
```

Note on `review` and `harden`: both bind `build.diff` and `refactor.diff`
with the supersede convention, because after a refactor cycle the build
output alone is stale. This is the empty-if-absent rule doing real work:
on the first pass `refactor.diff` is empty and the prompts degrade cleanly.

---

## Key Decisions and Trade-offs

### Specs are pure data, executed by trusted infrastructure

The spec is a declarative artifact reviewed at the human gate, not an
executable. The language has no conditionals, no loops, no arithmetic;
anything that looks like logic is either a route (explicit) or a governor
concern (loop detection, budget). Verify commands are the one escape hatch:
they embed executables, so by convention anything beyond a one-liner lives
in a checked-in script reviewed as code. Trade-off: a spec cannot compute.
Deliberate.

### Steps declare their data contract in the signature

`step build(plan = plan) -> diff: text` states the coupling and the shape
before you read the body. A step can only consume what it declares; a
prompt referencing an undeclared binding is a load-time error. This kills
the "prompt silently expands to nothing" bug class at startup instead of at
hour three. Trade-off: verbosity, accepted -- explicit and simple is the
goal, the same trade Go makes.

### Imports and exports, not reaching in

Pipelines compose through `from ... use` and signatures. A child exposes
exports; `pipeline.step.output` paths across pipeline boundaries are not
expressible, so a child can rename and restructure its internals without
breaking callers. The fully-qualified path exists only in the physical
layer (artifact paths, SQLite keys) where audit needs it. Trade-off: every
value a parent wants costs an export line in the child.

### Retry is behavior, not a route

`retries N` + `retry_prompt` replaced the `retry` action and the implicit
verify-output injection. The repair conversation is explicit in the spec,
runs in the same session (the model keeps its context), and a routing-level
"fail" always means retries are exhausted. Blind retries are
unrepresentable: `retries > 0` without `retry_prompt` fails at load.
Timeouts fold into failure -- a hung model call or verify is a failed
attempt -- which removed the `timeout` outcome and signal entirely and
shrank `any` to `loop` and `budget`.

### Multiple verifies, fail-fast

Verify commands run sequentially; the first failure stops the chain and is
what `{last_verify.cmd}` / `{last_verify.output}` reference. Ordering is a
spec-author concern: cheap and likely-to-fail first, expensive last.

### Sequential personas over parallel fan-out

`with a, b, c` runs in order, each persona seeing prior outputs, so critics
complement instead of duplicating each other. This trades wall-clock time
for output quality and dramatically simpler semantics (one verify chain,
attributable escalation). Parallel fan-out remains a possible future
update, not a current feature.

### Surface is the only stop

`halt` was removed: its sole distinction was "terminal, non-resumable", and
the human at a `surface` gate can abandon a run, which subsumes it. Every
stop reaches a human, with no asterisk. Notification (Discord webhook,
local-model summary) is harness config so specs stay portable across
deployments.

### The pipeline does not know the roadmap exists

No `item`, no `advance`. A pipeline takes parameters and describes one unit
of work; a small runner outside the DSL reads the roadmap, dispatches each
item to the pipeline named in its frontmatter, and binds the item as the
argument. Heterogeneous item complexity is solved by items naming different
pipelines. Fail counters and escalation rungs are per-run and therefore
reset per item naturally. A self-contained full-roadmap pipeline is still
expressible: take no parameters and read `"roadmap/"` as a literal source.

---

## Planned

**Checkpoint and resume across spec changes.** Today a spec SHA mismatch
refuses resume. Planned: an explicit migration path ("resume from step X
under the new spec") so a stuck pipeline can be fixed mid-run without
restarting from zero. Requires defining which SQLite run state survives a
spec edit. In the plan; sequencing TBD.

**Proper parser.** Hand-rolled recursive descent over `text/scanner`,
estimated ~300-400 lines including load-time validation. Decided: no JSON
intermediate format. The grammar in this document is the source of truth.

---

## Potential Improvements

**Shorthand for identity bindings.** `step build(plan = plan)` stutters;
`step build(plan)` as sugar for `x = x` would remove the noise. Cheap, pure
taste, and the first sugar in the language -- decide deliberately.

**Schema'd text outputs.** `json` takes a schema path today; `text` could
take a validation command the same way. Probably redundant with the verify
chain; revisit only if a real spec wants it.

**Guard metrics referencing step state.** Extending guards beyond `files`,
`iterations`, `tokens` to e.g. a sibling step's fail count. Adds query
semantics to the guard language; wait for a concrete need.

---

## Potential Updates

**Parallel fan-out on `with`.** A parallel mode for independent critics,
merging artifacts and adjudicating via verify. Sequential is the semantics
today; parallel would need an attribution and merge story.

**Forked delegation.** A route action that forks a sub-pipeline and lets
the parent continue, with the governor tracking run trees. Significant
harness work; not a language change.

**Smarter runner dispatch.** The runner currently dispatches on item
frontmatter; it could evaluate item metadata (size, affected packages) to
pick a pipeline. Runner logic, never DSL logic.

---

## TBDs

**1. Timeout folded into fail.** Proposed in this revision: a timeout is a
failed attempt, consuming a retry, removing the `timeout` outcome and `any`
signal. Open question inside that: after a model-call timeout the session
may be unusable, so the retry may need to re-issue the interrupted turn
fresh rather than append `retry_prompt`. Confirm the fold and pin the
session rule.

**2. Verify-failure attribution on multi-persona steps.** Mid-turn failures
attribute to the active persona (decided). Verify-chain failures currently
attribute to the final persona, since it had the last word. Plausible
alternative: surface instead, since the harness cannot actually know which
persona is at fault. Pick before implementing multi-persona escalation.

**3. Empty-if-absent for `json`.** An absent `text` binding is the empty
string; an absent `json` binding currently resolves to `null`. Schema'd
consumers may choke on `null`. Alternative: absent `json` skips schema
validation and interpolates as the empty string. Pin one.

**4. `dir` in prompts.** Whether a `dir` binding may appear in `{param}`
interpolation at all (as a file listing? recursive contents with size
caps?) or is restricted to `$name` path usage in verify commands. Currently
specified as a load-time error in prompts; confirm or define the
interpolation.
