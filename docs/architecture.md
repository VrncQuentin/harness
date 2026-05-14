# Architecture

## Overview

This project now targets Pi Coding instead of a standalone Go harness. The product is a Pi package made of extensions, skills, prompts, and optional web UI modules that add persistent project memory and workflow affordances to Pi.

Pi owns the agent loop, provider integration, tool execution, session storage, terminal UI, and web UI component library. This repository owns only the behavior that should be layered on top of Pi.

The archived Go-native implementation is preserved under `archived/go-harness/` for reference.

## Active Components

```text
Pi Coding
  packages/coding-agent       extension host, sessions, tools, commands
  packages/agent              agent loop and tool execution
  packages/ai                 provider/model integrations
  packages/web-ui             reusable browser chat components

pi-memory-harness
  src/extension.ts            Pi extension entrypoint
  skills/                     Agent Skills loaded by Pi
  prompts/                    prompt templates loaded by Pi
  .pi-memory/                 project-local memory files, user managed
```

## Extension Boundary

The extension uses Pi lifecycle hooks instead of running its own daemon:

- `before_agent_start` injects layered memory into the system prompt.
- Extension commands expose maintenance actions such as `/memory-status`.
- Later milestones can use `agent_end`, `session_shutdown`, and compaction hooks to summarize sessions and persist durable memory.
- Later milestones can add tools for explicit promotion, search, and project memory management.

## Memory Layout

Memory starts as Markdown so it can be edited, reviewed, and versioned by the user:

```text
.pi-memory/
  global/
    rules.md
    user.md
    facts.md
  agents/
    coder/
      notes.md
  projects/
    default/
      rules.md
```

The extension currently reads these layers if present and appends them to Pi's system prompt. Missing optional files are ignored.

## Web UI Direction

Any browser UI should use `@earendil-works/pi-web-ui` rather than the archived server-rendered Go UI. The likely shape is a small TypeScript app that composes `ChatPanel`, Pi `Agent`, storage stores, custom message renderers, and memory-management panels.

## Non-Goals

- No custom Go process manager.
- No standalone tray app.
- No custom OpenAI-compatible API proxy unless Pi cannot use the target provider directly.
- No custom tool execution layer.
- No duplicate chat UI primitives outside Pi Web UI.
