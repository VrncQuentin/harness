# AGENTS.md

This file is the entry point for coding agents. Read it before touching anything.

## Project

This repository is now a Pi Coding package for persistent project memory and Pi Web UI integrations.

- Active docs: `docs/architecture.md`, `docs/roadmap.md`, `docs/agents.md`
- Language: TypeScript
- Platform: Pi Coding extensions, skills, prompts, and optional Pi Web UI modules
- Archived implementation: `archived/go-harness/`

The archived Go-native harness is retained for reference only. Do not extend it unless the user explicitly asks to work on archived code.

## Repository Structure

```text
src/                  Pi extension source
skills/               Agent Skills loaded by Pi
prompts/              Pi prompt templates
docs/                 Active Pi package architecture and roadmap
archived/go-harness/  Previous Go harness docs and implementation
```

## Rules

### Git

- Always work on a feature branch, never directly on `main`.
- Branch naming: `feat/<short-description>` or `fix/<short-description>`.
- Commit small logical changes with clear messages.
- When the task is complete, open a PR against `main`.
- Do not merge. Wait for the user to explicitly say so.

### General

- Read `docs/architecture.md` before implementation work.
- Check `docs/roadmap.md` for the current milestone and acceptance tests.
- Prefer Pi extension hooks and Pi package resources over custom runtime services.
- Do not rebuild Pi's agent loop, tools, session storage, providers, or UI primitives.
- If a required Pi API is missing or unclear, document the gap before introducing custom infrastructure.

### TypeScript

- Keep extension code small and explicit.
- Use Pi's published extension APIs and types.
- Runtime dependencies belong in `dependencies`; Pi peer packages belong in `peerDependencies` with `*` ranges when publishing.
- Avoid global mutable state unless it is tied to Pi extension lifecycle events and cleaned up on reload/shutdown.
- Propagate cancellation with `ctx.signal` when starting abort-aware async work inside Pi turn events.

### Memory

- Store user-editable memory as Markdown under `.pi-memory/` until a roadmap milestone introduces indexing.
- Inject memory through Pi lifecycle hooks; do not create a separate API proxy or daemon for prompt assembly.
- Keep injected context bounded and inspectable.
- Prefer explicit promotion commands/tools over silent memory mutation.

## Current Milestone

Work on M1 in `docs/roadmap.md`: Pi package pivot and scaffold.
