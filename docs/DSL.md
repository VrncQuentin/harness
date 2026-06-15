# Harness Pipeline DSL

A declarative language for defining agent pipelines in the Harness. One
pipeline spec describes one unit of work: the agents involved, the steps
executed, how data flows between them, and what happens when things go wrong.

File extension: `.hp` (harness pipeline). Source files are UTF-8. The harness sanitizer hard-fails on
bidi override and zero-width characters.

Runtime integration is staged in [roadmap M9](roadmap.md#m9--pipeline-dsl), detailed in [dsl_roadmap.md](dsl_roadmap.md), and bounded in [architecture.md](architecture.md#pipeline-runner-internalpipeline--planned-m9). Until that milestone lands, this document is the language contract only; it does not imply a partially supported runtime.

Specs are source-repo workflow files. M9 discovers them in the active project's attached git directories, using `pipelines/**/*.hp` by default. Prompts and outputs are stored as run evidence under `projects/<slug>/artifacts/<run>/` in the memory repo, and operational run state stays in `harness.db` alongside other metrics/config state.

Where this spec says "project root", M9 runtime treats that as the attached source repo root containing the entrypoint `.hp` file.

Comments run from `#` to end of line, anywhere outside a `STRING` or `TEXT`
literal. A `#` inside a quoted string or a triple-backtick-quoted prompt is literal.

---

## Lexical

```ebnf
IDENT      ::= [A-Za-z_][A-Za-z0-9_]{0,63}

; Named identifier categories used in the grammar.
; They all derive from IDENT and share the same lexical rules.
pipeline_name     ::= IDENT
lib_name          ::= IDENT
step_name         ::= IDENT
agent_name        ::= IDENT
param_name        ::= IDENT
output_name       ::= IDENT
export_name       ::= IDENT
export_alias      ::= IDENT
open_agent_param  ::= IDENT

POS_INT    ::= [1-9][0-9]*

; Plain STRING: used for names, paths, and messages.
; Escapes are limited to quote, backslash, and common whitespace chars.
STRING     ::= '"' STRING_CHAR* '"'
STRING_CHAR::= UNESCAPED | ESCAPE
UNESCAPED  ::= any Unicode scalar except '"', '\', control characters, and line breaks
ESCAPE     ::= '\\' | '\"' | '\n' | '\t' | '\r'

; TEXT: used for prompts and retry text. Delimited by triple backticks.
TEXT       ::= '```' TEXT_CHAR* '```'
TEXT_CHAR  ::= TEXT_UNESCAPED | TEXT_ESCAPE
TEXT_UNESCAPED ::= any Unicode scalar sequence that does not contain an unescaped '```'
TEXT_ESCAPE    ::= '\\' | '\`' | '\$' | '\{'

COMMENT    ::= '#' NOT_NEWLINE*
NOT_NEWLINE       ::= any Unicode scalar except '\n' and '\r'

WHITESPACE ::= ' ' | '\t' | '\n' | '\r'
             ; whitespace separates tokens and is otherwise ignored

; Named STRING/TEXT categories used in the grammar.
import_path     ::= STRING
role_name       ::= STRING
model_name      ::= STRING
literal_path    ::= STRING
surface_message ::= STRING

; Agent reference: @ followed by a visible agent name.
agent_ref       ::= "@" agent_name
```

Identifiers are ASCII and case-sensitive. Reserved words are illegal for every
IDENT-derived name. Two identifiers that collide under Windows case-insensitive
comparison are rejected within the same namespace. Identifiers are never used
directly as filesystem path components; artifact paths use harness-controlled
encoding.

---

## Grammar

```ebnf
file            ::= import* (pipeline | lib)+

import          ::= "from" import_path "use" lib_name ("," lib_name)*
                    ; path relative to project root, must end in .hp
                    ; import graph must be acyclic (load-time check)

data_type       ::= "text"
                  | "json"

pipeline        ::= "pipeline" pipeline_name "(" [pipeline_params] ")"
                    ["->" export ("," export)*]
                    "{" agent* step+ "}"
                    ; runner entry point; declares agents; no agent params

lib             ::= "lib" lib_name "(" [lib_params] ")"
                    ["->" export ("," export)*]
                    "{" step+ "}"
                    ; reusable callable; agents arrive only through params

pipeline_params ::= data_param ("," data_param)*
lib_params      ::= lib_param ("," lib_param)*
lib_param       ::= data_param | agent_param
data_param      ::= param_name ":" data_type
agent_param     ::= param_name ":" "agent"
export          ::= export_alias "=" step_name "." output_name

agent           ::= "agent" agent_name "{" agent_body "}"
agent_body      ::= "as" role_name "uses" model_name

step            ::= model_step | runs_step

model_step      ::= "step" step_name "(" model_step_params ")"
                    ["->" output ("," output)*]
                    "{" model_body routes "}"
model_step_params ::= model_agent_param
                    | model_agent_param "," data_binding ("," data_binding)*
model_agent_param ::= bound_agent_param
                    | open_agent_param ":" "agent" ; route-supplied
bound_agent_param ::= param_name "=" agent_ref
data_binding    ::= param_name "=" data_source ["?"] ; ? = empty-if-absent
data_source     ::= step_name "." output_name
                  | param_name                     ; callable param or step param
                  | literal_path                   ; literal path, read-only
output          ::= output_name ":" data_type

model_body      ::= "prompt" TEXT verify* [retry] gate*

runs_step       ::= "step" step_name "(" [runs_step_params] ")"
                    ["->" output ("," output)*]
                    "{" runs_body routes "}"
runs_step_params ::= open_agent_param ":" "agent"
                   ("," open_agent_param ":" "agent")*
runs_body       ::= "runs" lib_name "(" [run_args] ")" [runs_exports]
                    ; invoke a lib
run_args        ::= run_arg ("," run_arg)*         ; bound from callable scope
run_arg         ::= param_name "=" run_source ["?"]
run_source      ::= data_source | agent_ref
runs_exports    ::= "exports" output_name ("," output_name)*

command         ::= "[" command_arg ("," command_arg)* "]"
command_arg     ::= STRING
verify          ::= "verify" command               ; repeatable; sequential, fail-fast
gate            ::= "gate" command                 ; repeatable; verdict checks after verify
retry           ::= "retry" POS_INT TEXT           ; repair turns: count + follow-up prompt

routes          ::= ok_route reject_route*
ok_route        ::= "ok" "->" ok_action
reject_route    ::= "reject" ["(" POS_INT ")"] "->" reject_action
ok_action       ::= "done"                         ; terminate callable, ok
                  | step_name ["(" route_arg ("," route_arg)* ")"]
reject_action   ::= "propagate"                    ; lib only: terminate callable, negative verdict
                  | "surface" [surface_message]    ; checkpoint + notify + human review
                  | step_name ["(" route_arg ("," route_arg)* ")"]
route_arg       ::= open_agent_param "=" agent_ref ; open-agent-param = agent
```

An `agent_ref` (`@coder`) refers to a visible agent value. In a pipeline,
visible agents are locally declared agents. In a lib, visible agents are
agent-typed callable params. The `@` is syntax; the visible name is the bare
`agent_name` (`coder`).

### Interpolation

`TEXT` literals in `prompt` and `retry` bodies, and command args in `verify`
and `gate`, may contain substitutions:

```ebnf
prompt_substitution ::= "{" IDENT "}"
                      | "$" IDENT

retry_substitution  ::= "{" IDENT "}"
                      | "{" "last_verify" "." ("cmd" | "output") "}"
                      | "$" IDENT

command_substitution ::= "$" IDENT
```

In `prompt_substitution` and `retry_substitution`:

- `{IDENT}` refers to a declared non-agent data binding visible in the step
  (callable param or step param).
- `$IDENT` refers to a declared output of the current step and expands to its
  writable artifact path.

In `retry_substitution` only:

- `{last_verify.cmd}` and `{last_verify.output}` refer to the failing verify
  command and its output tail.

In `command_substitution`:

- `$IDENT` refers to a declared output of the current step and expands to its
  artifact file path.

Substitutions referring to undeclared names, or interpolating an `agent` param,
are load-time errors.

A prompt step (a step with a `prompt` body) declares exactly one agent param,
in first position, either bound (`dev = @coder`) or open (`dev: agent`). A
`runs` step declares open agent params only (usually none); its data flows are
bound directly in the runs args from callable scope.

A prompt step body has exactly one `prompt`, followed by zero or more `verify`
commands, at most one `retry`, and then zero or more `gate` commands. `retry`
repairs output-contract and verify failures only; gate rejection is routed.
`retry` counts must be positive. A step has exactly one `ok` route. It may have
at most one bare `reject` route and at most one `reject(N)` route per threshold.

Reserved words (illegal as any IDENT-derived name):
`from`, `use`, `pipeline`, `lib`, `agent`, `step`, `runs`, `prompt`, `verify`,
`gate`, `retry`, `as`, `uses`, `ok`, `reject`, `propagate`, `done`, `surface`,
`exports`, `text`, `json`, plus `pause`, `escalates`, and `to` (reserved for
future versions).

---

## Execution Semantics

### Agents

An agent is an identity bound to a model: `as` resolves the role content from
the project's agents.md (exact match, exactly one heading), `uses` names a
registered model. One agent, one model. Model ladders (`escalates to`) are
deferred to a future version; escalation in this version is expressed through
routing (see "Open agent params").

### One step, one agent

A prompt step has exactly one agent param, and it must be the first param.
It takes one of two forms:

- bound: `step build(dev = @coder, ...)` -- the agent is fixed at declaration.
  Binding to a callable-level agent param (`rev = @reviewer`) also counts as
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
reject    -> refactor(dev = @coder)
reject(2) -> refactor(dev = @coder_xl)
```

This is the escalation mechanism of this version: the step that judges decides,
in its own routes, which agent handles each level of trouble. The policy is
readable at the human review point. Route args bind open agent params only;
they cannot override a bound agent param. Data does not travel through routes:
text and json inputs are declared on the target step, not passed in route args.

```
step review(rev = @critic, patch = build.diff) { ... }
ok -> review                         # allowed: data was bound on review
ok -> review(patch = build.diff)     # rejected: route args are not data args
```

Reject counters and cycle counters belong to the step, not the agent:
`refactor(dev = @coder)` and `refactor(dev = @coder_xl)` are the same step
trying harder, sharing one history.

### Attempts, retry, and malfunction

One entry into a prompt step is a sequence of attempts:

1. The model runs `prompt` (first attempt) in a fresh session.
2. Declared outputs are checked. Missing outputs, malformed `json`, and other
   output-contract failures fail the attempt before user verifies run.
3. The `verify` commands run in declaration order. First nonzero exit stops
   the chain and fails the attempt. Verify answers: did this step produce
   valid, usable output?
4. On a failed attempt, if repair turns remain: the harness sends the retry
   text as a follow-up turn in the same session. `retry 1` followed by a
   `TEXT` literal grants one repair turn; no `retry` field means none. The
   builtins `{last_verify.cmd}` and `{last_verify.output}` interpolate the
   failing command and its output tail. Step params remain available.
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
the callable-specific decision the routes exist to express.

`reject(N)` counts consecutive rejects and matches after at least N rejects;
the counter resets on `ok`. Resolution order: the matching `reject(N)` route
with the highest N not exceeding the count, then bare `reject`, then implicit
`surface "unhandled rejection in <step>"`. There are no silent dead ends.

Given these routes:

```
ok        -> done
reject    -> refactor(dev = @coder)
reject(2) -> refactor(dev = @coder_xl)
reject(3) -> surface "keeps failing"
```

| reject count | matched route |
|---|---|
| 1 | bare `reject` |
| 2 | `reject(2)` |
| 3 | `reject(3)` |
| 4 | `reject(3)` (highest N <= 4) |

Routes are typically written least-serious-first for readability, but
matching is independent of source order.

The harness logs the rejecting command and its output tail for route logs
and surface summaries; they are not spec-visible builtins, because gates do
not trigger repair turns and no prompt can reference them.

The `done` action terminates the current callable with `ok`. The `propagate`
action terminates it with a negative verdict. In a child callable, that
verdict maps to the caller step's `reject`. A top-level `pipeline` cannot use
`propagate`; rejected work at the runner boundary must route to another step or
use `surface` explicitly.

The action sets are asymmetric by construction. An ok route continues or
finishes; it cannot terminate with `propagate` (a passed gate cannot be
repudiated by its own route) and it cannot `surface` (resume re-resolves
routes, so an ok-surface would loop; the planned `pause` action covers the
deliberate post-success human checkpoint with its own resume rule). A
reject route routes the work, propagates the verdict, or surfaces; it cannot
resolve `done`, because accepting some rejections is the gate script's
policy decision, not the route's.

### Types and bindings

`pipeline` params declare `text` or `json` only. `lib` params may declare
`text`, `json`, or `agent`. Every step output declares `text` or `json`;
`agent` is not a valid output type. `json` values are validated by parsing
whenever they enter a step: callable args, literal sources, model-step outputs,
runs-step outputs, and step outputs all must parse before the receiving step
runs. There is no schema resolver in the DSL.

Bindings are type-checked at load: binding a data source to an agent param, or
an agent to a data param, is an error. A non-optional step-output binding must
be definitely available on every statically knowable route path into the
consuming step. If the source may be absent on any path, the binding must be
marked optional with `?`. Optional absent bindings resolve by target type:
`text` gets the empty string; `json` gets `null`. Cyclic data references across
a route loop must make their first-pass optionality visible in the signature.

`{param}` in a prompt or retry text interpolates data params only. Interpolating
an `agent` param is a load-time error. The interpolation expands the value, not
the source path: `text` params expand to their UTF-8 text content; `json` params
must parse before the step runs and expand to their JSON text. `$name` in a
verify or gate command is the file path of one of the step's own outputs. A
prompt or retry text referencing an undeclared binding is a load-time error.

`{last_verify.cmd}` and `{last_verify.output}` are only valid inside `retry`
TEXT. They refer to the failing verify command and its output tail from the
most recent attempt.

`surface` messages are literal: they are not interpolated and are passed to the
notification layer unchanged.

### Verify and gate commands

`verify` and `gate` commands are argv arrays, not shell strings. The first arg
is the executable; remaining args are passed without shell parsing. Commands run
from the project root. There is no implicit shell expansion, pipes, redirects,
globbing, or environment-variable interpolation.

Substitution is applied independently to each arg before execution. `$output`
expands to that output's artifact file path. If a command needs complex logic,
put it in a checked-in script and call the script directly:

```
verify ["go", "test", "./..."]
gate ["./scripts/no-blockers.sh", "$findings"]
```

### Namespaces

Names cannot shadow other visible names. Within one file, imported lib names
and local pipeline/lib names must be unique. Within one callable,
params, agents (pipelines only), steps, and exports share one source-resolvable
namespace and must not collide. Within one step, step params and outputs share
one namespace and must not collide with each other or with visible
callable-level names.

The practical rule is: if an unqualified `IDENT` could resolve to two things at
`file.callable(.step.$x)` scope, the spec is rejected at load. Case-insensitive
collisions are also rejected for Windows portability.

### Outputs

Declared outputs are artifact files allocated by the harness for each step
entry. In a model step, `prompt` and retry text may use `$output_name` to refer
to the writable artifact path for that output. Since this DSL runs after the M7
tool-permission milestone, model writes go through the harness tool layer and
inherit the run's sandbox and approval policy. In a runs step, declared outputs
are materialized by the harness from the child exports named in the step's
`exports` clause; no model writes them directly.

After the model turn completes, the harness validates every declared output:

- `text`: the output file must exist and be readable text.
- `json`: the output file must exist and parse as JSON.

Missing outputs, invalid JSON, paths outside the artifact root, and failed
tool writes fail the attempt before user `verify` commands run. Extra files
are permitted only inside the step's artifact directory and are recorded in
the run audit trail.

### Calling libs

`runs child(bindings)` invokes an imported or sibling `lib` as a step. Runs
args bind directly from the calling callable's scope -- step outputs, params,
agent refs, literals -- so each data flow is stated exactly once. The runs
step's own signature declares open agent params only, for the case where routes
choose which agent the child receives.

If the parent needs child exports as data, the runs step declares outputs and
names matching child exports in an `exports` clause. The harness materializes
each named child export as the runs step output of the same name. The parent
then reads the runs step output (`harden.report`), never the child internals.
Renaming child exports in the parent is not supported in this version.

```
step harden() -> report: json {
  runs security_pass(reviewer = @critic, input = build.diff)
    exports report
  ok -> done
}
```

Outcome mapping: child `done` resolves as the step's `ok`; child `propagate`
resolves as the step's `reject`. A child malfunction surfaces from inside the
child -- `surface` does not map to a caller outcome, it propagates straight to
the human regardless of nesting depth. The callable call graph must be acyclic
(load-time check).

A `pipeline` is a runner entry point: it declares agents and may call `lib`
callables, but it cannot declare `agent` params. A `lib` is reusable logic that
may declare `agent` params, but it cannot declare agents of its own and cannot
be a runner entry point. The runner binds data, not agents; only calling
callables supply agents.

### Path resolution

All `import_path` and `literal_path` values are resolved against the project
root. A path is validated as follows:

1. Reject absolute paths.
2. Reject any `..` segment before symlink resolution.
3. Resolve symlinks.
4. Reject the path if the resolved result is outside the project root.

On Windows, also reject drive-qualified paths, UNC paths, reserved device
names, and case-insensitive collisions. The same validation applies to any
path-like value the harness derives from the spec. Artifacts are stored under
the harness-controlled artifact root, and user identifiers are encoded before
they become path components.

### Load-time validation (fail fast)

A spec that parses but cannot run safely is rejected at load, before the
human review point.

**Grammar-enforced checks:**

- A prompt step has exactly one agent param and it is the first param.
- A prompt step has exactly one `prompt` and at most one `retry`.
- `retry` appears before any `gate` commands, so repair turns apply only to
  output-contract and verify failures.
- `retry` counts and `reject(N)` thresholds are positive.
- A step has exactly one `ok` route.
- `pipeline` callables do not declare `agent` params; only `lib` callables may.
- A `runs` step's step params declare open agent params only.

**Semantic checks:**

- A step has at most one bare `reject` route and at most one `reject(N)` route
  per threshold.
- Route args exactly match the target step's open agent params, with no
  missing, extra, or duplicate args.
- A `runs` step binds every param of the child lib and binds no extras or
  duplicates.
- If a `runs` step declares outputs, it must have an `exports` clause naming
  exactly those outputs. If it has no outputs, it must not have `exports`.
- Every `runs` exported name must exist in the child lib's exports with the
  same type.
- Every callable export must definitely resolve on every `done` path.
- Every non-optional step-output binding must be definitely available on every
  statically knowable route path into the consuming step.
- Agent `as` matches exactly one `agents.md` heading (zero or two-plus matches
  is an error; never a fuzzy pick).
- Every `uses` model is registered.
- `lib` callables do not declare agents; agents arrive only through params.
- `runs` may only invoke `lib` callables, not `pipeline` callables.
- Every route target is a real step or reserved action.
- `pipeline` reject routes cannot use `propagate`; top-level rejected work must
  route to another step or `surface` explicitly.
- Every binding source exists and type-checks.
- Every prompt or retry-text placeholder is a declared non-agent binding or
  builtin; `{last_verify.*}` appears only inside `retry` TEXT.
- `surface` messages are literal and not interpolated.
- Import and call graphs are acyclic.
- No visible name shadows another visible name.
- Every route arg value is a visible agent.
- `json` literal sources that exist at load parse as JSON.
- `reject` routes require at least one `gate` in the step or a `runs` child that
  can return `propagate`.

**Linter warnings (non-fatal):**

- Unreachable steps.
- Missing fallback reject routes.
- Optional bindings.
- Unused agents, unused outputs.
- Route cycles.
- `gate` commands with no preceding `verify`.
- `verify`/`gate` commands that look unusually broad or destructive.

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
artifact hashes, source repo commit, and spec SHA) lives in SQLite. Source
specs live in the attached project git repo; prompts and artifacts live in the
memory repo as run evidence.

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

Two files. `pipelines/security.hp` defines a reusable, agent-generic `lib`;
`pipelines/feature.hp` imports it and supplies the agent.

`pipelines/security.hp`:

````
lib security_pass(reviewer: agent, input: text, rework: text)
    -> report = adjudicate.summary {

  step scan(rev = @reviewer, changes = input, reworked = rework)
      -> findings: json {
    prompt ```
      Review the changes below for security issues. Write each
      finding as a JSON object to $findings: include severity,
      location, and a concrete remediation. If <reworked> is
      non-empty it supersedes the corresponding parts of <changes>.

      <changes>{changes}</changes>
      <reworked>{reworked}</reworked>
    ```
    verify ["./scripts/findings-follow-schema.sh"]
    retry 1 ```
      Your findings failed validation:

      <cmd>{last_verify.cmd}</cmd>
      <output>{last_verify.output}</output>

      Rewrite $findings so every entry passes.
    ```
    ok -> adjudicate
  }

  step adjudicate(rev = @reviewer, raw = scan.findings) -> summary: json {
    prompt ```
      Given these raw security findings, produce a single JSON
      object in $summary:
        { "pass": bool, "blockers": [...], "warnings": [...] }

      <findings>{raw}</findings>
    ```
    verify ["./scripts/summary-follows-schema.sh"]
    retry 1 ```
      Your summary failed validation:

      <cmd>{last_verify.cmd}</cmd>
      <output>{last_verify.output}</output>

      Rewrite $summary so it passes.
    ```
    gate ["./scripts/security-pass.sh", "$summary"]
    ok     -> done
    # The propagation idiom: outcome on the left, terminal action on the
    # right. This lib's verdict is negative; hand it to whoever invoked us
    # through the caller's reject routes. The
    # language equivalent of Go's "return err".
    reject -> propagate
  }
}
````

`pipelines/feature.hp`:

````
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
    prompt ```
      Implement the item below on a feature branch. Work
      incrementally and commit as you go. Do not consider
      yourself finished until build, vet, and tests pass.

      <plan>{item}</plan>
    ```
    verify ["go", "build", "./..."]
    verify ["go", "vet", "./..."]
    verify ["go", "test", "./..."]
    ok -> review
  }

  step review(rev = @critic, changes = build.diff, reworked = refactor.diff?)
      -> critiques: json {
    prompt ```
      Review the changes below for correctness and design.
      Write each finding as JSON to $critiques. If <reworked>
      is non-empty it supersedes the corresponding parts of
      <changes>.

      <changes>{changes}</changes>
      <reworked>{reworked}</reworked>
    ```
    # Shape, not judgment: every entry parses into severity /
    # location / remediation. Guarantees the gate and refactor's
    # prompt can read this document.
    verify ["./scripts/critiques-follow-schema.sh"]
    # Verdict: nonzero iff any finding is severity=blocker. The
    # verify above is what makes this exit code mean "verdict",
    # never "script choked on a malformed entry".
    retry 1 ```
      Your critique output failed validation:

      <cmd>{last_verify.cmd}</cmd>
      <output>{last_verify.output}</output>

      Rewrite $critiques so every entry passes.
    ```
    gate ["./scripts/no-blockers.sh", "$critiques"]
    ok        -> harden
    reject    -> refactor(dev = @coder)       # first strike: cheap model
    reject(2) -> refactor(dev = @coder_xl)   # two strikes: stronger model
    reject(3) -> surface "review keeps finding blockers"
  }

  step refactor(dev: agent, findings = review.critiques) -> diff: text {
    prompt ```
      Address every blocking finding below. Do not introduce
      new features while doing so.

      <findings>{findings}</findings>
    ```
    verify ["go", "build", "./..."]
    verify ["go", "test", "./..."]
    ok -> review
  }

  step harden() -> report: json {
    runs security_pass(reviewer = @critic,
                       input = build.diff,
                       rework = refactor.diff?)
      exports report
    ok        -> cleanup
    reject    -> refactor(dev = @coder_xl)   # security blockers skip the cheap model
    reject(2) -> surface "security pass keeps finding blockers"
  }

  step cleanup(dev = @coder) {
    prompt ```
      The work on this branch is complete and reviewed. Create
      the PR, and once it is approved and merged, return to
      main, delete the feature branch, and remove any leftover
      scratch files or artifacts from the work tree.
    ```
    verify ["./scripts/clean-tree.sh"]
    ok -> done
  }
}
````

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
- `harden` uses `exports report` to materialize the child lib's `report`
  export as the runs step's own `report` output; the parent never reaches into
  the child step names.
- `refactor` is the only step with an open agent param, and every route
  targeting it says which agent it sends: escalation policy is visible in
  `review` and `harden`, not hidden in a ladder.
- `security_pass` is an agent-generic `lib`: the caller decides who reviews.
  A stricter caller could pass a stronger reviewer without touching the child.

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
agent handles each level of trouble: `reject -> refactor(dev = @coder)` and
`reject(2) -> refactor(dev = @coder_xl)`.
The policy is readable at the human review point, which a per-agent ladder
never was. Model ladders may return in a future version for malfunction
handling; verdict-driven escalation stays in routes regardless.

### Malfunction is implicit; only verdicts are routed

A step that cannot produce well-formed output after its repair turns has
malfunctioned, and the response policy is universal: repair in-session, then
surface to the human with an auto-summary. No spec text exists for it -- no
fail routes, no fail counters, no escalate action. The routes express the
one thing that is genuinely callable-specific: where negatively-judged work
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

**Import aliases.** Imports currently use the lib's original name:
`from "pipelines/security.hp" use security_pass`. If name collisions become a
real problem, add explicit alias syntax such as
`from "pipelines/security.hp" use security_pass as security`. Do not add this
until a real spec needs it.

**Generated sub-libraries.** A step outputs a `lib`, a later step runs it:
a strong model splits a large item into small build steps with detailed
prompts for a cheaper model, and the run executes the result before moving
on. Generation, never self-editing: the parent spec stays immutable (spec
SHA, resume, audit trail, and the human review point all survive), and the
generated `.hp` is an artifact with provenance. The design rests on the
"specs are pure data" decision: a generated spec is data a model wrote, and
the existing load-time validation suite becomes its safety net.

Sketch: `lib` becomes an output type carrying a declared signature
(`-> sub: lib(dev: agent, item: text)`); output-contract validation
parses the artifact, runs full load validation, and checks the signature,
so `retry` handles bad generations with validator errors as the repair
signal and `runs plan.sub(...)` stays statically checkable. Generated specs
run under an expansion profile: no imports, no agent declarations (agents
arrive only through the signature), no lib-typed outputs (depth 1, no
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

**Forked delegation.** A route action that forks a sub-lib and lets the
parent continue, with the harness tracking run trees. Significant harness
work; not a language change.

**Smarter runner dispatch.** The runner currently dispatches on item
frontmatter; it could evaluate item metadata (size, affected packages) to
pick a pipeline. Runner logic, never DSL logic.

---

## Future Decisions

**Resume across spec changes.** Today a spec SHA mismatch refuses resume. A
future migration flow could let a human resume from step X under a new spec
while explicitly choosing which run state survives.

**Runner policy for terminal rejection.** A top-level pipeline cannot use
`propagate`; it must route rejected work to another step or `surface`
explicitly. The runner may still add external policy around surfaced items
(skip item, park item for review, halt the roadmap). Runner concern; decide
when the runner is built.
