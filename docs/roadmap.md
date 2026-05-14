# Roadmap

Each milestone should end with a usable Pi package state. Do not reintroduce standalone harness services unless a Pi extension hook cannot support the requirement.

## M1 - Pi Package Pivot

Goal: the repository becomes a Pi package scaffold and the old Go implementation is archived.

- [x] Archive Go harness docs and code under `archived/go-harness/`.
- [x] Add package metadata with Pi resource manifest.
- [x] Add a minimal extension that loads in Pi.
- [x] Add initial memory skill and prompt template.
- [x] Rewrite active architecture and roadmap docs for Pi.

Acceptance tests:
- [ ] `npm install` succeeds.
- [ ] `npm run check` succeeds.
- [ ] Pi can load `src/extension.ts` with `pi -e ./src/extension.ts`.
- [ ] `/memory-status` reports missing or present `.pi-memory` clearly.

## M2 - Layered Memory Injection

Goal: Pi sessions receive predictable project memory without replacing Pi context handling.

- [ ] Formalize `.pi-memory/` layout.
- [ ] Add configurable layer ordering and size limits.
- [ ] Add diagnostics for loaded layers.
- [ ] Add tests for missing files, empty files, and ordering.

Acceptance tests:
- [ ] A prompt in a project with `.pi-memory/global/rules.md` receives those rules.
- [ ] Missing optional memory files do not fail startup.
- [ ] Oversized memory produces a clear warning and bounded injection.

## M3 - Session Summaries

Goal: durable memory is produced from Pi sessions.

- [ ] Hook `agent_end` or `session_shutdown` for summary capture.
- [ ] Store summaries as Markdown under `.pi-memory/projects/<name>/episodes/`.
- [ ] Add `/memory-summarize` and `/memory-promote` commands.
- [ ] Keep Pi session JSONL as the source of detailed conversation truth.

Acceptance tests:
- [ ] Ending a session can write a summary file.
- [ ] Promotion appends to a facts or notes file with user-visible confirmation.
- [ ] Reloading Pi picks up promoted memory on the next prompt.

## M4 - Pi Web UI Integration

Goal: browser surfaces use Pi Web UI components.

- [ ] Add a web UI package using `@earendil-works/pi-web-ui`.
- [ ] Display memory layers, summaries, and promotion actions.
- [ ] Register custom message or tool renderers where useful.

Acceptance tests:
- [ ] Web UI runs without Go services.
- [ ] Memory browser uses Pi Web UI styling/components.
- [ ] Promotion from UI updates `.pi-memory/` files.

## M5 - Retrieval

Goal: memory injection becomes relevance-aware.

- [ ] Add recency retrieval from episode files.
- [ ] Add optional semantic retrieval using a provider-compatible embedding path.
- [ ] Blend semantic and recency scores.
- [ ] Show retrieval diagnostics in command/UI output.

Acceptance tests:
- [ ] Old relevant episodes can be injected.
- [ ] Irrelevant memory is omitted when budget is tight.
- [ ] Retrieval failures degrade to recency or no memory without blocking Pi.

## M6 - Package Hardening

Goal: the package is installable and safe for daily use.

- [ ] Add focused automated tests.
- [ ] Document install via `pi install` for local, git, and npm sources.
- [ ] Add path safety for writes and promotions.
- [ ] Add troubleshooting docs.

Acceptance tests:
- [ ] Fresh clone can run `npm install`, `npm run check`, and Pi extension smoke tests.
- [ ] Package install from a local path works.
- [ ] Unsafe writes outside `.pi-memory/` are rejected.
