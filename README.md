# harness

A local AI inference harness. One double-clickable binary that runs a local
model, keeps git-backed memory per project, and gives an agent an
approval-gated set of tools to work with. No cloud, no telemetry, and no
runtime dependencies beyond what the harness downloads and manages itself.

It starts silently, opens its management UI in your browser, and lives in the
system tray until you quit it. Everything — setup errors included — is surfaced
in the browser. You should never need a terminal to diagnose it.

> **Status: pre-release, under active development.** The inference core, prompt
> assembly, project memory, agent loop, and tool surface are in place. The
> pipeline DSL and the deeper memory layer are not. Interfaces still change
> without notice, and there are no tagged releases yet. See
> [docs/roadmap.md](docs/roadmap.md) for what is done and what is not.

## What it does

- **Runs a local model.** Spawns and supervises `llama-server`, restarts it when
  it dies, and streams health to the UI.
- **Remembers per project.** Each project is a plain git repo holding rules,
  facts, agent notes, and session episodes. Nothing is hidden in a proprietary
  store — it is markdown in git, readable and editable by hand.
- **Retrieves semantically.** A separate embedding sidecar indexes episodes; the
  prompt assembler blends semantic similarity with recency.
- **Runs an agent loop.** A first-party turn loop with tool calls, doom-loop
  detection, and a layered approval evaluator. Coding agents from other vendors
  are design references, not runtime dependencies.
- **Gates its tools.** Read-only tools (`read`, `ast_map`, `git_log`, …) are on
  by default. Anything that writes, executes, or touches a remote (`edit`,
  `exec`, `git_commit`, `git_push`, `gh_pr_create`, …) is off by default and
  passes through the approval layer when enabled.

## Requirements

- **Go 1.25.12+** to build.
- **Windows 10 1803+** or **Linux**. Both are first-class targets.
- **Linux only:** the systray needs CGO and GTK headers.
  ```sh
  sudo apt install libayatana-appindicator3-dev   # Debian/Ubuntu
  sudo dnf install libayatana-appindicator3-devel # Fedora
  sudo pacman -S libayatana-appindicator          # Arch
  ```
  Windows needs no external development libraries.

A GPU is optional. `bootstrap.ps1` detects CUDA or ROCm and falls back to CPU.

## Quick start (Windows)

```powershell
.\bootstrap.ps1
```

That detects your backend, pulls the matching `llama.cpp` release, and
downloads the Qwen3.6-35B-A3B and Nomic Embed Text v2 GGUF models into
`models/`. It is a large download — expect tens of gigabytes.

Then build and run:

```powershell
.\build.ps1
.\dist\harness.exe
```

The UI opens at `http://localhost:3000`. On first run nothing is configured, so
the status page shows a setup prompt pointing at the config editor. Fill in the
model and embedder paths, save, and the harness starts everything else.

To build without the bootstrap script:

```sh
go build ./cmd/harness
```

## How it runs

The UI server starts first and always succeeds. Every later step can fail
independently, and each failure becomes a line item on the status page with a
Retry button that re-runs the sequence without restarting the binary.

```
1. Acquire single-instance lock  → already held? exit silently
2. Start UI server (:3000)       → open browser
3. Open harness.db               → migrate
4. Load config                   → never saved? show first-run CTA and stop
5. Validate project memory repos
6. Start llama-server (:8081)
7. Start embedder sidecar (:8082)
8. Health check loops
9. Start OpenAI-compatible API (:8080, optional)
10. Hand off to the system tray
```

All state lives under `~/.harness/`:

```
~/.harness/
  harness.db              ← SQLite: config, metrics, projects
  projects/<slug>/        ← one git repo per project
    rules.md  user.md  facts.md
    agents/<name>/{persona.md, rules.md, notes.md}
    episodes/<agent>/<timestamp>.md
    index/_episodes/{vectors.bin, manifest.json}
    sessions.jsonl
```

`harness.db` is machine-local and never committed.

## Security posture

This is a local-first tool and its defaults assume a single-user machine.

- **Loopback only.** The UI and API servers bind `127.0.0.1`, as do the
  `llama-server` and embedder children. Neither server has an authentication
  layer, so the bind address is the security boundary. Do not put either behind
  a reverse proxy without adding authentication first.
- **Cross-origin requests are rejected.** Every state-changing route sits behind
  an origin check, so a hostile web page cannot drive the UI in your browser.
- **Destructive tools are opt-in.** They default to off and are scope-checked
  against project memory repos when enabled. `git_push`, `gh_pr_create`, and
  `gh_pr_merge` return proposals only — nothing reaches a remote until a human
  runs the command.
- **Secrets come from the environment.** `gh_pr_wait` reads `GITHUB_TOKEN` at
  call time. Tokens are never persisted to `harness.db` and never written to
  logs or tool output.

## Repository layout

```
cmd/harness/        main entry point, wires everything together
internal/
  agent/            agent registry (persona/rules/notes)
  agentloop/        native agent turn loop
  approvals/        layered permission evaluator
  config/           config schema, defaults, validation
  db/               SQLite persistence
  governor/         tool-output compression + token gates
  memory/           memory repo access and scaffolding
  memoryops/        embed-on-save, rebuild, dedup, scoring
  prompt/           layered prompt assembler
  retrieval/        blended semantic + recency scoring
  runtime/          mutable service graph and config re-apply
  tools/            tool registry + built-in sandboxed tools
  tray/             system tray, single-instance
  ui/               management web UI
assets/             embedded templates, CSS, htmx
migrations/         embedded SQL schema
docs/               architecture and roadmaps
```

The UI is server-rendered `net/http` + `html/template` + htmx + SSE. There is no
JavaScript framework, no build step, and no `node_modules`.

## Development

```sh
go build ./...
go test ./...
go vet ./...
```

CI runs lint, vet, cross-compilation for Linux and Windows, and the full test
suite on both `ubuntu-latest` and `windows-latest` for every pull request.

Read [docs/architecture.md](docs/architecture.md) before changing anything — it
defines component boundaries and package responsibilities that the codebase
holds to deliberately.

## Documentation

| Document | Contents |
| --- | --- |
| [docs/architecture.md](docs/architecture.md) | Component map, boundaries, design decisions |
| [docs/roadmap.md](docs/roadmap.md) | Milestones and acceptance tests |
| [docs/tool_roadmap.md](docs/tool_roadmap.md) | Tool surface specification |
| [docs/memory_roadmap.md](docs/memory_roadmap.md) | Memory layer plan |
| [docs/DSL.md](docs/DSL.md) | Pipeline DSL specification (planned) |
| [docs/dsl_roadmap.md](docs/dsl_roadmap.md) | Pipeline DSL milestones |

## License

Copyright (C) 2026 VrncQuentin.

Licensed under the **GNU Affero General Public License v3.0** — see
[LICENSE](LICENSE) for the full text.

In short: you may use, study, modify, and redistribute the harness freely. If
you distribute a modified version, or offer one to users over a network, you
must release your source under the same license. That network clause is the
reason for AGPL over plain GPL — the harness serves a web UI and runs models, so
hosting it as a service is a realistic path that GPL alone would not reach.

If the AGPL does not work for your situation, **a separate commercial license
can be granted** — open an issue to start that conversation. Copyright is held
in full by one author, which is what makes that possible, and it is why
contributions are accepted under a CLA. See [CONTRIBUTING.md](CONTRIBUTING.md).
