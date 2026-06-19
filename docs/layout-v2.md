# Layout V2

## Goal

Move from one configured memory repo containing every project to a harness home with one git-backed memory repo per project. The harness remains a global resident desktop program: project activation is explicit through the UI/config state, never inferred from the process working directory.

This milestone preserves the existing pillars:
- Config, metrics, and runtime control state stay in SQLite.
- Memory stays git-backed; git history remains the durable index boundary.
- The harness has no CLI mode and no cwd-driven behavior.
- Multi-repo projects attach multiple git code repos to one project memory repo.

## Harness Home

Default location:

```text
~/.harness/
  harness.db                  config + metrics + runtime state
  projects/
    global/                   global project memory repo
    <project-id>/              one memory repo per user project
  logs/
  cache/
```

`harness.db`, `logs/`, and `cache/` are machine-local and are not committed. Every directory under `~/.harness/projects/` is a separate git repo managed through `go-git`.

The home directory is resolved at startup before SQLite opens. A later implementation may allow an advanced override, but the default remains local and user-owned.

## Global Project Repo

`global` is a first-class project repo, not a separate top-level profile repo.

```text
~/.harness/projects/global/
  .git/
  rules.md
  user.md
  facts.md
  agents/
    <name>/
      persona.md
      rules.md
      notes.md
  sessions.jsonl
  episodes/
    <agent>/
      <timestamp>.md
  index/
    _episodes/
      vectors.bin
      manifest.json
  queue.wal
  artifacts/
```

The `global` repo is semantically special because it supplies base prompt layers and the fallback agent library. It is not structurally special: it has the same session, episode, index, queue, and artifact layout as any other project memory repo.

## User Project Memory Repo

Each user project has one memory repo:

```text
~/.harness/projects/<project-id>/
  .git/
  rules.md
  agents/
    <name>/
      persona.md
      rules.md
      notes.md
  sessions.jsonl
  episodes/
    <agent>/
      <timestamp>.md
  index/
    _episodes/
      vectors.bin
      manifest.json
    <dir-slug>/
      vectors.bin
      manifest.json
  queue.wal
  artifacts/
    <run>/
```

The project memory repo stores harness-owned memory and evidence. It does not contain the source code repos unless the user deliberately chooses a source repo as the memory repo directory during project creation.

## Attached Code Repos

A project can attach any number of code repos through the `project_directories` table. Each attached directory must be a git repo.

```text
<code-repo>/
  .git/
  pipelines/
    *.hp
    **/*.hp
```

Attached code repos are indexed by git state. A new HEAD in one attached repo invalidates only that repo's index slot under the active project memory repo:

```text
~/.harness/projects/<project-id>/index/<dir-slug>/
```

This keeps multi-repo projects unified without requiring runtime memory to be written into every source repo.

## Prompt Layering

The prompt assembler resolves layers from the global project repo and the active project memory repo:

```text
1. projects/global/rules.md                    always injected, never trimmed
2. projects/global/user.md                     always injected, never trimmed
3. projects/<active>/rules.md                  skipped when active == global
4. resolved agent persona.md                   active project overrides global per file
5. resolved agent rules.md                     active project overrides global per file
6. projects/global/facts.md                    keep lean
7. resolved agent notes.md                     keep lean
8. active project retrieved episodes           project-scoped by default
9. conversation turns
```

Agent resolution is per file:

```text
projects/<active>/agents/<name>/<file>
fallback to:
projects/global/agents/<name>/<file>
```

For the global project, the resolver reads directly from `projects/global/agents/`.

## Create Project Flow

Project creation always produces or selects a git repo for memory:

1. Directory provided and has `.git`: use it as the project memory repo.
2. Directory provided and has no `.git`: initialize it with `go-git` and use it.
3. No directory provided: create `~/.harness/projects/<project-id>/`, initialize it with `go-git`, and use it.
4. After creation: optionally offer "Back up to GitHub?". This opt-in path shells to the logged-in `gh` binary and is isolated from core memory operations.

The GitHub backup path is deliberately external and optional. Core project creation stays local, offline, and dependency-free beyond the harness-managed Go code.

## SQLite State

`harness.db` remains the source of truth for machine-local operational state:
- Config values and saved-at state.
- Active project slug.
- Project identity rows.
- Attached code repo paths.
- Metrics and runtime tables.
- Pipeline run control state.

The `projects` table keeps project identity and effective model overrides. Layout v2 changes storage location semantics, not the need for the table. Each project row points to or resolves a project memory repo path.

## Migration From Single Memory Repo

Existing M3/M3b layout:

```text
memory/
  global/
  agents/
  projects/
    global/
    <slug>/
```

Layout v2 destination:

```text
~/.harness/projects/global/
~/.harness/projects/<slug>/
```

Migration rules:
- Move `memory/global/{rules.md,user.md,facts.md}` into `projects/global/`.
- Move top-level `memory/agents/` into `projects/global/agents/`.
- Move `memory/projects/global/{sessions.jsonl,queue.wal,episodes,index,artifacts}` into `projects/global/`.
- Move each `memory/projects/<slug>/` to `~/.harness/projects/<slug>/` as its own git repo.
- Preserve project slugs, session records, episode paths within each destination repo, commit history where feasible, and attached directory rows.

If preserving the single old repo history across split repos is impractical, the migration must write an explicit import commit in each destination repo and leave the old repo untouched.

## Acceptance Tests

- First run creates `~/.harness/harness.db` and initializes `~/.harness/projects/global` as a git repo.
- Creating a project with no directory creates and initializes `~/.harness/projects/<id>`.
- Creating a project with a non-git directory initializes that directory with `go-git` and uses it as the memory repo.
- Creating a project with an existing git directory uses it without reinitializing or rewriting unrelated files.
- The global project can be backed up through the same opt-in GitHub flow as user projects.
- Starting the harness never depends on cwd and never activates a project based on the launch directory.
- One project with two attached code repos writes sessions and episodes to one project memory repo and creates separate index entries for each attached repo.
- Agent resolution falls back from `projects/<active>/agents/<name>/<file>` to `projects/global/agents/<name>/<file>` per file.
- Promoting a global fact appends to `projects/global/facts.md` and commits in the global repo.
- Completing a session in a user project writes and commits `episodes/<agent>/<timestamp>.md` in that project's memory repo.
- Pipeline discovery reads `.hp` files from attached code repos, not from project memory repos.
- Pipeline run artifacts are committed under `artifacts/<run>/` in the active project memory repo.
- `harness.db`, logs, and cache files are never committed to any project memory repo.
