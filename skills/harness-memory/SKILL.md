# Harness Memory

Use this skill when the user asks about the project memory harness, persistent coding memory, or how Pi should load project-specific context.

## Guidance

1. Prefer Pi extensions, skills, prompts, and web UI components over custom runtime infrastructure.
2. Keep memory files human-readable Markdown under `.pi-memory/` unless a later milestone introduces an index format.
3. Treat Pi sessions as the source of live conversation history and inject only compact, relevant memory into the system prompt.
4. Avoid reimplementing Pi tools, providers, sessions, or UI primitives unless the Pi API does not expose the needed hook.
