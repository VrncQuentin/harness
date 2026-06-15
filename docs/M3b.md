# M3b — Projects

## Goal

Introduce a `Project` concept: a named container for rules, agents, sessions, and 0+ git directories. Projects scope memory, prompt layering, and (optionally) the active model. The previous "no project" baseline is implemented as the always-present **global project** (slug `global`), so existing single-user setups continue to work unchanged when this milestone lands — they simply run inside the global project by default.

## Dependencies

Builds on M2 (agent registry, layered prompt, hot-reload) and M3 (memory repo, sessions, git backend). Cannot land before both. Indexing of project directories is deferred to M5; M3b only stages the directory layout and ensures activation eagerly validates that configured paths are valid git repos.

---

## Concepts

### Project

Identified by an immutable lowercase-dashed **slug** (used in filesystem paths and DB keys) and a free-text **display name** (editable, what users see). All filesystem paths use the slug; never the display name. Slug is auto-generated from display name on create and editable only at that point.

A project owns:

- **Rules** — `projects/<slug>/rules.md`. Layered into the prompt between global rules and agent persona.
- **Agents** — `projects/<slug>/agents/<name>/{persona.md, rules.md, notes.md}`. Either *extends* a global agent of the same name, or is a *project-only* agent invisible elsewhere. Agent dirs hold definition only — episodes live under the project, not under the agent.
- **Sessions** — `projects/<slug>/sessions.jsonl`. Append-only, per-project.
- **Episodes** — `projects/<slug>/episodes/<agent-name>/<timestamp>.md`. Session summaries written when a session ends. Project-owned and organized by agent for retrieval; agent directories no longer host episodes. Their embeddings (M5) live at `projects/<slug>/index/_episodes/`, alongside the embeddings of attached directories — every indexable tree is a peer entry under the project's `index/`.
- **Directories** — 0+ paths to git repos on disk. Indexed independently. Belong to exactly one project (no sharing in this milestone).
- **Optional model overrides** — nullable per-project copies of model config fields. NULL means inherit from global.

A project has a `hidden` boolean. Hidden projects are excluded from pickers and switchers; their data is preserved on disk. Hard delete is deferred to a later iteration.

### Global project (`slug = "global"`)

The system-default project, always present, seeded on first run. Acts as the active project when the user has not selected a more specific one — what was previously called "no project" mode is implemented as `active_project_slug = "global"`.

Special properties:

- Always exists: row is seeded on first run and cannot be deleted or hidden.
- Slug is reserved and immutable; display name is editable.
- Can carry model overrides like any other project (rare in practice — defaults inherit from `config`).

Behavior:

- Sessions are written to `projects/global/sessions.jsonl`.
- Project rules from `projects/global/rules.md` are layered if the file exists; by default it does not.
- Only the global agents library (top-level `agents/`) is visible by default; if the user populates `projects/global/agents/`, those layer on top like any other project.
- Always defaults to a fresh session on activation; the previous-session picker for the global project is deferred to a later iteration.

The user can return to the global project at any time from the project switcher.

### Indexable tree

A git directory belonging to a project. Walked via `go-git`. One vector file per tree, located at `projects/<slug>/index/<dir-slug>/{vectors.bin, manifest.json}`. Invalidation aligns with git refs — a new HEAD commit in tree A does not dirty tree B's index. Non-git directories are out of scope for this milestone.

---

## Layered Prompt

The prompt assembler adds a project tier between global and agent:

```
1. global/rules.md                       — never trimmed
2. global/user.md                        — never trimmed
3. projects/<slug>/rules.md              — never trimmed (only when a project is active)
4. agents/<name>/persona.md (resolved)   — never trimmed
5. agents/<name>/rules.md (resolved)     — never trimmed
6. global/facts.md                       — keep lean
7. agents/<name>/notes.md (resolved)     — keep lean
8. retrieved episodes (project-scoped)   — top-K, trimmed oldest-first
9. conversation turns                    — current session
```

**Agent resolution rule:** if the active project has `agents/<name>/<file>.md`, use it; otherwise fall back to the top-level `agents/<name>/<file>.md` (global agents library). Resolution is per-file, so a project agent can override `persona.md` while inheriting the global `rules.md`.

In the global project, layer 3 is skipped unless `projects/global/rules.md` has been authored (uncommon).

---

## Agent Visibility

| Context           | Visible agents                                  |
|-------------------|-------------------------------------------------|
| In project DT     | Globals + DT-only agents + DT extensions        |
| In global project | Globals only (unless `projects/global/agents/` is populated) |

The `/agents` and `/projects/<slug>` views badge each agent: `global`, `extends global`, `project-only`. Project-only agents are invisible outside their owning project.

---

## Sessions

- A session is bound to its project at creation (slug is always set, since `global` is itself a project). The binding is immutable — sessions never migrate between projects.
- The session record carries `project: <slug>` in its header, in addition to the path encoding.
- Switching projects always ends the active session; the destination project starts a fresh one.
- Default on activation is to start a new session. In user-created projects, a previous-session picker is available. In the global project the picker is deferred — sessions still write to `projects/global/sessions.jsonl` so the data is there when the picker arrives.

---

## Project Switch Behavior

A new config field, `project.llama_on_switch`, controls how the **llama-server process** is handled when the active project changes. The name and column explicitly target the llama-server lifecycle (rather than implying behavior through the value alone) so future fields covering the embedder, API server, or other subsystems can coexist without ambiguity.

| `llama_on_switch` | Behavior                                                                                  |
|-------------------|-------------------------------------------------------------------------------------------|
| `reload`          | Default. Drain queue, unload current model, load destination project's effective model. Visible loading state in UI. |
| `keep`            | Current llama-server keeps running. Destination project's prompts are served by whatever model is loaded. UI surfaces the mismatch ("running: X / project preference: Y"). Manual reload required to actually switch the model. |

The switch is a no-op for the llama-server regardless of mode if the destination project's effective model config equals the currently running model.

The embedder lifecycle is unaffected by this setting — there is one global embedder shared across all projects in M3b.

`spawn_per_project` (one llama-server per project, port pool, idle eviction) is deferred. See "Future Iterations".

---

## Per-Project Model Overrides

Each project may specify any subset of:

- `model_binary`
- `model_path`
- `model_ctx_size`
- `model_gpu_layers`
- `model_n_parallel`

Stored as nullable columns in the `projects` table. NULL means "inherit from global." Effective value = `COALESCE(project, global)`.

**Out of scope** for this milestone: per-project ports (model and embedder ports stay global), per-project embedder (one global embedder serves all projects).

---

## Schema Changes (`harness.db`)

### `projects` (new, multi-row)

| Column             | Type      | Notes                                              |
|--------------------|-----------|----------------------------------------------------|
| `slug`             | TEXT PK   | Immutable, lowercase-dashed, dir-safe              |
| `display_name`     | TEXT      | Editable                                           |
| `model_binary`     | TEXT      | Nullable; inherits from `config` when NULL         |
| `model_path`       | TEXT      | Nullable                                           |
| `model_ctx_size`   | INTEGER   | Nullable                                           |
| `model_gpu_layers` | INTEGER   | Nullable                                           |
| `model_n_parallel` | INTEGER   | Nullable                                           |
| `hidden`           | BOOLEAN   | Default 0                                          |
| `created_at`       | TIMESTAMP | Set on insert                                      |
| `saved_at`         | TIMESTAMP | Updated on edit                                    |

A row with `slug = "global"` is seeded on first run and is treated as a system row: cannot be deleted, cannot be hidden, slug is locked. Display name and model overrides remain editable.

### `project_directories` (new)

| Column         | Type    | Notes                                              |
|----------------|---------|----------------------------------------------------|
| `project_slug` | TEXT    | FK → `projects.slug`, ON DELETE CASCADE            |
| `path`         | TEXT    | Absolute path on disk                              |

Composite PK `(project_slug, path)`.

### `config` additions

| Column                     | Type    | Notes                                                                                       |
|----------------------------|---------|---------------------------------------------------------------------------------------------|
| `active_project_slug`      | TEXT    | NOT NULL, default `'global'`. Always references a row in `projects`.                        |
| `project_llama_on_switch`  | TEXT    | `'keep' \| 'reload'`, default `'reload'`. Controls llama-server lifecycle on project switch.|

### Sessions

The session record gains a `project` field — always set to the active project's slug, never null (since `global` is itself a project). The path encoding (`projects/<slug>/sessions.jsonl`) and the in-record `project: <slug>` field always agree.

---

## Memory Repo Layout

Two structural changes from the M3 layout:

1. The previous top-level `index/` and `runtime/` directories fold into `projects/global/`.
2. Episodes move out of `agents/<name>/episodes/` and into `projects/<slug>/episodes/<agent-name>/`. Agent directories now hold definition only (persona, rules, notes); episodes are session artifacts and belong to the project that owned the session.

Cross-project base content (`global/` rules/user/facts and the global `agents/` library) stays at the top level.

```
memory/
  global/                              (unchanged — cross-project base content)
    rules.md
    user.md
    facts.md
  agents/                              (global agents library — definition only, no episodes)
    <name>/
      persona.md
      rules.md
      notes.md
  projects/                            ← NEW
    global/                            ← system project; replaces previous top-level `index/` and `runtime/`
      sessions.jsonl                       (was runtime/sessions.jsonl)
      queue.wal                            (was runtime/queue.wal)
      episodes/                            (was agents/<name>/episodes/, now project-owned)
        <agent-name>/
          <timestamp>.md
      index/                               (was top-level index/)
        _episodes/                         (M5: embeddings of this project's episodes; reserved slot)
          vectors.bin
          manifest.json
        <dir-slug>/                        (M5: embeddings of one attached directory)
          vectors.bin
          manifest.json
    <slug>/                            ← user-created projects
      rules.md
      agents/
        <name>/
          persona.md
          rules.md
          notes.md
      sessions.jsonl
      queue.wal
      episodes/
        <agent-name>/
          <timestamp>.md
      index/
        _episodes/
          vectors.bin
          manifest.json
        <dir-slug>/
          vectors.bin
          manifest.json
```

Each indexable tree gets its own subdirectory under the project's `index/` so refresh, rebuild, and removal are localized. Episode embeddings sit in the reserved `_episodes/` slot alongside `<dir-slug>/` entries for attached directories — both are peer indexable trees from the embedder's perspective, just with different sources (the project's own `episodes/` subtree vs. an external git repo). The leading underscore marks `_episodes` as system-reserved so it cannot collide with a user-chosen directory slug. Every project — including `global` — owns its own `sessions.jsonl`, `queue.wal`, `episodes/`, and `index/`, so there is no special-cased path in the memory layer. Episode retrieval defaults to the active project's own `episodes/<agent-name>/` directory; cross-agent reads within a project remain explicit (per the existing M6 design).

---

## Activation

When a project is activated (selected via UI, or loaded from `active_project_slug` on startup):

1. **Eager directory check.** All configured directories must exist and be valid git repos (resolved via `go-git`). Missing or invalid directories are flagged with a per-project UI badge ("directory missing"); queries against those trees skip silently. Activation still succeeds — the project is usable, the user is told which directories are unhealthy.
2. **Session start.** A fresh session is created in `projects/<slug>/sessions.jsonl`. Previous sessions remain available via the picker.
3. **llama-server swap (if needed).** Per `project.llama_on_switch`:
   - `reload` — drain queue, stop llama-server, start with the project's effective model config. Skip if the effective config matches the currently running model.
   - `keep` — no llama-server action. UI surfaces the mismatch when the running model differs from the project's preferred model.
4. **Index check.** For each tree, compare HEAD commit against the manifest. The actual embedding refresh is part of M5 once the embedder pipeline lands; in M3b this step is a placeholder that records HEAD per tree.

Switching to the global project runs the same flow — there is no special path. Sessions then write to `projects/global/sessions.jsonl`, and the model swap follows `llama_on_switch` against the global project's effective config (which inherits from `config` unless overridden).

---

## UI Surfaces

### `/projects` (new page)
- Lists all non-hidden projects (display name, slug, directory count). Toggle to show hidden.
- The `global` row is always present at the top, marked as the system project (no Hide / Delete actions; slug is locked).
- **Create**: form with display name (slug auto-generated, editable), initial directory list, optional model overrides.
- **Edit**: same fields, slug read-only.
- **Hide / unhide** action (disabled for `global`).
- Per-project indicator for missing directories.

### Topbar
- Project switcher dropdown showing the active project and the list of non-hidden projects (with `global` always at the top, labeled "Global").
- Switching triggers the activation sequence; UI shows a loading state during `reload`.

### `/config`
- New "Project" section. `llama_on_switch` selector with caveats inline:
  - `keep` — "llama-server keeps running. Faster switch, but the new project's preferred model is ignored until manual reload."
  - `reload` — "Drains queue, unloads current model, loads new project's model. Slower switch, correct behavior."

### `/agents`
- Scoped to the active project.
- Each agent badged: `global`, `extends global`, `project-only`.
- In the global project, only the global agents library is listed by default.

### Status page
- Shows active project (always set, never empty — defaults to "Global").
- Per-project missing-directory warnings.
- Currently loaded model + active project's preferred model. Highlight when `keep` has caused a mismatch.

---

## Acceptance Tests

- [ ] First run → `projects` table seeded with a `global` row; `active_project_slug = 'global'`; `memory/projects/global/` exists with empty `sessions.jsonl` and (lazily) `queue.wal`
- [ ] Attempt to delete or hide the `global` project → blocked by UI and DB constraint
- [ ] Attempt to rename the `global` slug → blocked; display name remains editable
- [ ] Create a project via UI → row appears in `projects` table, directory created at `memory/projects/<slug>/`, slug auto-generated from display name and editable on create
- [ ] Edit a project → display name and overrides change, slug is read-only
- [ ] Activate a project → `active_project_slug` updated, fresh session opens at `projects/<slug>/sessions.jsonl` with `project: <slug>` in the header
- [ ] Switch from project A to project B (default `reload` mode) → queue drains, llama-server restarts with B's effective model
- [ ] Switch with `llama_on_switch = keep` → llama-server keeps running, status page shows model mismatch when applicable
- [ ] Switch between two projects with identical effective model config → no llama-server restart, regardless of mode
- [ ] Switch to the global project from a user project → subsequent sessions write to `projects/global/sessions.jsonl` with `project: global`, only the global agents library is visible
- [ ] Activate a project whose configured directory is missing → activation succeeds, UI shows "directory missing" badge for that tree, no crash
- [ ] Create `projects/dt/agents/trader/persona.md` with no global counterpart → agent visible in DT, invisible in the global project and other projects
- [ ] Create `projects/dt/agents/coder/persona.md` with top-level `agents/coder/persona.md` present → DT-active prompt uses DT's persona; switching to the global project reverts to the global persona
- [ ] Set per-project `model_path` → activating that project loads the override; activating the global project reverts to its effective config (inherited from `config` unless overridden)
- [ ] Hide a non-global project → it disappears from the topbar switcher and the default `/projects` list, data and sessions remain on disk
- [ ] Unhide a project → re-appears in pickers, all data intact
- [ ] Project rules file present → `projects/<slug>/rules.md` is injected into the prompt between global rules and agent persona (verify via logs page token breakdown)
- [ ] Complete a session in a user project with agent `coder` → episode file appears at `projects/<slug>/episodes/coder/<timestamp>.md` (and **not** at `agents/coder/episodes/` — that path no longer exists)
- [ ] Complete a session in the global project with agent `coder` → episode file appears at `projects/global/episodes/coder/<timestamp>.md`
- [ ] Switch projects mid-session → active session ends, new fresh session starts in destination project; previous session still reachable via picker
- [ ] First-run user with no user-created projects → remains in the global project indefinitely, no auto-seeded "Untitled" project beyond `global` itself
- [ ] Two directories configured for one project → both create distinct subdirectories at `projects/<slug>/index/<dir-slug>/` (manifest only in M3b; vectors in M5)
- [ ] Restart harness → `active_project_slug` is honored on startup, activation runs, eager directory check executes, status page reflects active project

---

## Future Iterations

Deliberately out of scope for M3b. Listed here so they don't get lost.

- **`spawn_per_project` switch mode.** One llama-server per project, ports drawn from a configurable pool, API server routing per active project, idle-eviction policy (LRU-style auto-stop after N minutes of inactivity). Significant escalation in `proc/` and `api/` complexity.
- **Hard project deletion.** Currently only the `hidden` flag exists. Real deletion (drop DB rows, `rm -rf memory/projects/<slug>/`, clean indexes) needs a confirmation flow and ideally an undo affordance.
- **Previous-session picker for the global project.** Sessions are written to `projects/global/sessions.jsonl` but the UI does not surface them for resumption yet.
- **Cross-project search.** Fan-out + merge across project indexes. Rare in practice; trivial to add when needed.
- **Shared directories across projects.** Requires reference counting on the index and per-project tagging on retrieval results.
- **Per-project embedder and per-project ports.** Currently embedder and all ports stay global.
- **Promote project-specific agent to global.** UI affordance to copy `projects/<slug>/agents/<name>/` to `agents/<name>/`.
- **Non-git directories.** Indexing arbitrary trees by mtime + content hash; out of scope while everything stays git-backed.
- **Pipeline run artifacts.** M9 adds `projects/<slug>/artifacts/` for pipeline run evidence. `.hp` source specs live in attached project git directories, not in the memory repo. M3b deliberately stops at projects, agents, sessions, episodes, directories, and index layout.
