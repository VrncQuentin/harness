# Contributing

Thanks for your interest in the harness.

## Licensing, and why there is a CLA

The harness is licensed under the **GNU AGPL-3.0** (see [LICENSE](LICENSE)).
Anything you build on it and distribute — or offer to users over a network —
has to be released under the same terms.

Copyright in the project is currently held in full by a single author. That is
deliberate: it keeps open the option of granting separate commercial licenses to
people who cannot comply with the AGPL. Merging a contribution without an
explicit grant would end that option permanently, because relicensing would then
require chasing down every past contributor for permission.

So contributions are accepted under a lightweight **Contributor License
Agreement**. By opening a pull request you confirm:

1. **You wrote it, or you have the right to submit it.** The contribution is
   your original work, or you have permission to contribute it and it does not
   knowingly infringe anyone's rights. If your employer has rights to work you
   produce, you have their permission to contribute it.
2. **You grant a relicensing right.** You grant the project's copyright holder a
   perpetual, worldwide, non-exclusive, royalty-free, irrevocable license to
   reproduce, modify, distribute, sublicense and otherwise use your
   contribution, both under the AGPL-3.0 and under other license terms,
   including proprietary ones.
3. **You keep your copyright.** This is a license grant, not an assignment. Your
   contribution stays yours, and it remains available to everyone under the
   AGPL.
4. **It is provided as-is.** You offer no warranty on the contribution.

State in your pull request description that you have read and agree to this
section. If you would rather not grant point 2, open an issue and describe the
change instead — a maintainer can implement it independently.

> This CLA is written to be readable rather than exhaustive, and it has not been
> reviewed by a lawyer. If a contribution ever becomes commercially significant,
> it is worth having a professional look at it.

## Working on a change

The full working rules — repo structure, architectural invariants, Go style,
milestone discipline — live in **[CLAUDE.md](CLAUDE.md)**. Read that first; it
is the single source of truth and applies to human and agent contributors alike.

The short version:

- Branch from `main`. Name it `feat/<description>` or `fix/<description>`. Never
  commit directly to `main`.
- Read [docs/architecture.md](docs/architecture.md) before writing code. It
  defines component boundaries that the codebase holds to deliberately.
- Work one milestone at a time — check [docs/roadmap.md](docs/roadmap.md).
- Commit in small logical steps rather than one batch at the end.
- Every package gets a `_test.go`. Tests are table-driven.

Before opening a pull request:

```sh
go build ./...
go test ./...
go vet ./...
gofmt -l .
```

CI runs lint, vet, cross-compilation for Linux and Windows, and the full test
suite on both `ubuntu-latest` and `windows-latest`. All of it must pass.

## Things that need discussion first

Open an issue before writing code if your change would:

- move a component boundary described in `docs/architecture.md`
- bind a listener to anything other than `127.0.0.1` — this is a security
  boundary, not a preference, and `TestStart_BindsLoopbackOnly` enforces it
- add a runtime dependency, or any dependency the standard library could cover
- introduce a JavaScript framework or a build step to the UI
- add an `init()` function

## Reporting a security issue

Please do not open a public issue for a vulnerability. Report it privately
through GitHub's security advisory tab so it can be fixed before disclosure.
