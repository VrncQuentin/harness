# M3b — Projects

## Goal

Introduce a `Project` concept: a named container for rules, agents, sessions, and 0+ git directories. Projects scope memory, prompt layering, and (optionally) the active model. `(no project)` mode is a permanent, first-class state — existing single-user setups continue to work unchanged when this milestone lands.

## Dependencies

Builds on M2 (agent registry, layered prompt, hot-reload) and M3 (memory repo, sessions, git backend). Cannot land before both. Indexing of project directories is deferred to M5; M3b only stages the directory layout and ensures activation eagerly validates that configured paths are valid git repos.

---

## Concepts

### Project

Identified by an immutable lowercase-dashed **slug** (used in filesystem paths and DB keys) and a free-text **display name** (editable, what users see). All filesystem paths use the slug; never the display name. Slug is auto-generated from display name on create and editable only at that point.

A project owns:

- **Rules** — `projects/<slug>/rules.md`. Layered into the prompt between global rules and agent persona.
- **Agents** — `projects/<slug>/agents/<name>/`. Either *extends* a global agent of the same name, or is a *project-only* agent invisible elsewhere.
- **Sessions** — `projects/<slug>/sessions.jsonl`. Append-only, per-project.
- **Directories** — 0+ paths to git repos on disk. Indexed independently. Belong to exactly one project (no sharing in this milestone).
- **Optional model overrides** — nullable per-project copies of model config fields. NULL means inherit from global.

A project has a `hidden` boolean. Hidden projects are excluded from pickers and switchers; their data is preserved on disk. Hard delete is deferred to a later iteration.

### `(no project)` mode

The default state. No active project is selected.

- Sessions are written to `runtime/sessions.jsonl` (existing M3 location).
- Only global agents are available.
- Project rules are not applied.
- Always defaults to a fresh session; the picker for previous sessions is deferred.

This mode is permanent — it does not disappear once projects exist. The user can return to it at any time.

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

**Agent resolution rule:** if the active project has `agents/<name>/<file>.md`, use it; otherwise fall back to the global `agents/<name>/<file>.md`. Resolution is per-file, so a project agent can override `persona.md` while inheriting the global `rules.md`.

In `(no project)` mode, layer 3 is omitted and only global agents are visible.

---

## Agent Visibility

| Context        | Visible agents                                  |
|----------------|-------------------------------------------------|
| In project DT  | Globals + DT-only agents + DT extensions        |
| In `(no proj)` | Globals only                                    |

The `/agents` and `/projects/<slug>` views badge each agent: `global`, `extends global`, `project-only`. Project-only agents are invisible outside their owning project.

---

## Sessions

- A session is bound to its project (or to `(no project)`) at creation. The binding is immutable — sessions never migrate between projects.
- The session record carries `project: <slug>` (or `null`) in its header, in addition to the path encoding.
- Switching projects always ends the active session; the destination project starts a fresh one.
- Default on activation is to start a new session. Inside projects, a previous-session picker is available. In `(no project)` the picker is deferred — sessions still write to `runtime/sessions.jsonl` so the data is there when the picker arrives.

---

## Project Switch Behavior

A new global config field, `project.on_switch`, controls what happens to the running model when the active project changes.

| Mode      | Behavior                                                                                  |
|-----------|-------------------------------------------------------------------------------------------|
| `reload`  | Default. Drain queue, unload current model, load destination project's effective model. Visible loading state in UI. |
| `keep`    | Current model keeps running. Destination project's prompts are served by whatever model is loaded. UI surfaces the mismatch ("running: X / project preference: Y"). Manual reload required to actually switch the model. |

The switch is a no-op for the model regardless of mode if the destination project's effective model config equals the currently running model.

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

### `project_directories` (new)

| Column         | Type    | Notes                                              |
|----------------|---------|----------------------------------------------------|
| `project_slug` | TEXT    | FK → `projects.slug`, ON DELETE CASCADE            |
| `path`         | TEXT    | Absolute path on disk                              |

Composite PK `(project_slug, path)`.

### `config` additions

| Column                | Type    | Notes                                |
|-----------------------|---------|--------------------------------------|
| `active_project_slug` | TEXT    | Nullable; NULL means `(no project)`  |
| `project_on_switch`   | TEXT    | `'keep' \| 'reload'`, default `'reload'` |

### Sessions

The session record gains a `project` field (slug string or `null`). For sessions stored in `runtime/sessions.jsonl`, this is always `null`. For sessions in `projects/<slug>/sessions.jsonl`, it matches the slug.

---

## Memory Repo Layout (additions)

```
memory/
  global/         (unchanged)
  agents/         (unchanged)
  index/          (unchanged — global agents' episodes still indexed here)
  runtime/
    sessions.jsonl    (used by `(no project)` mode)
    queue.wal
  projects/                            ← NEW
    <slug>/
      rules.md
      agents/
        <name>/
          persona.md
          rules.md
          notes.md
          episodes/
      sessions.jsonl
      index/
        <dir-slug>/
          vectors.bin
          manifest.json
```

Each indexable tree gets its own subdirectory under the project's `index/` so refresh, rebuild, and removal are localized.

---

## Activation

When a project is activated (selected via UI, or loaded from `active_project_slug` on startup):

1. **Eager directory check.** All configured directories must exist and be valid git repos (resolved via `go-git`). Missing or invalid directories are flagged with a per-project UI badge ("directory missing"); queries against those trees skip silently. Activation still succeeds — the project is usable, the user is told which directories are unhealthy.
2. **Session start.** A fresh session is created in `projects/<slug>/sessions.jsonl`. Previous sessions remain available via the picker.
3. **Model swap (if needed).** Per `project.on_switch`:
   - `reload` — drain queue, stop llama-server, start with the project's effective model config. Skip if the effective config matches the currently running model.
   - `keep` — no model action. UI surfaces the mismatch when the running model differs from the project's preferred model.
4. **Index check.** For each tree, compare HEAD commit against the manifest. The actual embedding refresh is part of M5 once the embedder pipeline lands; in M3b this step is a placeholder that records HEAD per tree.

Switching to `(no project)` is symmetric: drain queue if `reload`, swap to global model config, write subsequent sessions to `runtime/sessions.jsonl`.

---

## UI Surfaces

### `/projects` (new page)
- Lists all non-hidden projects (display name, slug, directory count). Toggle to show hidden.
- **Create**: form with display name (slug auto-generated, editable), initial directory list, optional model overrides.
- **Edit**: same fields, slug read-only.
- **Hide / unhide** action.
- Per-project indicator for missing directories.

### Topbar
- Project switcher dropdown showing the active project (or "(no project)") and the list of non-hidden projects.
- Switching triggers the activation sequence; UI shows a loading state during `reload`.

### `/config`
- New "Project" section. `on_switch` selector with caveats inline:
  - `keep` — "Current model keeps running. Faster switch, but the new project's preferred model is ignored until manual reload."
  - `reload` — "Drains queue, unloads current model, loads new project's model. Slower switch, correct behavior."

### `/agents`
- Scoped to the active project.
- Each agent badged: `global`, `extends global`, `project-only`.
- In `(no project)`, only globals are listed.

### Status page
- Shows active project (or `(no project)`).
- Per-project missing-directory warnings.
- Currently loaded model + active project's preferred model. Highlight when `keep` mode has caused a mismatch.

---

## Acceptance Tests

- [ ] Create a project via UI → row appears in `projects` table, directory created at `memory/projects/<slug>/`, slug auto-generated from display name and editable on create
- [ ] Edit a project → display name and overrides change, slug is read-only
- [ ] Activate a project → `active_project_slug` updated, fresh session opens at `projects/<slug>/sessions.jsonl` with `project: <slug>` in the header
- [ ] Switch from project A to project B (default `reload` mode) → queue drains, llama-server restarts with B's effective model
- [ ] Switch with `on_switch = keep` → llama-server keeps running, status page shows model mismatch when applicable
- [ ] Switch between two projects with identical effective model config → no llama-server restart, regardless of mode
- [ ] Switch to `(no project)` from a project → subsequent sessions write to `runtime/sessions.jsonl` with `project: null`, only global agents are visible
- [ ] Activate a project whose configured directory is missing → activation succeeds, UI shows "directory missing" badge for that tree, no crash
- [ ] Create `projects/dt/agents/trader/persona.md` with no global counterpart → agent visible in DT, invisible in `(no project)` and other projects
- [ ] Create `projects/dt/agents/coder/persona.md` with global `agents/coder/persona.md` present → DT-active prompt uses DT's persona; switching to `(no project)` reverts to the global persona
- [ ] Set per-project `model_path` → activating that project loads the override; activating `(no project)` reverts to the global `model_path`
- [ ] Hide a project → it disappears from the topbar switcher and the default `/projects` list, data and sessions remain on disk
- [ ] Unhide a project → re-appears in pickers, all data intact
- [ ] Project rules file present → `projects/<slug>/rules.md` is injected into the prompt between global rules and agent persona (verify via logs page token breakdown)
- [ ] Switch projects mid-session → active session ends, new fresh session starts in destination project; previous session still reachable via picker
- [ ] First-run user with no projects → stays in `(no project)` indefinitely, no auto-seeded "Untitled" project
- [ ] Two directories configured for one project → both create distinct subdirectories at `projects/<slug>/index/<dir-slug>/` (manifest only in M3b; vectors in M5)
- [ ] Restart harness with `active_project_slug` set → activation runs on startup, eager directory check executes, status page reflects active project

---

## Future Iterations

Deliberately out of scope for M3b. Listed here so they don't get lost.

- **`spawn_per_project` switch mode.** One llama-server per project, ports drawn from a configurable pool, API server routing per active project, idle-eviction policy (LRU-style auto-stop after N minutes of inactivity). Significant escalation in `proc/` and `api/` complexity.
- **Hard project deletion.** Currently only the `hidden` flag exists. Real deletion (drop DB rows, `rm -rf memory/projects/<slug>/`, clean indexes) needs a confirmation flow and ideally an undo affordance.
- **Previous-session picker in `(no project)`.** Sessions are written to `runtime/sessions.jsonl` but the UI does not surface them for resumption yet.
- **Cross-project search.** Fan-out + merge across project indexes. Rare in practice; trivial to add when needed.
- **Shared directories across projects.** Requires reference counting on the index and per-project tagging on retrieval results.
- **Per-project embedder and per-project ports.** Currently embedder and all ports stay global.
- **Promote project-specific agent to global.** UI affordance to copy `projects/<slug>/agents/<name>/` to `agents/<name>/`.
- **Non-git directories.** Indexing arbitrary trees by mtime + content hash; out of scope while everything stays git-backed.
