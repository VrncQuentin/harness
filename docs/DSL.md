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
                "{" persona* step+ "}"

params      ::= param ("," param)*
param       ::= IDENT ":" type
export      ::= IDENT "=" IDENT "." IDENT      ; alias = step.output

type        ::= "text"
              | "dir"
              | "json"

persona     ::= "persona" IDENT "{"
                "as" STRING                    ; exact match against agents.md
                "uses" STRING                  ; must be in model registry
                ("escalates" "to" STRING)*     ; ordered ladder, same registry
                "}"

step        ::= "step" IDENT "(" [bindings] ")" ["->" output ("," output)*]
                "{" body route+ "}"

bindings    ::= binding ("," binding)*
binding     ::= IDENT "=" source ["?"]        ; ? permits empty-if-absent
source      ::= IDENT "." IDENT                ; step.output (same pipeline)
              | IDENT                          ; pipeline parameter
              | STRING                         ; literal path, read-only
output      ::= IDENT ":" type

body        ::= field*                         ; model or verify-only step
              | "runs" IDENT "(" [bindings] ")" ; sub-pipeline step

field       ::= "with" IDENT ("," IDENT)*      ; persona ref(s), run in order
              | "prompt" TEXT                  ; triple-quoted, interpolates {param}
              | "verify" STRING                ; repeatable; sequential, fail-fast
              | "gate" STRING                  ; repeatable; verdict checks after verify
              | "retries" INT                  ; default 0
              | "retry_prompt" TEXT            ; follow-up turn on retry
              | "timeout" DURATION

route       ::= outcome guard? "->" action
outcome     ::= "ok" | "fail" ["(" INT ")"] | "reject" ["(" INT ")"]
guard       ::= "[" metric op INT "]"
metric      ::= "files" | "iterations" | "tokens"
op          ::= ">" | ">=" | "==" | "<" | "<="
action      ::= IDENT                          ; bare step name = goto
              | "escalate" [IDENT]             ; walk persona ladder; optional step
              | "done"
              | "reject"                       ; terminal negative verdict
              | "surface" STRING               ; checkpoint + notify + human review
```

Reserved words (illegal as pipeline, step, persona, or binding names):
`from`, `use`, `pipeline`, `persona`, `step`, `runs`, `with`, `prompt`,
`verify`, `gate`, `retries`, `retry_prompt`, `timeout`,
`as`, `uses`, `escalates`, `to`, `ok`, `fail`, `reject`, `escalate`, `done`,
`surface`, `text`, `dir`, `json`.

Identifiers are ASCII and case-sensitive:

```ebnf
IDENT ::= [A-Za-z_][A-Za-z0-9_]{0,63}
```

Two identifiers that collide under Windows case-insensitive comparison are
rejected within the same namespace. Identifiers are never used directly as
filesystem path components; artifact paths use harness-controlled encoding.

---

## Execution Semantics

### Attempts, retries, and failure

One entry into a model step is a sequence of attempts:

1. The model runs `prompt` (first attempt) in a fresh session.
2. Declared outputs are checked. Missing outputs, malformed `json`, and other
   output-contract failures fail the attempt before user verifies run.
3. The `verify` commands run in declaration order. First nonzero exit stops
   the chain and fails the attempt. A timeout (model call or verify command
   exceeding the step `timeout`) also fails the attempt. Verify answers: did
   this step produce valid, usable output?
4. On a failed attempt, if attempts used < `retries`: the harness sends
   `retry_prompt` as a follow-up turn in the same session. The builtins
   `{last_verify.cmd}` and `{last_verify.output}` interpolate the failing
   command and its output tail. Step parameters remain available.
5. Retries exhausted: the step has malfunctioned. Fail routes resolve.
6. If output and verify pass, the `gate` commands run in declaration order.
   First nonzero exit stops the chain and rejects the step. Gate answers: does
   this valid output approve the input?
7. If output, verify, and gate all pass, ok routes resolve.

A verify-only step skips the model call and output-contract check. It runs its
`verify` commands once as the attempt, then any `gate` commands, and resolves
`ok`, `fail`, or `reject`.

A model-call timeout invalidates the current session. If retries remain, the
next attempt starts a fresh session with the original prompt plus the rendered
`retry_prompt` and timeout metadata. The timed-out session is never appended to.
A verify timeout is treated as a failed verify command; if retries remain, the
repair turn stays in the same model session because the model turn completed.
For output-contract failures and model timeouts, `{last_verify.cmd}` is a
synthetic label such as `output:summary` or `model:timeout`, and
`{last_verify.output}` contains the harness error text.

For gate rejections, `{last_gate.cmd}` and `{last_gate.output}` record the
command and output tail that produced the negative verdict. They are available
to surface summaries and route logs, but not to `retry_prompt`, because gates do
not trigger retries.

A "fail" for routing purposes always means "failed after exhausting its
retries". `fail(N)` counts consecutive such failures and matches after at
least N failures. The counter resets on `ok`. Resolution order: matching
`fail(N)` routes from highest N to lowest N, then guarded fail routes top-down,
then bare `fail`, then implicit `surface "unhandled failure in <step>"`.
There are no silent dead ends and no blind retries: `retries > 0` on a model
step without `retry_prompt` is a load-time error.

A "reject" for routing purposes means the step successfully produced valid
output, then a gate returned a negative verdict. Rejects do not consume retries
and do not trigger escalation: there is nothing malformed about the step's own
work. `reject(N)` counts consecutive rejects and matches after at least N
rejects. The counter resets on `ok`. Resolution order mirrors fail: matching
`reject(N)` routes from highest N to lowest N, then guarded reject routes
top-down, then bare `reject`, then implicit
`surface "unhandled rejection in <step>"`.

Fail and reject counters are independent. Escalation resets only within-step
attempt state and persona rung selection; it does not reset reject counters.

The `done` action terminates the current pipeline with `ok`. The `reject`
action terminates the current pipeline with a negative verdict. In a child
pipeline, that verdict maps to the caller step's `reject`. At top level, a
terminal `reject` surfaces unless the runner that invoked the pipeline has an
explicit external policy for rejected work items.

Retry vs escalate, pinned:

- retry  = same session, `retry_prompt`. The model keeps the context of what
   it just attempted; the verify output is the repair signal.
- escalate = next model on the persona ladder, fresh session, original
   `prompt`. The new model has no context to continue from.
- reject = no retry and no escalation. The step worked; its verdict says the
  upstream input is not acceptable.

### Multi-persona steps

`with a, b, c` runs the personas sequentially, each in its own session.
Persona k receives the step prompt plus the tagged outputs of personas
1..k-1, so each can build on (and avoid repeating) the previous work. The
verify chain runs once, after the final persona. Escalation attribution: a
failure during persona y's turn (model error, timeout) escalates y; a verify
failure after the full sequence escalates the final persona, since it had
the last word. `escalate <step>` from another step targets that step's
attribution rule, not a specific persona.

This final-persona verify attribution is deliberately simple and temporary.
A later revision may replace it with a human-selected attribution flow after
retries are exhausted, but the current DSL does not expose per-route persona
selection.

### Escalation

`escalate [step]` bumps the attributed persona of the named step (default:
the current step) one rung up its ladder. Ladder exhausted: implicit
`surface`. The ladder is the bound; there is no separate escalation limit.
State is keyed `(run_id, pipeline_path, step, persona)` in SQLite, scoped
per run. `escalate` on a `runs` step is a load-time error: escalation
happens inside the child pipeline, under its own personas.

After a successful bump, escalation re-enters the named step. If no step is
named, it re-enters the current step.

### Types and bindings

Every pipeline parameter and step output declares a type: `text`, `dir`, or
`json`. `json` outputs are validated by parsing before the verify chain runs,
as an implicit verify step zero. There is no schema resolver in the DSL.
Bindings are type-checked at load: binding a `dir` output to a parameter used
in `{param}` prompt interpolation is an error.

A binding to a step that has not yet executed in this run is an error unless
the binding is explicitly marked optional with `?`. Optional absent bindings
resolve to empty values (`text`: empty string, `json`: `null`, `dir`: empty
directory). Cyclic data references across a route loop must make their
first-pass optionality visible in the step signature.

`{param}` in a prompt interpolates file contents. `$name` in a verify
command is the file path of one of the step's own outputs. A prompt or
retry_prompt referencing an undeclared binding is a load-time error.

### Outputs

Declared outputs are artifact files or directories allocated by the harness
before a model step runs. In `prompt` and `retry_prompt`, `$output_name`
expands to the writable artifact path for that output. Since this DSL runs
after the M7 tool-permission milestone, model writes go through the harness
tool layer and inherit the run's sandbox and approval policy.

After the model turn completes, the harness validates every declared output:

- `text`: the output file must exist and be readable text.
- `json`: the output file must exist and parse as JSON.
- `dir`: the output directory must exist and remain inside the artifact root.

Missing outputs, invalid JSON, paths outside the artifact root, and failed
tool writes fail the attempt before user `verify` commands run. Extra files
are permitted only inside the step's artifact directory and are recorded in
the run audit trail.

### Sub-pipelines

`runs child(bindings)` invokes an imported or sibling pipeline as a step.
The caller sees only the child's exports: `harden.report` works because the
child exports `report`; the parent never learns the child's step names, and
reaching into child internals is not expressible. Outcome mapping: child
`done` resolves as the step's `ok`; child `reject` resolves as the step's
`reject`; child failure resolves as the step's `fail` (the `fail(N)` ladder
applies; re-running a failed sub-pipeline is legitimate). `surface` does not
map: it propagates straight to the human regardless of nesting depth. The
pipeline call graph must be acyclic (load-time check).

### Path resolution

All import paths and literal sources are resolved against the project root.
Absolute paths are rejected. `..` path segments are rejected before symlink
resolution. After symlink resolution, the final path must still be inside the
project root.

On Windows, drive-qualified paths, UNC paths, reserved device names, and
case-insensitive collisions are rejected. The same validation applies to any
path-like value the harness derives from the spec. Artifacts are stored under
the harness-controlled artifact root, and user identifiers are encoded before
they become path components.

### Load-time validation (fail fast)

A spec that parses but cannot run safely is rejected at load, before the human
review point: persona `as` must match exactly one agents.md heading (zero or
two-plus matches is an error, never a fuzzy pick); every `uses` / `escalates to`
model must be registered; every route target must be a real step or reserved
action; every binding source must exist and type-check; every prompt placeholder
must be a declared binding or builtin; import and call graphs must be acyclic;
`retries > 0` on a model step requires `retry_prompt`; `with` on a step without
`prompt` is an error (no prompt means no model call: the step is verify-only);
a required binding to a step output that may be absent on the first pass is an
error unless marked optional with `?`; `reject` routes require at least one
`gate` in the step or a `runs` child that can return `reject`.

### Artifacts and run state

Outputs materialize under
`artifacts/<run>/<pipeline-path>/<step>/<cycle>/<name>`, with every component
encoded by the harness before it is used on disk.

- `<run>` is the run id (one invocation of the top-level pipeline).
- `<pipeline-path>` is the invocation chain, e.g.
  `full_feature/harden:security_pass`.
- `<cycle>` is the step's entry counter within the run: routes form loops,
  so a step can execute many times per run, and each entry via a route
  increments its cycle so artifacts never overwrite. Retries within an entry
  share the cycle -- same session, same attempt, the artifact is that
  conversation's final state.

A cross-step binding resolves to the source step's latest cycle; the consumed
cycle and artifact hashes are recorded in SQLite as the audit trail. Run state
(fail counters, escalation rungs, cycle counters, consumed-cycle records,
artifact hashes, spec SHA) lives in SQLite; specs, prompts, and artifacts live
in git.

### Surfacing and notification

`surface` checkpoints the run, freezes it, and notifies the human. Delivery
is harness configuration, not DSL:

```toml
[surface]
discord_webhook = "https://discord.com/api/webhooks/..."
summarizer      = "qwen3-coder-30b"
```

The message contains the pipeline path, step, fail counts, escalation rungs,
reject counts, the last verify or gate output tail, consumed artifact hashes,
current output hashes, and a short summary of the step transcript produced by
the local summarizer model.

Resume semantics:

- If the spec SHA changed, resume is refused.
- The harness re-runs the surfaced step's verify chain first.
- If verify passes, the run continues from the step's `ok` route.
- If verify fails and tracked artifacts changed, the UI shows a human conflict
  prompt instead of blindly re-entering the model.
- If verify fails and tracked artifacts are unchanged, the step re-enters the
  normal retry/escalation path.

This lets a human fix the problem by hand without the harness clobbering the
fix, while still detecting stale or conflicting state.

---

## Example

Two files. `pipelines/security.hp` defines a reusable sub-pipeline;
`pipelines/feature.hp` imports and invokes it.

`pipelines/security.hp`:

```
pipeline security_pass(input: text, rework: text)
    -> report = adjudicate.summary {
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

  step adjudicate(raw = scan.findings) -> summary: json {
    with critic
    prompt """
      Given these raw security findings, produce a single JSON
      object in $summary:
        { "pass": bool, "blockers": [...], "warnings": [...] }

      <findings>{raw}</findings>
    """
    verify "./scripts/security-summary-valid.sh"
    gate "./scripts/security-pass.sh $summary"
    retries 1
    retry_prompt """
      Your summary failed JSON validation:

      <output>{last_verify.output}</output>

      Rewrite $summary as valid JSON.
    """
    ok     -> done
    reject -> reject
    fail   -> surface "adjudication malformed"
  }

}
```

`pipelines/feature.hp`:

```
from "pipelines/security.hp" use security_pass

pipeline full_feature(plan: text) {
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
    fail(2) -> surface "build stuck"
    fail    -> escalate
  }

  step review(changes = build.diff, reworked = refactor.diff?)
      -> critiques: json {
    with critic
    prompt """
      Review the changes below for correctness and design.
      Write each finding as JSON to $critiques. If <reworked>
      is non-empty it supersedes the corresponding parts of
      <changes>.

      <changes>{changes}</changes>
      <reworked>{reworked}</reworked>
    """
    verify "./scripts/critiques-valid.sh"
    gate "./scripts/no-blockers.sh $critiques"
    retries 1
    retry_prompt """
      Your critique output failed validation:

      <output>{last_verify.output}</output>

      Rewrite $critiques so every entry passes.
    """
    ok        -> harden
    reject(3) -> surface "review keeps finding blockers"
    reject    -> refactor
    fail(2)   -> surface "reviewer malformed output"
    fail      -> escalate
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
    fail    -> review
  }

  step harden(changes = build.diff, reworked = refactor.diff?) {
    runs security_pass(input = changes, rework = reworked)
    ok        -> cleanup
    reject(2) -> surface "security pass keeps finding blockers"
    reject    -> refactor
    fail(2)   -> surface "security pass failing"
    fail      -> harden
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
}
```

Note on `review` and `harden`: both bind `build.diff` and `refactor.diff?`
with the supersede convention, because after a refactor cycle the build output
alone is stale. The `?` marks `refactor.diff` as optional: on the first pass it
is empty because `refactor` has not run yet, and the prompts degrade cleanly.

---

## Key Decisions and Trade-offs

### Specs are pure data, executed by trusted infrastructure

The spec is a declarative artifact reviewed at the human review point, not an
executable. The language has no conditionals, no loops, and no arithmetic;
anything that looks like logic is an explicit route. Verify commands are the
one escape hatch: they embed executables, so by convention anything beyond a
one-liner lives in a checked-in script reviewed as code. Trade-off: a spec
cannot compute. Deliberate.

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
verify-output injection. The repair conversation is explicit in the spec, runs
in the same session for verify/output failures (the model keeps its context),
and a routing-level `fail` always means retries are exhausted. Blind retries
are unrepresentable: `retries > 0` without `retry_prompt` fails at load.
Timeouts fold into failure -- a hung model call or verify is a failed attempt
-- which removed the `timeout` outcome entirely.

### Verify is not verdict

Verify commands check whether the step produced usable output. Gate commands
check whether that usable output approves the input. The first verify failure
stops the verify chain and is what `{last_verify.cmd}` /
`{last_verify.output}` reference. The first gate rejection stops the gate chain
and is what `{last_gate.cmd}` / `{last_gate.output}` reference. Ordering is a
spec-author concern: cheap and likely-to-fail first, expensive last.

### Sequential personas over parallel fan-out

`with a, b, c` runs in order, each persona seeing prior outputs, so critics
complement instead of duplicating each other. This trades wall-clock time
for output quality and dramatically simpler semantics (one verify chain,
attributable escalation). Parallel fan-out remains a possible future
update, not a current feature.

### Run limits are configuration, not language

Runaway protection belongs to harness configuration, not the DSL. Operators can
set wall-clock, step-entry, and similar run limits outside the spec. If a
configured limit fires, the harness surfaces the run with a clear operational
reason; the DSL does not route on configured run limits.

### Surface is the only stop

`halt` was removed: its sole distinction was "terminal, non-resumable", and
the human at a `surface` checkpoint can abandon a run, which subsumes it. Every
stop reaches a human, with no asterisk. Notification (Discord webhook,
local-model summary) is harness config so specs stay portable across
deployments.

### The pipeline does not know the roadmap exists

No `item`, no `advance`. A pipeline takes parameters and describes one unit
of work; a small runner outside the DSL reads the roadmap, dispatches each
item to the pipeline named in its frontmatter, and binds the item as the
argument. Heterogeneous item complexity is solved by items naming different
pipelines. Fail counters, reject counters, and escalation rungs are per-run and
therefore reset per item naturally. A self-contained full-roadmap pipeline is
still expressible: take no parameters and read `"roadmap/"` as a literal source.

---

## UX Requirements

Before a run starts, the UI shows a dry-run preview: resolved imports,
personas, models, steps, routes, verify commands, gate commands, declared
outputs, optional bindings, and suspicious paths. Because verify and gate
commands run as trusted local code, the preview also states that they will
execute with the user's local permissions.

During a run, the UI shows a visual pipeline graph with the current step,
previous cycles, fail counts, reject counts, escalation rung, and next matched
route. Route logs explain each transition, for example:

```
review reject count=1 matched reject -> refactor
```

Each run has an artifact browser organized by step cycle. It shows the rendered
prompt, resolved bindings, model transcript, declared outputs, extra files,
verify logs, gate logs, and consumed-cycle records. Optional absent bindings are
called out explicitly, e.g. `refactor.diff? is empty because refactor has not
run yet`.

When a run surfaces, the UI presents a "why am I stopped?" card with the failed
or rejecting command, output tail, artifact hashes, matched route, escalation
state, and suggested next actions. Resume controls are explicit: re-run verify
and gate, continue model repair, escalate, abort run, or mark fixed.

The spec linter is available without starting a run. It reports parse and
load-time errors plus warnings for unreachable steps, missing fallback fail or
reject routes, optional bindings, unused personas, unused outputs, route cycles,
and verify/gate commands that look unusually broad or destructive.

Failed, rejected, or surfaced runs can produce a minimal repro bundle containing
the spec SHA, rendered step prompt, consumed artifact hashes, current output
hashes, and verify or gate output tail.

---

## Planned

**Proper parser.** Hand-rolled recursive descent over `text/scanner`,
estimated ~300-400 lines including load-time validation. Decided: no JSON
intermediate format. The grammar in this document is the source of truth.

---

## Potential Improvements

**Shorthand for identity bindings.** `step build(plan = plan)` stutters;
`step build(plan)` as sugar for `x = x` would remove the noise. Cheap, pure
taste, and the first sugar in the language -- decide deliberately.

**Guard metrics referencing step state.** Extending guards beyond `files`,
`iterations`, `tokens` to e.g. a sibling step's fail count. Adds query
semantics to the guard language; wait for a concrete need.

---

## Potential Updates

**Parallel fan-out on `with`.** A parallel mode for independent critics,
merging artifacts and adjudicating via verify. Sequential is the semantics
today; parallel would need an attribution and merge story.

**Forked delegation.** A route action that forks a sub-pipeline and lets
the parent continue, with the harness tracking run trees. Significant harness
work; not a language change.

**Smarter runner dispatch.** The runner currently dispatches on item
frontmatter; it could evaluate item metadata (size, affected packages) to
pick a pipeline. Runner logic, never DSL logic.

---

## Future Decisions

**Human-selected multi-persona attribution.** Today, verify failures on a
multi-persona step attribute to the final persona. A later UX can surface the
transcript after retries are exhausted and let the human choose which persona,
if any, should escalate.

**Resume across spec changes.** Today a spec SHA mismatch refuses resume. A
future migration flow could let a human resume from step X under a new spec
while explicitly choosing which run state survives.
