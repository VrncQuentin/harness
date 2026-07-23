# DSL Roadmap

## Goal

Execute reviewed `.hp` pipeline specs inside the harness using the native agent loop, tool registry, project sandbox, and browser UI.

The language contract lives in [DSL.md](DSL.md). This document is the implementation roadmap for wiring that contract into the harness runtime. It should not expand the grammar speculatively; grammar changes belong in `DSL.md` only when a concrete implementation need or real spec requires them.

## Position In The Main Roadmap

DSL runtime support is M11. It depends on M7 because pipeline model steps write declared outputs through harness tools, and `verify` / `gate` commands run as trusted local processes. Safe file writes, shell execution, approvals, and hardened project sandboxing must exist before pipeline automation is allowed to drive them.

M11 also depends on these earlier foundations:
- M3b projects: active project slug, project-owned memory layout, project directories, and project-scoped agent resolution.
- M4 native agent loop: part-based messages, cancellation, tool-call dispatch, loop limits, and UI-visible run events.
- M5 semantic memory: project-scoped indexes and episode retrieval for long-running pipeline steps.
- M7 permissions: destructive tools, shell execution, approvals, sandbox roots, audit trail, and tool toggles.
- M8 hardening: full test suite, graceful shutdown, observability, and reliable packaging.
- M9 project memory repos: one project memory repo per project, global project repo fallback, and attached source repo semantics.
- M10 tool surface: the tool ids and contracts the runner binds tool calls against ([tool_roadmap.md](tool_roadmap.md)).

## Non-Goals

- Do not implement DSL runtime before the interactive native agent loop is stable.
- Do not add roadmap concepts to the DSL. A runner can bind roadmap items to pipeline params, but `.hp` specs do not know about milestones, tickets, or item advancement.
- Do not add a JSON intermediate format. The grammar in `DSL.md` remains the source of truth.
- Do not add speculative constructs such as route guards, parallel execution, generated sub-libraries, schemas on `json`, or per-step timeouts in M11.
- Do not bypass the harness tool/approval layer. Model-written outputs are normal tool writes into declared artifact paths.
- Do not store source `.hp` specs in the memory repo by default. Specs live with the project/code repos whose scripts, tests, and paths they orchestrate.

## Package Boundary

Keep the language implementation isolated under `internal/dsl` so it can be extracted or open-sourced later without dragging the harness runtime with it.

Planned shape:

```
internal/
  dsl/
    ast.go          # syntax tree and source span types
    token.go        # token and position types
    errors.go       # parse/load/lint diagnostics
    parser/         # lexer + recursive-descent parser
    validate/       # semantic load-time validation
    linter/         # non-fatal warnings
    source/         # UTF-8 sanitizer and source file graph loading
    x/              # temporary experiments only; no runtime dependency allowed
  pipeline/         # harness runtime: DB, artifacts, UI events, agent loop, commands
```

Rules:
- `internal/dsl` may use the Go standard library and small self-contained helpers only.
- `internal/dsl` must not import `internal/pipeline`, `internal/ui`, `internal/memory`, `internal/agentloop`, `internal/tools`, `internal/config`, or database packages.
- `internal/dsl` returns typed ASTs, diagnostics, linter warnings, and a validated spec graph. It does not execute commands, open model sessions, write artifacts, or touch SQLite.
- `internal/pipeline` owns all harness integration: project directory discovery, source repo metadata, run state, artifacts, verify/gate command execution, agent-loop calls, UI events, and metrics.
- Experimental code in `internal/dsl/x` must not be imported by production packages. Promote or delete it before M11 is accepted.
- Public-ish names in `internal/dsl` should be boring and stable enough to survive extraction into a standalone module later.

## Source And Artifact Layout

Pipeline source specs are project repo data. Pipeline run artifacts are harness-managed evidence.

Source specs live in the active project's attached git directories:

```
<attached-repo>/
  pipelines/
    *.hp
    **/*.hp
  scripts/
    ...
```

The `pipelines/` convention keeps `.hp` files reviewed alongside the code, tests, and scripts they invoke. M11 discovery scans each configured project directory for `pipelines/**/*.hp`. A later iteration may add configurable pipeline roots or a shared template library, but the default execution source is the attached repo, not memory.

Run artifacts live under the active project memory repo so the harness can preserve prompts, transcripts, command logs, and output evidence independently of the source repo working tree:

```
~/.harness/
  projects/
    <project-id>/
      artifacts/
        <run>/
          source.json
          spec.sha
          preview.json
          summary.md
          steps/
            <pipeline-path>/
              <step>/
                <cycle>/
                  prompt.md
                  transcript.jsonl
                  bindings.json
                  outputs/
                    <output-name>
                  extra/
                  verify/
                    <n>.json
                  gate/
                    <n>.json
```

Rules:
- Attached project repos hold reviewed source specs. The UI may edit them in place; saving a spec writes to the attached repo working tree and does not auto-commit.
- `artifacts/<run>/` in the active project memory repo holds rendered prompts, transcripts, declared outputs, extra files, verify logs, gate logs, summaries, and source metadata.
- Artifact path components are harness-encoded, never raw user identifiers.
- Operational run state lives in `harness.db`; the project memory repo carries portable run evidence, not the source specs.
- A run records the active project slug, source repo path, source repo HEAD, source dirty flag, top-level spec path, spec graph SHA, and effective model/agent bindings.
- Artifacts are committed through the existing Git Backend with structured commit tags.

## SQLite State

M11 adds tables for durable run control and resume. Exact DDL can change during implementation, but the model should preserve these records.

`pipeline_runs`:
- `id` stable run id.
- `project_slug` active project at run creation.
- `source_repo_path` attached git directory containing the entrypoint spec.
- `source_repo_head` commit checked out when the run starts.
- `source_repo_dirty` whether the source repo had uncommitted changes at run start.
- `spec_path` path relative to `source_repo_path`.
- `spec_sha` hash of the loaded spec graph, including imports.
- `entrypoint` top-level pipeline name.
- `status` one of `running`, `succeeded`, `surfaced`, `cancelled`, `failed`.
- `started_at`, `updated_at`, `finished_at`.
- `surface_reason` nullable human-readable stop reason.

`pipeline_steps`:
- `run_id` parent run.
- `pipeline_path` invocation chain, including child lib calls.
- `step_name` DSL step name.
- `cycle` step entry counter.
- `attempt` model/repair attempt counter.
- `agent_name` supplied agent for this entry.
- `status` one of `running`, `ok`, `rejected`, `malfunctioned`, `surfaced`, `cancelled`.
- `prompt_artifact_path`, `transcript_artifact_path`.
- `started_at`, `updated_at`, `finished_at`.

`pipeline_bindings`:
- `run_id`, `pipeline_path`, `step_name`, `cycle`.
- `name` binding name.
- `source_kind` one of `param`, `literal`, `step_output`, `agent`.
- `source_ref` source identifier.
- `value_hash` hash of consumed data when applicable.
- `optional_absent` boolean.

`pipeline_outputs`:
- `run_id`, `pipeline_path`, `step_name`, `cycle`.
- `name` output name.
- `type` `text` or `json`.
- `artifact_path` path under the run artifact directory.
- `hash` content hash.
- `validated_at` timestamp.

`pipeline_route_events`:
- `run_id`, `pipeline_path`, `step_name`, `cycle`.
- `outcome` `ok` or `reject`.
- `reject_count` count after this event.
- `matched_route` textual normalized route.
- `target` target step or terminal action.
- `created_at` timestamp.

`pipeline_command_events`:
- `run_id`, `pipeline_path`, `step_name`, `cycle`.
- `kind` `verify` or `gate`.
- `index` declaration order.
- `argv_json` exact argv after substitution.
- `exit_code`, `duration_ms`, `output_tail`.
- `artifact_path` full command log artifact.

## Execution Phases

### 1. Discovery

The UI lists specs from `pipelines/**/*.hp` in every git directory configured on the active project. Discovery only reads files under those attached repos. It does not execute commands or call models.

Discovery validates:
- File extension is `.hp`.
- Source is UTF-8.
- Bidi override and zero-width characters are rejected by the sanitizer.
- Paths are relative to the attached source repo root.
- Case-insensitive path collisions are rejected for Windows portability.

### 2. Parse

The parser should be a small recursive-descent parser over the grammar in `DSL.md`. It should produce a typed AST that preserves source spans for UI errors.

Parse errors must report:
- File path.
- Line and column.
- Expected token or construct.
- Nearby source excerpt.

### 3. Load-Time Validation

Validation rejects specs that parse but cannot run safely.

Required checks:
- Imports resolve under the same attached source repo as the entrypoint and end in `.hp`.
- Import graph is acyclic.
- Callable graph is acyclic.
- Imported and local callable names do not collide.
- `pipeline` callables never declare agent params.
- `lib` callables do not declare local agents.
- Prompt steps have exactly one first-position agent param.
- Runs steps only declare open agent params.
- Route targets exist or are reserved terminal actions.
- Route args bind only open agent params and bind all of them exactly once.
- Data bindings type-check across `text`, `json`, and `agent`.
- Non-optional step-output bindings are definitely available on every statically knowable route path.
- Optional absent bindings resolve to empty text or `null` JSON.
- Prompt and retry placeholders refer only to visible non-agent data bindings or valid retry builtins.
- `verify` and `gate` command substitutions refer only to declared outputs of the current step.
- `reject` routes appear only when a step can produce a rejection through a gate or child propagation.
- Every callable export definitely resolves on every `done` path.
- `propagate` is invalid in top-level pipelines.
- Agent role names resolve exactly one visible agent role.
- Model names are registered.
- Literal paths resolve within the attached source repo root and reject symlink escapes.
- JSON literal sources parse before a receiving step runs.

Linter warnings are non-fatal:
- Unreachable steps.
- Missing fallback reject route.
- Optional bindings.
- Unused agents.
- Unused outputs.
- Route cycles.
- `gate` command without preceding `verify`.
- Commands that look broad or destructive.

### 4. Dry-Run Preview

Before execution, the UI shows a preview. Nothing runs before the user starts the run.

Preview includes:
- Loaded files and import graph.
- Entrypoint pipeline and params.
- Agents and resolved models.
- Steps, declared inputs, declared outputs, and routes.
- Open agent params and route-supplied agents.
- Literal paths and optional bindings.
- Verify and gate argv templates.
- Export mappings for every `lib` and `runs` step.
- Linter warnings.
- Permission notice that verify/gate commands execute locally with the user's permissions.

### 5. Run Creation

Starting a run snapshots:
- Spec graph SHA.
- Entrypoint and params.
- Active project slug.
- Source repo path, HEAD commit, dirty flag, and spec path.
- Effective agent and model resolution.
- Current permission profile.
- Harness version.

The runner creates `pipeline_runs`, writes `preview.json`, and commits an initial run artifact marker if needed. If setup fails after the run row is created, status becomes `failed` with a clear reason.

### 6. Step Execution

Every prompt step entry opens a fresh agent-loop session. Repair turns for the same entry stay in that session. A route loop into the same step increments `cycle` and opens a new session.

For each prompt step entry:
1. Resolve bindings and record consumed hashes.
2. Allocate declared output artifact paths.
3. Render the prompt and expose `$output` artifact paths to the model.
4. Run the agent loop with the supplied agent and current project sandbox.
5. Validate every declared output.
6. Run `verify` commands in declaration order, fail-fast.
7. If output validation or verify fails and retry turns remain, send retry text in the same session.
8. If retries are exhausted, surface as malfunction.
9. Run `gate` commands in declaration order, fail-fast.
10. Resolve `ok` or `reject` route.

For each runs step entry:
1. Bind child lib params from the parent scope.
2. Execute the child callable with a nested pipeline path.
3. Map child `done` to step `ok`.
4. Map child `propagate` to step `reject`.
5. Propagate child `surface` directly to the top-level run.
6. Materialize exported child outputs as the runs step outputs.

### 7. Commands

`verify` and `gate` commands execute as argv arrays, never shell strings.

Rules:
- No shell expansion, pipes, redirects, globs, or environment interpolation.
- Working directory is the attached source repo root containing the entrypoint spec.
- `$output` substitutions expand to declared output artifact paths only.
- Commands inherit the M7 approval and permission system when classified as destructive or broad.
- Command stdout/stderr are captured, truncated for UI tails, and stored fully in artifacts subject to output-size limits.
- Nonzero verify means step malfunction path.
- Nonzero gate means valid negative verdict and routes through `reject`.

### 8. Routing

Route resolution follows `DSL.md` exactly.

Rules:
- `ok -> done` completes the current callable.
- `ok -> step` enters the target step.
- `reject -> propagate` returns a negative verdict from a lib.
- `reject -> surface` checkpoints and stops for human review.
- `reject -> step` routes rejected work to a target step.
- `reject(N)` chooses the highest threshold not exceeding the current consecutive reject count.
- Missing reject fallback implicitly surfaces with a clear unhandled-rejection reason.
- Reject counters reset on `ok` and are scoped to the step, not the supplied agent.

### 9. Surfacing And Resume

Surfacing freezes a run and presents a human review card.

Surface payload includes:
- Run id, project, entrypoint, spec SHA.
- Pipeline path, step, cycle, attempt.
- Supplied agent args.
- Reject counters.
- Last verify or gate command and output tail.
- Consumed artifact hashes.
- Current output hashes.
- Short local-model summary of the step transcript.
- Optional literal `surface` message from the spec.

Resume rules:
- Refuse resume if the loaded spec graph SHA differs.
- Re-run the surfaced step's verify chain before continuing.
- If verify passes, continue through gate and routes.
- If verify fails and tracked artifacts changed, show a conflict prompt.
- If verify fails and tracked artifacts are unchanged, re-enter the normal attempt path.
- Resume, abort, and mark-fixed actions must be explicit UI actions.

### 10. Cancellation And Shutdown

Cancellation propagates to:
- Current agent-loop model request.
- Current tool call.
- Current verify/gate command process.
- Child callable execution.

On graceful shutdown:
- Mark active runs as interrupted or checkpointed.
- Flush command logs and artifact manifests.
- Flush SQLite state.
- Reconcile in-progress rows on next start.

On crash recovery:
- Detect runs in `running` status from a previous process.
- Mark them `surfaced` with an interrupted reason unless all artifacts prove the step completed cleanly.
- Never silently continue a pipeline after process death.

## UI Requirements

### Pipelines Page

The page lists `.hp` specs for the active project.

Each row shows:
- Source repo.
- Spec path.
- Entrypoint callables.
- Last lint result.
- Last run status.
- Last run time.
- Edit, save, start, preview, lint, and history actions.

The editor writes `.hp` files directly to the attached source repo working tree. Save runs sanitizer, parse, and load-time validation before writing. It does not auto-commit; the changed spec remains a normal source repo working-tree change for the user or a later pipeline step to commit.

### Lint And Preview

Lint can run without creating a run. Preview is the reviewed launch point.

The preview page shows:
- Import graph.
- Agent/model table.
- Step graph.
- Route table.
- Verify/gate command list.
- Output declaration table.
- Optional binding warnings.
- Unsafe-looking command warnings.

### Run Graph

During execution, the UI shows:
- Current step and cycle.
- Attempts and retry count.
- Supplied agent for open-agent steps.
- Reject counters.
- Matched route history.
- Streaming model transcript.
- Tool calls and tool results.
- Verify/gate command logs.
- Cancellation control.

### Artifact Browser

The artifact browser shows:
- Rendered prompt.
- Resolved bindings.
- Supplied agent.
- Model transcript.
- Declared outputs.
- Extra files.
- Verify logs.
- Gate logs.
- Consumed-cycle records.
- Content hashes.

### Surface Card

When a run surfaces, the UI shows:
- Why the run stopped.
- The failing or rejecting command.
- Output tail.
- Matched route or malfunction summary.
- Artifact hashes.
- Suggested next actions.
- Resume, re-run verify/gate, abort, and mark-fixed controls.

### Repro Bundle

Failed, rejected, or surfaced runs can export a minimal repro bundle containing:
- Spec SHA and spec files.
- Rendered prompt.
- Supplied agent args.
- Consumed artifact hashes.
- Current output hashes.
- Verify or gate output tail.
- Harness version and platform.

## Observability

M11 metrics:
- `pipeline_runs`
- `pipeline_step_attempts`
- `pipeline_rejects`
- `pipeline_surfaces`
- `pipeline_verify_duration_ms`
- `pipeline_gate_duration_ms`
- `pipeline_artifact_bytes`

Logs should include:
- Spec load and validation result.
- Run creation and status transitions.
- Step start/end.
- Attempt start/end.
- Output validation failures.
- Verify/gate argv and exit status.
- Route decisions.
- Surface and resume events.
- Cancellation and recovery events.

Log lines must carry run id, project slug, pipeline path, step name, cycle, and attempt when available.

## Security And Safety

M11 must preserve these invariants:
- Specs are data, not executable scripts.
- Verify/gate commands are explicit argv arrays and visible in preview.
- Model writes go through declared artifact paths and harness tools.
- Paths are rooted in the attached source repo or harness-controlled artifact root.
- Symlink escapes, absolute paths, `..`, Windows drive paths, UNC paths, reserved device names, and case-insensitive collisions are rejected.
- The runner never interpolates agent values into prompts or commands.
- Surface messages are literals, not interpolated templates.
- Extra model-written files are allowed only inside the step artifact directory and are audited.
- Spec SHA mismatch refuses resume.
- The DSL cannot disable harness-level loop, token, wall-clock, or permission limits.

## Implementation Sequence

### Phase 1 — Parser And Validator

- Add isolated `internal/dsl` package tree with no harness-runtime imports.
- Implement sanitizer and source graph loading in `internal/dsl/source`.
- Implement lexer and recursive-descent parser with source spans in `internal/dsl/parser`.
- Implement AST and shared diagnostics at the `internal/dsl` package boundary.
- Implement semantic validator in `internal/dsl/validate`.
- Implement lint warnings in `internal/dsl/linter`.
- Add golden tests for valid and invalid specs from `DSL.md` examples.
- Add import-boundary tests or static checks that prevent `internal/dsl` from depending on harness runtime packages.

### Phase 1b — Harness Runner Shell

- Add `internal/pipeline` package.
- Make `internal/pipeline` consume `internal/dsl` validated spec graphs.
- Keep DB, artifact, UI, command, and agent-loop dependencies out of `internal/dsl`.

### Phase 2 — Source Repo Discovery

- Add `pipelines/**/*.hp` discovery across active project directories.
- Add safe import/literal path resolution.
- Add spec graph hashing.
- Add UI lint endpoint and preview rendering.
- Add UI spec editor that writes `.hp` files back to the attached repo working tree without auto-commit.

### Phase 3 — Run State And Artifacts

- Add SQLite tables for pipeline runs, steps, bindings, outputs, route events, and command events.
- Add artifact path allocator.
- Add artifact manifest writer.
- Add crash reconciliation for in-progress runs.

### Phase 4 — Prompt Step Execution

- Integrate with `internal/agentloop`.
- Allocate declared outputs before model execution.
- Validate `text` and `json` outputs.
- Implement retry repair turns.
- Store prompts, transcripts, outputs, and extra files.

### Phase 5 — Verify, Gate, And Routing

- Add argv command runner using M7 shell/approval policy.
- Capture stdout/stderr logs and UI tails.
- Implement verify malfunction path.
- Implement gate rejection path.
- Implement reject counters and route resolution.

### Phase 6 — Lib Calls

- Implement `runs` step execution.
- Implement child export materialization.
- Implement `propagate` outcome mapping.
- Ensure child surfaces stop the whole run.

### Phase 7 — UI Runtime

- Add pipelines page.
- Add dry-run preview.
- Add live run graph.
- Add route logs.
- Add artifact browser.
- Add surface card and resume controls.
- Add repro bundle export.

### Phase 8 — Recovery And Metrics

- Add cancellation propagation.
- Add graceful shutdown behavior.
- Add crash recovery reconciliation.
- Add M11 metrics.
- Add logs/SSE events for pipeline lifecycle.

## Acceptance Tests

- [ ] Lint a valid two-file pipeline with an imported `lib` -> preview shows resolved imports, agents, steps, routes, commands, outputs, and optional bindings.
- [ ] Run the import-boundary check -> `internal/dsl` has no dependencies on harness runtime packages such as `internal/pipeline`, `internal/ui`, `internal/memory`, `internal/agentloop`, `internal/tools`, or database packages.
- [ ] Place a `.hp` spec under an attached repo's `pipelines/` tree -> the pipelines page discovers it and shows the source repo path.
- [ ] Place a `.hp` spec only under the project memory repo -> the pipelines page does not treat it as an executable source spec.
- [ ] Edit a `.hp` spec through the UI -> the attached source repo file changes, the project memory repo does not receive a source-spec copy, and the change remains uncommitted.
- [ ] Load a spec with an import cycle -> validation fails before any model call or command execution.
- [ ] Load a spec with a callable cycle -> validation fails before any model call or command execution.
- [ ] Load a spec with an unresolved agent role -> validation fails with file, line, and role name.
- [ ] Load a spec with a model name not registered in config -> validation fails with a model resolution error.
- [ ] Load a spec with a path outside the attached source repo root in an import, literal source, or derived artifact path -> validation rejects it.
- [ ] Load a spec with Windows case-insensitive namespace collisions -> validation rejects it on every platform.
- [ ] Preview a spec with broad verify/gate commands -> linter warns without blocking execution.
- [ ] Start a run -> `pipeline_runs` row is created with project slug, source repo path, source repo HEAD, dirty flag, spec path, spec SHA, entrypoint, and `running` status.
- [ ] Run a model step that writes all declared outputs -> outputs are validated and committed as run evidence under `artifacts/<run>/` in the active project memory repo.
- [ ] Run a model step that writes invalid JSON to a `json` output -> output validation fails before verify commands run.
- [ ] Run a model step that omits a declared output -> retry runs when configured; exhausted retries surface with the output-contract error.
- [ ] A failing `verify` command triggers retry, and `{last_verify.cmd}` / `{last_verify.output}` render in the repair prompt.
- [ ] A second verify command does not run after the first verify command fails.
- [ ] A failing `gate` command increments the step's reject counter and follows the highest matching `reject(N)` route.
- [ ] A step with no matching explicit reject route surfaces with an unhandled-rejection message.
- [ ] A route loop into the same step increments cycle and does not overwrite earlier artifacts.
- [ ] A child lib returns `propagate` -> the caller step resolves as `reject`.
- [ ] A child lib surfaces -> the top-level run surfaces immediately with the child pipeline path.
- [ ] A runs step with `exports report` materializes the child export as the parent step output.
- [ ] Resume a surfaced run without changing the spec -> harness re-runs verify, then gate/routes.
- [ ] Resume a surfaced run after changing the spec graph -> resume is refused because the spec SHA differs.
- [ ] A human changes tracked artifacts before resume and verify still fails -> UI shows a conflict prompt instead of re-entering the model.
- [ ] A `verify` or `gate` command with shell metacharacters is passed as argv, not shell-expanded.
- [ ] Cancel a run during a model call -> model request, tool call, and run state are cancelled cleanly.
- [ ] Cancel a run during a verify command -> command process is stopped and run state records cancellation.
- [ ] Kill the harness during a running pipeline -> next start surfaces the interrupted run instead of silently continuing.
- [ ] The UI run graph shows current step, cycle, attempts, supplied agent, reject count, and matched route history.
- [ ] The UI artifact browser shows rendered prompt, resolved bindings, supplied agent, transcript, outputs, extra files, command logs, and consumed-cycle records.
- [ ] A surfaced run can export a minimal repro bundle with spec SHA, rendered prompt, agent args, consumed hashes, output hashes, and command tail.
- [ ] M11 metrics record run count, step attempts, rejects, surfaces, verify/gate duration, and artifact bytes.

## Open Decisions

- Exact SQLite DDL and retention for old pipeline run state.
- Exact command classification heuristics for linter warnings versus approval requirements.
- Whether artifacts should be committed incrementally per step or batched at surface/success checkpoints.
- How much of the transcript to include in repro bundles by default.
- Whether a future shared pipeline template library belongs in a project memory repo or another user-controlled repo.
