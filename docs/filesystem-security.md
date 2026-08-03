# Filesystem Security Model

> **Current reference.** This document describes what `main` implements now.
> It is the single canonical home for the filesystem threat model and for the
> `internal/pathid` / `internal/rootfs` primitives. The [tool document](tools.md)
> links here for primitive behavior instead of reproducing it, and
> [architecture.md](architecture.md) keeps only the cross-cutting invariants.

## Overview: two primitives, one invariant

The harness's filesystem security is built on two primitives:

- **`internal/pathid`** — physical path identity. It answers where a path
  physically is (junctions, symlinks, case, and 8.3 aliases resolved), and
  provides containment and identity checks.
- **`internal/rootfs`** — rooted directory operations. It acts on the physical
  place pathid resolved, through a pinned `os.Root` handle, never through the
  caller's pathname.

Together they implement **pin-before-authorize**: resolve the configured
directory once, bind the open handle to its physical identity, and perform all
subsequent operations through that handle. Neither primitive substitutes for
the other: validating a pathname and then reopening it checks one resolution
and acts on another, and canonicalizing the opened target against a pinned
string is no better — the comparison can agree with itself while admitting a
replaced directory.

Every configured-tree operation in the repository operates through
`internal/rootfs` pinned handles; the fsaudit allowlist contains no migration
entries, only permanent boundary exceptions.

## Physical identity (`internal/pathid`)

`pathid` answers one question — where a path physically is — for every
component that enforces a boundary with it: the tool sandbox, the C2
memory-repo lock, and the git write lock. They must not decide differently, so
they all read from this one package.

### Public surface

- **`Canonical`** resolves an existing path to its physical location. On
  Windows this is `GetFinalPathNameByHandle`, which resolves junctions, mount
  points, symlinks, and 8.3 short names; elsewhere `filepath.EvalSymlinks` is
  complete because symlinks are the only reparse mechanism.
- **`Resolve`** canonicalizes the deepest existing component and re-appends the
  components below it, so a path that does not exist yet is judged by where it
  would land rather than by its parent. A relative input is made absolute
  first, so the result is absolute on every OS.
- **`ID`** is the opaque result of `Resolve`. Comparison lives on the `ID` —
  `Equal`, `Contains`, `Key` — so there is no exported operation with an
  "already resolved" precondition for a caller to forget.
- **`Same`** and **`SameOrWithin`** are the high-level operations: repository
  identity and sandbox/C2 containment. The repository-wide mutation coordinator
  is keyed by an identity's `Key`, never by a pathname or a hand-composed
  resolution.
- **`ID.Contains`** is `filepath.Rel`-based, not prefix-based. A prefix test
  rejects everything below a filesystem or volume root (`C:\`, `/` already end
  in a separator), accepts a sibling sharing a textual prefix, and has no
  answer for two different volumes.
- **Key maps and locks use `ID.Key`, never the `ID` itself.** Go compares every
  field including the display path, and `Resolve` re-appends a not-yet-created
  tail in the caller's case, so one identity can produce two structs.

### Design constraints

- **`filepath.EvalSymlinks` must not be used for containment or identity.** It
  leaves a Windows junction unresolved and fails outright on paths below one,
  so a check built on it accepts a path that physically reads outside its root
  and gives a junction alias a different identity from its target.
- **Every function fails closed.** Only `fs.ErrNotExist` walks upward in
  `Resolve`; any other resolution failure is returned. A caller cannot
  distinguish an unresolvable path from a safe one, so the unknown case must be
  a refusal rather than a lexical guess.

## Rooted operations (`internal/rootfs`)

`os.Root` removes the pathname from the decision by holding the directory open
and resolving every component against that handle. An `os.Root`-based boundary:

- **follows** a symlink whose target stays inside the root;
- **refuses** an absolute link target unconditionally — which is what a Windows
  junction always stores, so a junction is never traversed through a root;
- does **not** stop traversal across bind mounts, ordinary mount points, or
  into `/proc` on Linux — mount-based escapes are outside the threat model
  (staging one needs privileges that already defeat the sandbox, and closing
  them would need `openat2` with `RESOLVE_NO_XDEV`, which has no Windows
  counterpart);
- does **not** sandbox subprocesses — command containment is a separate
  problem.

### Responsibilities and invariants

- **`Root`** wraps an open directory for relative access. It never returns an
  `*os.File`: reads come back through an opaque `ReadCloser` and appends
  through an opaque `AppendFile`, neither of which reveals a pathname.
- **`OpenIdentified`** pins a directory and returns it *with* the physical
  identity that directory has been confirmed to have. The pairing is the point:
  an identity resolved separately from a pin describes a name, so any later
  reasoning about the handle is reasoning about something that need not be what
  is held open. `internal/git` opens its repository boundary through
  `OpenIdentified` and retains it (`NewAnchorFromRoot`); `memory.DirReader`
  opens and identifies in the same step; runtime construction compares the
  retained boundaries with `os.SameFile` before publishing a candidate, so git
  commits, session writes, and index publication are bound to the same physical
  repository or the candidate fails closed.
- **`Anchor`** retains an open kernel handle so identity comparison with
  `os.SameFile` stays durable across reopen. `Open()` re-opens the stored
  pathname and refuses when the newly opened handle is not the same object, so
  a same-name replacement fails closed. `SameAnchor`/`SameRoot` compare open
  directory objects via `os.SameFile`.
- **`Set`** is the sandbox-root list. `Set.Open` pins each candidate root with
  `OpenIdentified` and resolves the caller's target alongside that pin, so the
  containment decision and the retained handle describe the same boundary; it
  returns a `Target` for the root that physically owns the path. A path that
  reaches a root through a link/junction/8.3 alias/different case is
  recognized; a path that leaves a root through one is refused.
- **`Target`** carries the caller's display spelling — locators and tool output
  stay in the terms the caller asked in — while reads and writes go through the
  handle.
- **`WriteAtomic`** (temp file + rename) and **`CreateExclusive`** (`O_EXCL`)
  are different operations, not variants. A rename replaces whatever holds the
  name, which is right for editing an existing file and destructive for
  creating a new one, so whole-file `edit` uses the latter with no preceding
  existence check to race against. A failed `CreateExclusive` leaves its
  partial file: cleaning up means removing a *name*, which by then may belong
  to someone else's file.
- **`AppendSync` and `AppendFile`** open with `O_WRONLY|O_CREATE|O_APPEND` and
  nothing else — no `O_TRUNC`, no `O_RDWR`, no seek, no caller-supplied flags —
  so no spelling in the package can shorten an append-only log.
- **`Root.SameDir`** compares two open directories as filesystem objects. It
  settles the directories only; it says nothing about the files inside them,
  nor about one being inside the other.
- **`Root.OpenChild`** pins a subdirectory as a `Root` of its own, so a
  traversal that inspects a directory and then descends into it uses one handle
  rather than resolving the same name twice.
- **`Root.OpenChildNoFollow`** opens the child, Lstats the entry through the
  parent, rejects links, and compares the entry with the opened handle via
  `os.SameFile` — what is opened and what is checked are the same object.
- **`Root.WriteStreamAtomic`** publishes by rename. Replacing a directory
  *entry* leaves the inode that held the name alone, which is the only way to
  write into a tree whose entries may be hard links to files elsewhere —
  truncating in place writes *through* the link.
- **`Root.AppendSync` is the deliberate in-place exception to rename
  publication**, used for append-only logs like `sessions.jsonl`. Appending
  necessarily writes in place, so it writes *through* a hard link: a
  `sessions.jsonl` entry hard-linked to a file outside the repo gains the
  record on both names. Rooted access prevents pathname, symlink, and junction
  escapes but cannot distinguish a hard-linked entry from the same underlying
  file elsewhere. This is inherent to append and is documented rather than
  solved; the log is not rewritten with a read-modify-rename because doing so
  would replace the append-only identity the log exists for.
- **`Root.ReadDir` sorts by filename.** `os.Root` has no `ReadDir`, and
  `File.ReadDir` returns filesystem order; tool output has to be stable across
  identical calls.
- **`Root.RemoveVerified`** Lstats the entry and compares it with the
  `os.FileInfo` the caller actually observed via `os.SameFile` before removing —
  a name that no longer identifies what was observed is refused.

### The repo copy layers its checks

The project-repo copy does not rely on any single check: the two trees must be
disjoint by name, disjoint again against handle-bound identities once both ends
are pinned, distinct as directories, and disjoint level by level during the
walk — every newly pinned source directory against every pinned destination
directory and vice versa, which is what catches a directory being moved from
one tree into the other mid-copy. Files need no comparison, because they are
published by rename.

## Repository identity coordination

The repository-wide mutation coordinator (`internal/coord`) is keyed by
`pathid.ID.Key()`, so two spellings of one physical repository (a junction
alias, an 8.3 name) resolve to the same gate and serialize on one object. Git
mutations, index publication, and project-repo scaffolding and moves share it;
an alias spelling cannot split one repository across two coordinators. Lock
order is fixed as repository gate, then the per-handle mutex, in both the
standalone and in-transaction paths.

Repository identity is proven handle-bound, not pathname-bound:

- `internal/git` pins its repository boundary at open, retains it, and compares
  it with other components' opened boundaries via `os.SameFile`
  (`SameAnchor`), so a directory replaced at the same pathname between two
  opens is detected rather than accepted on the strength of a shared canonical
  path.
- `project.Workflow` settles a repo move as a handle-bound proof
  (`PinRepoIdentity` → `OpenIdentified`), keeps that proof private, and
  re-verifies it at the moment of mutation (`ApplyUpdate`): `SameAs` re-opens
  the current path and compares retained handles with `Root.SameDir`
  (`os.SameFile`), so a repointed alias or a same-name physical replacement
  fails closed even when it reuses the pathid key.
- Runtime construction compares the retained git boundary and the generation's
  memory readers before publishing a candidate, so all writes in a generation
  bind to the same physical repository.

## Defended threats

| Threat | Mechanism | Status |
| --- | --- | --- |
| Symlink escape from a sandbox root | `os.Root` resolve-by-handle; absolute-target links refused unconditionally | implemented |
| Windows junction escape | `pathid` resolves junctions before the root sees the name; `Set.Open` addresses the target through the physical path | implemented |
| Case / 8.3 alias (Windows) | `pathid.Canonical` resolves to a single physical name; containment checked against the canonical form | implemented |
| Same-name directory replacement | `OpenIdentified` verifies the pinned handle against the physical identity with `os.SameFile`; a replacement fails the comparison | implemented |
| Rename of original directory | Operations through the pinned handle continue to address the original directory; `OpenIdentified` fails closed on the renamed name | implemented |
| Hard-link leaf writes | `WriteStreamAtomic` publishes by rename — a rename replaces the directory entry and leaves the linked inode alone. The one deliberate exception is `AppendSync` for append-only logs, where writing through a hard link is inherent to append and documented, not solved by rewriting the log | implemented |
| In-process concurrent writers | One repository-wide mutation coordinator per physical repository identity, shared by git mutations, index publication, and project-repo scaffolding and moves; publication + commit run inside one repository transaction held across both | implemented |
| Check/use races on intermediate directories | `OpenChildNoFollow` opens the child, Lstats the entry through the parent, rejects links, and compares the entry with the opened handle via `os.SameFile` | implemented |
| Memory repo reads/writes through pathname | Read and write operations use pinned `os.Root` handles; index uses vector-first copy-on-write publication; `DirReader` identity is bound at construction and compared against the git repository's retained identity at runtime construction; the episode index is rooted with a verified identity; scaffolding, validation, destination creation, and file enumeration route through pinned roots and bind to the retained git boundary with `os.SameFile` inside the repository transaction; the session log is read and appended through the same generation-owned pinned reader, with only `fs.ErrNotExist` meaning "no sessions" and a rooted append primitive that cannot truncate | implemented |

## Out of scope

- **Privileged mount manipulation.** Staging a bind mount, mount point, or
  `/proc` entry inside a tree requires privileges that already defeat the
  sandbox. `openat2` with `RESOLVE_NO_XDEV` would close this but has no Windows
  counterpart.
- **Subprocess sandboxing.** `exec`, `go_test`, and `go_lint` validate their
  working directory with `pathid` and hand a pathname to the child process.
  Command containment is a separate problem; see [tools.md](tools.md).
- **go-git pathname boundary.** go-git resolves its storage by pathname, not by
  handle. The wrapper pins and verifies the repository boundary at open,
  retains that verified identity for coordinator selection, and refuses to open
  through a repointed spelling. A spelling repointed *after* the open is a
  documented go-git limitation the wrapper does not claim to close, and go-git
  itself will not open through a directory link.

## Acknowledged residual windows

- **Within `WriteStreamAtomic`**, the temporary file is created inside a
  destination parent directory pinned with `OpenChild`. After writing, the data
  is fsynced and the temp file's identity is captured from the live handle with
  `f.Stat()` and compared against the named entry through the pinned parent via
  `os.SameFile`. A substituted entry is refused. The remaining window is
  between that identity comparison and the rename: an external process can
  substitute the entry in that interval. Closing it requires a
  compare-and-rename primitive that operates on a handle rather than a name; no
  such primitive exists in the portable Go standard library. Documenting the
  window rather than claiming it closed is deliberate.
- **`RemoveVerified`** re-reads the entry immediately before removal; a swap
  between the re-read and the remove is a narrow, inherent residual window of
  remove-by-name.

## Audit enforcement (`cmd/fsaudit`)

Production calls to the watched filesystem symbols — approximately 35 across
`os` and `path/filepath`, including `MkdirTemp`, `Lchown`, `Chdir`, `CopyFS`,
and `DirFS` — are inventoried in `cmd/fsaudit/allowlist.json`. The
configured-tree migration is complete, so every inventoried call is a
**permanent** boundary exception with a justification. There is no migration
category: the JSON schema has no field for one, the decoder rejects unknown
fields, and validation requires a justification on every entry, so a migration
entry cannot be repopulated.

The tool verifies on every CI run that no new direct filesystem call appears
without a matching entry. It scans all production `.go` files including
`internal/rootfs` and `internal/pathid`; only the audit tool itself is exempt.
The watched-function policy is compiled into the scanner — it is not
configurable from the allowlist.

The scanner also blocks capability escapes that cannot be inventoried: dot
imports of watched packages, extracting watched functions as values, `os.Root`
type references outside `internal/rootfs`, and `os.OpenRoot` calls outside
`internal/rootfs` — creating a root is the core primitive of the boundary and
must be centralized there. Within `rootfs`, every `os.Root` reference is
blocked except the single private `Root.root` backing field.

## Why subprocess and go-git paths cannot receive rooted handles

`exec`, `go_test`, and `go_lint` hand a working directory pathname to a child
process; `go-git` resolves its storage by pathname. Neither can be given an
`os.Root` handle, so the harness does not pretend to sandbox them. Instead:

- subprocess working directories are validated with `pathid` (`Resolve` +
  `ID.Contains`) before the pathname is handed over;
- go-git repositories keep the explicit identity and C2 checks around the
  boundary: the wrapper pins and verifies the repository at open, retains that
  identity for coordinator selection, and the memory-repo scope predicate
  rejects any git-write target that resolves inside a project memory repo.

These are documented boundaries, not holes in `rootfs`: rooted handles protect
the operations that *can* be rooted; operations that structurally take a
pathname get the strongest identity check available and are treated as separate
threat surfaces.
