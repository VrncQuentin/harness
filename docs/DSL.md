# Harness Pipeline DSL

A declarative language for defining agent pipelines in the Harness. One
pipeline spec describes one unit of work: the agents involved, the steps
executed, how data flows between them, and what happens when things go wrong.

File extension: `.hp` (harness pipeline). Source files are UTF-8. The harness sanitizer hard-fails on
bidi override and zero-width characters.

Comments run from `#` to end of line, anywhere outside a `STRING` or `TEXT`
literal. A `#` inside a quoted string or a triple-quoted prompt is literal.

---

## Lexical

```ebnf
IDENT      ::= [A-Za-z_][A-Za-z0-9_]{0,63}

; Named identifier categories used in the grammar.
; They all derive from IDENT and share the same lexical rules.
pipeline_name     ::= IDENT
step_name         ::= IDENT
agent_name        ::= IDENT
param_name        ::= IDENT
output_name       ::= IDENT
export_alias      ::= IDENT
open_agent_param  ::= IDENT

POS_INT    ::= [1-9][0-9]*

STRING     ::= '"' STRING_CHAR* '"'
STRING_CHAR::= UNESCAPED | ESCAPE
UNESCAPED  ::= any Unicode scalar except '"', '\', control characters, and line breaks
ESCAPE     ::= '\\' | '\"' | '\n' | '\t' | '\r' | '\$' | '\{'

TEXT       ::= '"""' TEXT_BODY '"""'
TEXT_BODY  ::= any Unicode scalar, with the following escapes recognized:
               '\\' | '\"' | '\$' | '\{'
               ; the unescaped sequence """ terminates the literal

COMMENT    ::= '#' NOT_NEWLINE*
             ; a comment runs to end of line and is ignored;
             ; '#' inside a STRING or TEXT literal is literal

WHITESPACE ::= ' ' | '\t' | '\n' | '\r'
             ; whitespace separates tokens and is otherwise ignored
```

Identifiers are ASCII and case-sensitive. Two identifiers that collide under
Windows case-insensitive comparison are rejected within the same namespace.
Identifiers are never used directly as filesystem path components; artifact
paths use harness-controlled encoding.

---

## Grammar

```ebnf
file            ::= import* pipeline+

import          ::= "from" import_path "use" pipeline_name ("," pipeline_name)*
                    ; path relative to project root, must end in .hp
                    ; import graph must be acyclic (load-time check)

data_type       ::= "text"
                  | "json"

param_type      ::= data_type
                  | "agent"

pipeline        ::= "pipeline" pipeline_name "(" [pipeline_params] ")"
                    ["->" export ("," export)*]
                    "{" agent* step+ "}"

pipeline_params ::= pipeline_param ("," pipeline_param)*
pipeline_param  ::= param_name ":" param_type
export          ::= export_alias "=" step_name "." output_name

agent           ::= "agent" agent_name "{"
                    "as" role_name                 ; exact match against agents.md
                    "uses" model_name              ; must be in model registry
                    "}"

step            ::= "step" step_name "(" [step_params] ")"
                    ["->" output ("," output)*]
                    "{" body routes "}"

step_params     ::= step_param ("," step_param)*
step_param      ::= binding                        ; bound param
                  | open_agent_param ":" "agent"  ; open agent param, route-supplied
binding         ::= param_name "=" source ["?"]    ; ? = empty-if-absent
source          ::= step_name "." output_name      ; step.output (same pipeline)
                  | param_name                     ; pipeline param or step param
                  | agent_ref                      ; agent name
                  | literal_path                   ; literal path, read-only
output          ::= output_name ":" data_type

body            ::= model_body
                  | runs_body
model_body      ::= "prompt" TEXT verify* gate* [retry]
runs_body       ::= "runs" pipeline_name "(" [run_args] ")" ; sub-pipeline step
run_args        ::= binding ("," binding)*         ; bound from pipeline scope

verify          ::= "verify" shell_command         ; repeatable; sequential, fail-fast
gate            ::= "gate" shell_command           ; repeatable; verdict checks after verify
retry           ::= "retry" POS_INT TEXT           ; repair turns: count + follow-up prompt

routes          ::= ok_route reject_route*
ok_route        ::= "ok" "->" ok_action
reject_route    ::= "reject" ["(" POS_INT ")"] "->" reject_action
ok_action       ::= goto                           ; continue the pipeline
                  | "done"                         ; terminate pipeline, ok
reject_action   ::= goto                           ; route rejected work
                  | "reject"                       ; terminate pipeline, negative verdict
                                                   ; "reject -> reject" is the propagation
                                                   ; idiom: hand the verdict to the caller
                                                   ; (or runner), like Go's "return err"
                  | "surface" [surface_message]    ; checkpoint + notify + human review
goto            ::= step_name ["(" route_arg ("," route_arg)* ")"]
route_arg       ::= open_agent_param "=" agent_ref ; open-agent-param = agent

agent_ref ::= "@" agent_name
```

An `agent_ref` (`@coder`) refers to a declared agent. The `@` is syntax; the
agent's declared name is the bare `agent_name` (`coder`).

### Interpolation

`TEXT` literals in `prompt` and `retry` bodies, and `shell_command` literals in
`verify` and `gate`, may contain substitutions:

```ebnf
text_substitution ::= "{" IDENT "}"
                    | "{" "last_verify" "." ("cmd" | "output") "}"
                    | "$" IDENT

cmd_substitution  ::= "$" IDENT
```

In `text_substitution`:

- `{IDENT}` refers to a declared non-agent data binding visible in the step
  (pipeline param or step param).
- `{last_verify.cmd}` and `{last_verify.output}` refer to the failing verify
  command and its output tail.
- `$IDENT` refers to a declared output of the current step and expands to its
  writable artifact path.

In `cmd_substitution`:

- `$IDENT` refers to a declared output of the current step and expands to its
  artifact file path.

Substitutions referring to undeclared names, or interpolating an `agent` param,
are load-time errors.

A model step's signature declares exactly one agent param, in first position,
either bound (`dev = @coder`) or open (`dev: agent`). A `runs` step's
signature declares open agent params only (usually none); its data flows are
bound directly in the runs args from pipeline scope.

A model step body has exactly one `prompt`, followed by zero or more `verify`
commands, zero or more `gate` commands, and at most one `retry`. `retry` counts
must be positive. A step has exactly one `ok` route. It may have at most one
bare `reject` route and at most one `reject(N)` route per threshold.

Reserved words (illegal as pipeline, step, agent, or binding names):
`from`, `use`, `pipeline`, `agent`, `step`, `runs`, `prompt`, `verify`,
`gate`, `retry`, `as`, `uses`, `ok`, `reject`, `done`, `surface`, `text`,
`json`, plus `pause`, `escalates`, and `to` (reserved for future versions).

---

## Execution Semantics

### Agents

An agent is an identity bound to a model: `as` resolves the role content from
the project's agents.md (exact match, exactly one heading), `uses` names a
registered model. One agent, one model. Model ladders (`escalates to`) are
deferred to a future version; escalation in this version is expressed through
routing (see "Open agent params").

### One step, one agent

A model step has exactly one agent param, and it must be the first param.
It takes one of two forms:

- bound: `step build(dev = @coder, ...)` -- the agent is fixed at declaration.
  Binding to a pipeline-level agent param (`rev = reviewer`) also counts as
  bound: the agent is fixed for the whole run once the caller supplies it.
- open: `step refactor(dev: agent, ...)` -- the agent is supplied by every
  route that targets the step.

There is no multi-agent step and, in this version, no agent-less step. A
sequence of reviewers is a sequence of steps, each with its own agent, each
binding the previous step's outputs explicitly. Verify-only steps (a gate
with no model call) are deferred to a future version.

### Open agent params

Every route targeting a step with an open agent param must bind it, fully
statically:

```
reject(2) -> refactor(dev = @coder_xl)
reject    -> refactor(dev = @coder)
```

This is the escalation mechanism of this version: the step that judges decides,
in its own routes, which agent handles each level of trouble. The policy is
readable at the human review point. Route args bind open agent params only;
data does not travel through routes.

Reject counters and cycle counters belong to the step, not the agent:
`refactor(dev = @coder)` and `refactor(dev = @coder_xl)` are the same step
trying harder, sharing one history.

### Attempts, retry, and malfunction

One entry into a model step is a sequence of attempts:

1. The model runs `prompt` (first attempt) in a fresh session.
2. Declared outputs are checked. Missing outputs, malformed `json`, and other
   output-contract failures fail the attempt before user verifies run.
3. The `verify` commands run in declaration order. First nonzero exit stops
   the chain and fails the attempt. Verify answers: did this step produce
   valid, usable output?
4. On a failed attempt, if repair turns remain: the harness sends the retry
   text as a follow-up turn in the same session. `retry 1 """..."""` grants
   one repair turn; no `retry` field means none. The builtins
   `{last_verify.cmd}` and `{last_verify.output}` interpolate the failing
   command and its output tail. Step params remain available.
5. Repair turns exhausted: the step has malfunctioned. The run surfaces with
   the auto-summary. Malfunction is never routed: there are no fail routes,
   no step can be the target of another step's malfunction, and the spec
   cannot express a policy other than repair-then-surface.
6. If outputs and verify pass, the `gate` commands run in declaration order.
   First nonzero exit stops the chain and rejects the step. Gate answers:
   does this valid output approve the input?
7. If outputs, verify, and gate all pass, ok routes resolve.

For output-contract failures, `{last_verify.cmd}` is a synthetic label such
as `output:summary` and `{last_verify.output}` contains the harness error
text.

Transient infrastructure errors (API failures, dropped connections) are
retried by the harness transparently with backoff. They are not attempts, not
spec-visible, and not routable. There is no `timeout` field in this version;
hung calls are an infrastructure concern handled by harness-level limits
(see "Run limits are configuration").

The recommended shape for agentic coding steps is no `retry` field: the agent
has tools, runs the compiler and tests itself inside its session, and
iterates until it believes the work is done. Harness `verify` is the
trust-but-verify check after the agent claims completion. A verify failure
then means the agent believes something false about its own work, which is
anomalous and worth a human look immediately. `retry` fits single-shot steps
(emit a JSON document) where one repair turn with the validator output is
cheap and usually sufficient.

### Rejection

A "reject" means the step worked and its verdict on the input is negative.
Rejects do not consume repair turns: there is nothing wrong with the step's
own work, the problem is upstream, and where rejected work goes is precisely
the pipeline-specific decision the routes exist to express.

`reject(N)` counts consecutive rejects and matches after at least N rejects;
the counter resets on `ok`. Resolution order: the matching `reject(N)` route
with the highest N not exceeding the count, then bare `reject`, then implicit
`surface "unhandled rejection in <step>"`. There are no silent dead ends.

The harness logs the rejecting command and its output tail for route logs
and surface summaries; they are not spec-visible builtins, because gates do
not trigger repair turns and no prompt can reference them.

The `done` action terminates the current pipeline with `ok`. The `reject`
action terminates it with a negative verdict. In a child pipeline, that
verdict maps to the caller step's `reject`. At top level, a terminal
`reject` surfaces unless the runner has an explicit external policy for
rejected work items.

The action sets are asymmetric by construction. An ok route continues or
finishes; it cannot terminate with `reject` (a passed gate cannot be
repudiated by its own route) and it cannot `surface` (resume re-resolves
routes, so an ok-surface would loop; the planned `pause` action covers the
deliberate post-success human checkpoint with its own resume rule). A
reject route routes the work, propagates the verdict, or surfaces; it cannot
resolve `done`, because accepting some rejections is the gate script's
policy decision, not the route's.

### Types and bindings

Every pipeline param declares `text`, `json`, or `agent`. Every step output
declares `text` or `json`; `agent` is not a valid output type. `json` values
are validated by parsing whenever they enter a step: pipeline args, literal
sources, parent outputs, child exports, and step outputs all must parse before
the receiving step runs. There is no schema resolver in the DSL.

Bindings are type-checked at load: binding a data source to an agent param, or
an agent to a data param, is an error. A binding to a step that has not yet
executed in this run is an error unless the binding is explicitly marked
optional with `?`. Optional absent bindings resolve by target type: `text` gets
the empty string; `json` gets `null`. Cyclic data references across a route loop
must make their first-pass optionality visible in the signature.

`{param}` in a prompt or retry text interpolates data params only. Interpolating
an `agent` param is a load-time error. `text` params interpolate their file
contents; `json` params interpolate their validated JSON text. `$name` in a
verify or gate command is the file path of one of the step's own outputs. A
prompt or retry text referencing an undeclared binding is a load-time error.

### Namespaces

Names cannot shadow other visible names. Within one file, imported pipeline
aliases and local pipeline names must be unique. Within one pipeline, pipeline
params, agents, steps, and exports share one source-resolvable namespace and
must not collide. Within one step, step params and outputs share one namespace
and must not collide with each other or with visible pipeline-level names.

The practical rule is: if an unqualified `IDENT` could resolve to two things at
`file.pipeline(.step.$x)` scope, the spec is rejected at load. Case-insensitive
collisions are also rejected for Windows portability.

### Outputs

Declared outputs are artifact files allocated by the harness before a model
step runs. In `prompt` and retry text, `$output_name` expands to the writable
artifact path for that output. Since this DSL runs after the M7
tool-permission milestone, model writes go through the harness tool layer and
inherit the run's sandbox and approval policy.

After the model turn completes, the harness validates every declared output:

- `text`: the output file must exist and be readable text.
- `json`: the output file must exist and parse as JSON.

Missing outputs, invalid JSON, paths outside the artifact root, and failed
tool writes fail the attempt before user `verify` commands run. Extra files
are permitted only inside the step's artifact directory and are recorded in
the run audit trail.

### Sub-pipelines

`runs child(bindings)` invokes an imported or sibling pipeline as a step.
Runs args bind directly from pipeline scope -- step outputs, pipeline params,
agent names, literals -- so each data flow is stated exactly once. The runs
step's own signature declares open agent params only, for the case where
routes choose which agent the child receives.

The caller sees only the child's exports: `harden.report` works because the
child exports `report`; the parent never learns the child's step names, and
reaching into child internals is not expressible.

Outcome mapping: child `done` resolves as the step's `ok`; child `reject`
resolves as the step's `reject`. A child malfunction surfaces from inside
the child -- `surface` does not map to a caller outcome, it propagates
straight to the human regardless of nesting depth. The pipeline call graph
must be acyclic (load-time check).

A pipeline with open (unbound) agent params cannot be a runner entry point:
the runner binds data, not agents. Only calling pipelines supply agents.
This is validated at dispatch, when the human approves the roadmap.

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

A spec that parses but cannot run safely is rejected at load, before the
human review point: agent `as` must match exactly one agents.md heading
(zero or two-plus matches is an error, never a fuzzy pick); every `uses`
model must be registered; every route target must be a real step or reserved
action; every binding source must exist and type-check; every prompt or
retry-text placeholder must be a declared non-agent binding or builtin; import
and call graphs must be acyclic; no visible name may shadow another visible
name; a model step's agent param must be first and unique; a model step must
have exactly one prompt and at most one retry; retry counts and reject route
thresholds must be positive; a step must have exactly one ok route; a step may
have at most one bare reject route and at most one reject route per threshold;
a runs step's signature may contain open agent params only; route args must
exactly match the target step's open agent params, with no missing, extra, or
duplicate args; every route arg value must be a known agent; a runs step must
bind every param of the child pipeline and may not bind extras or duplicates;
a required binding to a step output that may be absent on the first pass is an
error unless marked optional with `?`; `json` literal sources that exist at
load must parse as JSON; `reject` routes require at least one `gate` in the
step or a `runs` child that can return `reject`.

### Artifacts and run state

Outputs materialize under
`artifacts/<run>/<pipeline-path>/<step>/<cycle>/<name>`, with every component
encoded by the harness before it is used on disk.

- `<run>` is the run id (one invocation of the top-level pipeline).
- `<pipeline-path>` is the invocation chain, e.g.
  `full_feature/harden:security_pass`.
- `<cycle>` is the step's entry counter within the run: routes form loops,
  so a step can execute many times per run, and each entry via a route
  increments its cycle so artifacts never overwrite. Repair turns within an
  entry share the cycle -- same session, same attempt, the artifact is that
  conversation's final state.

A cross-step binding resolves to the source step's latest cycle; the consumed
cycle and artifact hashes are recorded in SQLite as the audit trail. Run state
(reject counters, cycle counters, consumed-cycle records, supplied agent args,
artifact hashes, spec SHA) lives in SQLite; specs, prompts, and artifacts live
in git.

### Run limits are configuration, not language

Runaway protection belongs to harness configuration, not the DSL. Operators
set wall-clock, step-entry, token, and similar run limits outside the spec;
the governor's loop detection is always on. If a configured limit fires or a
loop is detected, the harness surfaces the run with a clear operational
reason. The DSL does not route on run limits.

### Surfacing and notification

`surface` checkpoints the run, freezes it, and notifies the human. The
implicit malfunction surface and the configured-limit surface use the same
mechanism. Delivery is harness configuration, not DSL:

```toml
[surface]
discord_webhook = "https://discord.com/api/webhooks/..."
summarizer      = "qwen3-coder-30b"
```

The message contains the pipeline path, step, supplied agent args, reject
counts, the last verify or gate output tail, consumed artifact hashes,
current output hashes, and a short summary of the step transcript produced
by the local summarizer model. The optional `surface` string is the one
piece of author intent that travels into the notification; comments stay in
the spec.

Resume semantics:

- If the spec SHA changed, resume is refused.
- The harness re-runs the surfaced step's verify chain first.
- If verify passes, the run continues through the step's gate and routes.
- If verify fails and tracked artifacts changed, the UI shows a human
  conflict prompt instead of blindly re-entering the model.
- If verify fails and tracked artifacts are unchanged, the step re-enters
  the normal attempt path.

This lets a human fix the problem by hand without the harness clobbering the
fix, while still detecting stale or conflicting state.

---

## Example

Two files. `pipelines/security.hp` defines a reusable, agent-generic
sub-pipeline; `pipelines/feature.hp` imports it and supplies the agent.

`pipelines/security.hp`:

```
pipeline security_pass(reviewer: agent, input: text, rework: text)
    -> report = adjudicate.summary {

  step scan(rev = reviewer, changes = input, reworked = rework)
      -> findings: json {
    prompt """
      Review the changes below for security issues. Write each
      finding as a JSON object to $findings: include severity,
      location, and a concrete remediation. If <reworked> is
      non-empty it supersedes the corresponding parts of <changes>.

      <changes>{changes}</changes>
      <reworked>{reworked}</reworked>
    """
    verify "./scripts/findings-follow-schema.sh"
    retry 1 """
      Your findings failed validation:

      <cmd>{last_verify.cmd}</cmd>
      <output>{last_verify.output}</output>

      Rewrite $findings so every entry passes.
    """
    ok -> adjudicate
  }

  step adjudicate(rev = reviewer, raw = scan.findings) -> summary: json {
    prompt """
      Given these raw security findings, produce a single JSON
      object in $summary:
        { "pass": bool, "blockers": [...], "warnings": [...] }

      <findings>{raw}</findings>
    """
    verify "./scripts/summary-follows-schema.sh"
    gate "./scripts/security-pass.sh $summary"
    retry 1 """
      Your summary failed validation:

      <cmd>{last_verify.cmd}</cmd>
      <output>{last_verify.output}</output>

      Rewrite $summary so it passes.
    """
    ok     -> done
    # The propagation idiom: outcome on the left, terminal action on the
    # right. This pipeline's verdict is negative; hand it to whoever
    # invoked us (the caller's reject routes, or runner policy at top
    # level). The language equivalent of Go's "return err".
    reject -> reject
  }
}
```

`pipelines/feature.hp`:

```
from "pipelines/security.hp" use security_pass

pipeline full_feature(plan: text) {
  agent coder {
    as "Go Engineer"
    uses "qwen3-coder-30b"
  }

  agent coder_xl {
    as "Go Engineer"
    uses "claude-opus-4-8"
  }

  agent critic {
    as "Code Reviewer"
    uses "qwen3-coder-30b"
  }

  # Agentic coding step: no retry on purpose. The agent iterates
  # against the compiler itself; a harness verify failure here is
  # anomalous and should surface immediately.
  step build(dev = @coder, item = plan) -> diff: text {
    prompt """
      Implement the item below on a feature branch. Work
      incrementally and commit as you go. Do not consider
      yourself finished until build, vet, and tests pass.

      <plan>{item}</plan>
    """
    verify "go build ./..."
    verify "go vet ./..."
    verify "go test ./..."
    ok -> review
  }

  step review(rev = @critic, changes = build.diff, reworked = refactor.diff?)
      -> critiques: json {
    prompt """
      Review the changes below for correctness and design.
      Write each finding as JSON to $critiques. If <reworked>
      is non-empty it supersedes the corresponding parts of
      <changes>.

      <changes>{changes}</changes>
      <reworked>{reworked}</reworked>
    """
    # Shape, not judgment: every entry parses into severity /
    # location / remediation. Guarantees the gate and refactor's
    # prompt can read this document.
    verify "./scripts/critiques-follow-schema.sh"
    # Verdict: nonzero iff any finding is severity=blocker. The
    # verify above is what makes this exit code mean "verdict",
    # never "script choked on a malformed entry".
    gate "./scripts/no-blockers.sh $critiques"
    retry 1 """
      Your critique output failed validation:

      <cmd>{last_verify.cmd}</cmd>
      <output>{last_verify.output}</output>

      Rewrite $critiques so every entry passes.
    """
    ok        -> harden
    reject(3) -> surface "review keeps finding blockers"
    reject(2) -> refactor(dev = @coder_xl)   # two strikes: stronger model
    reject    -> refactor(dev = @coder)
  }

  step refactor(dev: agent, findings = review.critiques) -> diff: text {
    prompt """
      Address every blocking finding below. Do not introduce
      new features while doing so.

      <findings>{findings}</findings>
    """
    verify "go build ./..."
    verify "go test ./..."
    ok -> review
  }

  step harden() {
    runs security_pass(reviewer = @critic,
                       input = build.diff,
                       rework = refactor.diff?)
    ok        -> cleanup
    reject(2) -> surface "security pass keeps finding blockers"
    reject    -> refactor(dev = @coder_xl)   # security blockers skip the cheap model
  }

  step cleanup(dev = @coder) {
    prompt """
      The work on this branch is complete and reviewed. Create
      the PR, and once it is approved and merged, return to
      main, delete the feature branch, and remove any leftover
      scratch files or artifacts from the work tree.
    """
    verify "./scripts/clean-tree.sh"
    ok -> done
  }
}
```

Notes on the example:

- `build`, `refactor`, and `cleanup` are agentic coding steps with no
  `retry`: the agent iterates against the compiler inside its own session,
  and a harness verify failure surfaces immediately. `scan`, `adjudicate`,
  and `review` are single-shot document steps where one repair turn is
  cheap, so they carry `retry 1`.
- `review` binds `build.diff` and `refactor.diff?` with the supersede
  convention; `harden` states the same flows once, directly in its runs
  args. The `?` marks `refactor.diff` as optional: on the first pass it
  resolves to the empty string because `refactor` has not run yet, and the
  prompts degrade cleanly.
- `refactor` is the only step with an open agent param, and every route
  targeting it says which agent it sends: escalation policy is visible in
  `review` and `harden`, not hidden in a ladder.
- `security_pass` is agent-generic: the caller decides who reviews. A
  stricter caller could pass a stronger reviewer without touching the child.

---

## Key Decisions and Trade-offs

### Specs are pure data, executed by trusted infrastructure

The spec is a declarative artifact reviewed at the human review point, not an
executable. The language has no conditionals, no loops, and no arithmetic;
anything that looks like logic is an explicit route. Verify and gate commands
are the one escape hatch: they embed executables, so by convention anything
beyond a one-liner lives in a checked-in script reviewed as code. Trade-off:
a spec cannot compute. Deliberate.

### Steps declare their data contract in the signature

`step build(dev = @coder, item = plan) -> diff: text` states the agent, the
coupling, and the shape before you read the body. A step can only consume
what it declares; a prompt referencing an undeclared binding is a load-time
error. This kills the "prompt silently expands to nothing" bug class at
startup instead of at hour three. Trade-off: verbosity, accepted -- explicit
and simple is the goal, the same trade Go makes.

### One step, one agent

`with` was removed and the agent became the step's first param. This deleted
the entire multi-agent problem cluster: escalation attribution, implicit
context threading between personas, and shared verify-chain ownership. A
panel of critics is now a chain of steps whose data flow is explicit in
their signatures. Trade-off: more steps in the spec for multi-reviewer
flows; each is individually simpler and individually routable.

### Escalation is routing, not a ladder

One agent, one model. A step that judges decides in its own routes which
agent handles each level of trouble: `reject(2) -> refactor(dev = @coder_xl)`.
The policy is readable at the human review point, which a per-agent ladder
never was. Model ladders may return in a future version for malfunction
handling; verdict-driven escalation stays in routes regardless.

### Malfunction is implicit; only verdicts are routed

A step that cannot produce well-formed output after its repair turns has
malfunctioned, and the response policy is universal: repair in-session, then
surface to the human with an auto-summary. No spec text exists for it -- no
fail routes, no fail counters, no escalate action. The routes express the
one thing that is genuinely pipeline-specific: where negatively-judged work
goes.

### Verify is not verdict

Verify commands check whether the step produced usable output; failure
enters the implicit malfunction path. Gate commands check whether that
usable output approves the input; rejection enters the routes. The first
failing verify command is what `{last_verify.cmd}` / `{last_verify.output}`
reference in retry text. Ordering within a chain is a spec-author concern:
cheap and likely-to-fail first.

A gate has one signal -- nonzero exit -- which must mean "verdict negative"
and never "the gate script crashed on input it could not read". A shape
check in the verify chain is what provides that guarantee: it ensures the
gate's input contract before the gate runs, so a crash on malformed output
is correctly attributed as the step's malfunction rather than misread as a
rejection of upstream work.

### Every construct is load-bearing

Route guards, the `dir` type, gate builtins, and standalone retry counters
were all cut once examples stopped using them. The rule going forward: a
construct exists in the grammar only while a real spec exercises it.
Re-adding is cheap; carrying speculative surface area is not. `retry INT
TEXT` is the visible result -- one field, because a retry count without a
repair prompt was an invalid state the old two-field form had to outlaw with
a validation rule.

### Run limits are configuration, not language

Runaway protection (wall-clock, step entries, tokens, loop detection)
belongs to harness configuration and the governor. If a limit fires, the
harness surfaces with an operational reason. The DSL does not route on run
limits, and there is no `timeout` field in this version.

### Surface is the only stop

Every stop reaches a human: explicit `surface` routes, implicit malfunction
surfaces, configured-limit surfaces, and propagated child surfaces all use
the same checkpoint-notify-freeze mechanism. `halt` does not exist; a human
at a surface checkpoint can abandon the run, which subsumes it. Notification
(Discord webhook, local-model summary) is harness config so specs stay
portable across deployments.

### The pipeline does not know the roadmap exists

No `item`, no `advance`. A pipeline takes params and describes one unit of
work; a small runner outside the DSL reads the roadmap, dispatches each item
to the pipeline named in its frontmatter, and binds the item as the
argument. Heterogeneous item complexity is solved by items naming different
pipelines. Reject counters and cycle counters are per-run and therefore
reset per item naturally. A self-contained full-roadmap pipeline is still
expressible: take no params and read `"roadmap/"` as a literal source.

---

## UX Requirements

Before a run starts, the UI shows a dry-run preview: resolved imports,
agents, models, steps, routes, verify commands, gate commands, declared
outputs, optional bindings, and suspicious paths. Because verify and gate
commands run as trusted local code, the preview also states that they will
execute with the user's local permissions.

During a run, the UI shows a visual pipeline graph with the current step,
previous cycles, reject counts, the supplied agent for open agent params,
and next matched route. Route logs explain each transition, for example:

```
review reject count=1 matched reject -> refactor(dev = @coder)
```

Each run has an artifact browser organized by step cycle. It shows the
rendered prompt, resolved bindings, supplied agent, model transcript,
declared outputs, extra files, verify logs, gate logs, and consumed-cycle
records. Optional absent bindings are called out explicitly, e.g.
`refactor.diff? is empty because refactor has not run yet`.

When a run surfaces, the UI presents a "why am I stopped?" card with the
failed or rejecting command, output tail, artifact hashes, matched route or
malfunction summary, and suggested next actions. Resume controls are
explicit: re-run verify and gate, continue model repair, abort run, or mark
fixed.

The spec linter is available without starting a run. It reports parse and
load-time errors plus warnings for unreachable steps, missing fallback
reject routes, optional bindings, unused agents, unused outputs, route
cycles, and verify/gate commands that look unusually broad or destructive.

Failed, rejected, or surfaced runs can produce a minimal repro bundle
containing the spec SHA, rendered step prompt, supplied agent args, consumed
artifact hashes, current output hashes, and verify or gate output tail.

---

## Planned

**Proper parser.** Hand-rolled recursive descent over `text/scanner`,
estimated ~300-400 lines including load-time validation. Decided: no JSON
intermediate format. The grammar in this document is the source of truth.

---

## Potential Improvements

**Route guards.** Conditional routes (`ok [files > 12] -> review`) existed
in earlier drafts and were cut as dead code: no example ever used one.
Re-add with concrete metrics the day a real spec needs a conditional route.

---

## Potential Updates

**Pause checkpoints (committed).** `ok -> pause`: a deliberate post-success
human checkpoint, distinct from `surface` (which marks trouble and whose
resume path re-runs verify and re-resolves routes -- an ok-surface would
loop). `pause` needs its own resume rule: continue past the pause point
without re-resolving the route that paused. Will be added in a future
version; the keyword is already reserved.

**Generated sub-pipelines.** A step outputs a pipeline, a later step runs
it: a strong model splits a large item into small build steps with detailed
prompts for a cheaper model, and the run executes the result before moving
on. Generation, never self-editing: the parent spec stays immutable (spec
SHA, resume, audit trail, and the human review point all survive), and the
generated `.hp` is an artifact with provenance. The design rests on the
"specs are pure data" decision: a generated spec is data a model wrote, and
the existing load-time validation suite becomes its safety net.

Sketch: `pipeline` becomes an output type carrying a declared signature
(`-> sub: pipeline(dev: agent, item: text)`); output-contract validation
parses the artifact, runs full load validation, and checks the signature,
so `retry` handles bad generations with validator errors as the repair
signal and `runs plan.sub(...)` stays statically checkable. Generated specs
run under an expansion profile: no imports, no agent declarations (agents
arrive only through the signature), no pipeline-typed outputs (depth 1, no
recursive expansion), and verify/gate commands restricted to an allowlist
of known commands and checked-in script paths -- the model composes which
checks run where, never what they do. Provenance: artifact SHA recorded,
pipeline-path `parent/step:sub@<sha>`. Execution policy is harness config:
pause-for-review of the dry-run preview by default; auto-execute is earned
with run history.

Prerequisite before building: run data from static pipelines showing that
large items fail between logical substeps in ways a single agentic session
hides, plus a manual test that generated prompts actually outperform
hand-written ones on the small model. The degenerate version (one agentic
step decomposing internally) already exists; the feature's value is the
structure between substeps -- per-substep verify, checkpoints, artifacts,
and surface points.

**Verify-only steps.** A step with no agent param acting as a pure command
gate existed in earlier drafts and was deferred: no example used one, and
cutting it gives the clean invariant that every step has exactly one agent.
The natural use case is a join point where several routes funnel through one
check; reintroduce when a real spec hits that shape.

**Step timeouts.** A `timeout DURATION` field existed in earlier drafts and
was removed: hung calls are currently an infrastructure concern under
harness-level limits. A future version may reintroduce per-step timeouts
once real runs show where they are needed and what the session-validity
rules after a timeout should be.

**Agent model ladders.** `escalates to` existed in earlier drafts and was
deferred. If route-based agent selection proves insufficient for malfunction
handling (not verdicts), ladders can return on the agent block without
touching the route grammar. The keywords `escalates` and `to` are already
reserved.

**Additional types.** `dir` existed in earlier drafts and was cut unused.
Re-add when a step genuinely produces or consumes a directory artifact.

**JSON schemas on the type.** `json "schemas/critique.json"` existed in an
earlier draft and was cut with the schema resolver. The shape-check verify
scripts in the example (`*-follow-schema.sh`) are that feature, hand-rolled:
they guarantee the gate's input contract so its exit code means verdict,
never crash. When the same shape script pattern appears across several
specs, re-add the optional schema path on the `json` type and fold those
checks into output-contract validation, where their failures already get
retry's repair treatment for free.

**Data through route args.** Route args currently bind open agent params
only. Allowing data through routes is a sanctioned direction -- the language
drifting toward a general call graph is acceptable -- but it should be
driven by a concrete spec that needs it, not added speculatively.

**Parallel step execution.** All execution is sequential today. Independent
reviewer chains could run in parallel in a future version; this needs a
merge story for routes that join, and harness run-tree tracking.

**Forked delegation.** A route action that forks a sub-pipeline and lets
the parent continue, with the harness tracking run trees. Significant
harness work; not a language change.

**Smarter runner dispatch.** The runner currently dispatches on item
frontmatter; it could evaluate item metadata (size, affected packages) to
pick a pipeline. Runner logic, never DSL logic.

---

## Future Decisions

**Resume across spec changes.** Today a spec SHA mismatch refuses resume. A
future migration flow could let a human resume from step X under a new spec
while explicitly choosing which run state survives.

**Runner policy for terminal `reject`.** A top-level pipeline ending in
`reject` currently surfaces. The runner could instead implement an external
policy (skip item, park item for review, halt the roadmap). Runner concern;
decide when the runner is built.
