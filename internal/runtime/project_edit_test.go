package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/VrncQuentin/harness/internal/config"
	"github.com/VrncQuentin/harness/internal/memory"
	"github.com/VrncQuentin/harness/internal/project"
	"github.com/VrncQuentin/harness/internal/ui"
)

// TestProjectEdit_ActiveRepoNotMovable verifies that moving the active
// project's memory-repository boundary is refused while the installed
// generation still targets it, and that the refusal happens before any
// metadata or filesystem mutation. The runtime rejects the edit before
// project.Workflow.Update runs, so the project store records no write and the
// destination never touches disk.
func TestProjectEdit_ActiveRepoNotMovable(t *testing.T) {
	cfg := config.Defaults()
	seedRequiredConfigFiles(t, &cfg)
	cfg.Project.ActiveProjectSlug = project.GlobalSlug

	rt, projects := appliedRuntimeForTest(t, &cfg, nil)
	oldPath := projects.projects[project.GlobalSlug].MemoryRepoPath
	dst := filepath.Join(t.TempDir(), "moved-repo")

	_, err := rt.EditProject(context.Background(), ui.NewServer(0), NewEventChannel(), nil, project.UpdateInput{
		Slug:           project.GlobalSlug,
		DisplayName:    "Global",
		MemoryRepoPath: dst,
	}, project.MemoryRepoModeMove)
	if !errors.Is(err, ErrActiveProjectRepoMove) {
		t.Fatalf("EditProject error = %v, want ErrActiveProjectRepoMove", err)
	}

	// No metadata mutation: the store received no Update, and the project row
	// is unchanged.
	if projects.updateCalls != 0 {
		t.Fatalf("project store received %d Update calls, want 0 (refusal must precede metadata mutation)", projects.updateCalls)
	}
	if got := projects.projects[project.GlobalSlug].MemoryRepoPath; got != oldPath {
		t.Fatalf("project memory repo path = %q, want unchanged %q", got, oldPath)
	}

	// No filesystem mutation: the destination was never created or initialized.
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatalf("destination %q exists after a refused move: %v", dst, err)
	}

	// The installed generation is untouched and still serves the old repo.
	if _, err := rt.activeMem.Read("rules.md"); err != nil {
		t.Fatalf("installed generation reader failed after refused move: %v", err)
	}
}

// TestProjectEdit_ActiveRepoAliasIsNotAMove verifies that aliases do not
// manufacture a move: an edit that points the active project's memory repo at
// a symlink/junction alias of the same physical repository is not a boundary
// change, so it is allowed and performs no repository move.
func TestProjectEdit_ActiveRepoAliasIsNotAMove(t *testing.T) {
	cfg := config.Defaults()
	seedRequiredConfigFiles(t, &cfg)
	cfg.Project.ActiveProjectSlug = project.GlobalSlug

	rt, projects := appliedRuntimeForTest(t, &cfg, nil)
	oldPath := projects.projects[project.GlobalSlug].MemoryRepoPath

	base := t.TempDir()
	alias := filepath.Join(base, "alias")
	linkDir(t, oldPath, alias)

	same, err := memory.ProjectRepoManager{}.SameProjectRepoPath(alias, oldPath)
	if err != nil {
		t.Fatalf("SameProjectRepoPath: %v", err)
	}
	if !same {
		t.Fatal("alias must identify the same physical repository, or the test no longer discriminates")
	}

	// A move-mode edit at the alias is the same repository: it must be
	// allowed, not refused, and it must not touch the filesystem.
	if _, err := rt.EditProject(context.Background(), ui.NewServer(0), NewEventChannel(), nil, project.UpdateInput{
		Slug:           project.GlobalSlug,
		DisplayName:    "Global",
		MemoryRepoPath: alias,
	}, project.MemoryRepoModeMove); err != nil {
		t.Fatalf("alias of the active repo was treated as a move: %v", err)
	}
	if got := projects.projects[project.GlobalSlug].MemoryRepoPath; got != alias {
		t.Fatalf("project memory repo path = %q, want the alias %q", got, alias)
	}
	if _, err := os.Stat(filepath.Join(oldPath, "rules.md")); err != nil {
		t.Fatalf("original repository files gone after alias edit: %v", err)
	}
}

// TestProjectEdit_UpdateRoutesThroughTransaction verifies that an active
// project edit takes live effect through the Runtime-owned transaction rather
// than silently updating the store while the runtime keeps serving the old
// generation. After the edit the live llama manager and the recorded applied
// state must follow the new effective model — an edit that only mutated the
// store would leave the runtime on the old model.
func TestProjectEdit_UpdateRoutesThroughTransaction(t *testing.T) {
	cfg := config.Defaults()
	seedRequiredConfigFiles(t, &cfg)
	cfg.Project.ActiveProjectSlug = project.GlobalSlug

	rt, projects := appliedRuntimeForTest(t, &cfg, nil)
	if rt.applied == nil || rt.applied.runningModel.ModelPath != cfg.Model.ModelPath {
		t.Fatalf("initial applied running model = %+v, want model A", rt.applied)
	}
	root := projects.projects[project.GlobalSlug].MemoryRepoPath

	binaryB := "project-llama-b"
	modelB := "project-b.gguf"
	if _, err := rt.EditProject(context.Background(), ui.NewServer(0), NewEventChannel(), nil, project.UpdateInput{
		Slug:           project.GlobalSlug,
		DisplayName:    "Global",
		MemoryRepoPath: root,
		ModelBinary:    &binaryB,
		ModelPath:      &modelB,
	}, ""); err != nil {
		t.Fatalf("EditProject: %v", err)
	}

	if rt.applied == nil || rt.applied.runningModel.ModelPath != modelB {
		t.Fatalf("applied running model = %+v, want model B after the edit", rt.applied)
	}
	bin, args, _ := rt.llamaMgr.Args()
	if !strings.Contains(strings.Join(args, " "), modelB) {
		t.Fatalf("llama not reconfigured to model B after the edit: bin=%q args=%v (the edit silently updated the store instead of routing through the transaction)", bin, args)
	}
	if bin != binaryB {
		t.Fatalf("llama binary = %q, want %q", bin, binaryB)
	}
}

// TestProjectEdit_RetryComparesAgainstAppliedState verifies that a reload
// triggered by a later project edit compares the newly-read store contents
// against PR 9's recorded applied state — never against "old" values
// reconstructed from the already-mutated project store.
//
// The store is edited to prefer model B behind the runtime's back (the state
// the pre-PR-10 handler left after silently updating the store). A subsequent
// active-project edit triggers a reload that must notice B differs from the
// recorded applied model A and reconfigure llama. If the reload reconstructed
// the old model from the store it would read B, see no change, and leave llama
// on A.
func TestProjectEdit_RetryComparesAgainstAppliedState(t *testing.T) {
	cfg := config.Defaults()
	seedRequiredConfigFiles(t, &cfg)
	cfg.Project.ActiveProjectSlug = project.GlobalSlug

	rt, projects := appliedRuntimeForTest(t, &cfg, nil)
	if rt.applied == nil || rt.applied.runningModel.ModelPath != cfg.Model.ModelPath {
		t.Fatalf("initial applied running model = %+v, want model A", rt.applied)
	}
	root := projects.projects[project.GlobalSlug].MemoryRepoPath

	// The store already prefers model B while the runtime still records and
	// runs model A — exactly the divergence an edit that bypassed the runtime
	// would leave behind.
	binaryB := "project-llama-b"
	modelB := "project-b.gguf"
	projects.projects[project.GlobalSlug] = project.Project{
		Slug: project.GlobalSlug, DisplayName: "Global", MemoryRepoPath: root,
		ModelBinary: &binaryB, ModelPath: &modelB,
	}

	// A subsequent edit resubmits the store's current values (as the edit form
	// does) and triggers a reload through the same transaction boundary. The
	// reload must compare the store's B against the recorded applied A and
	// reconfigure llama.
	if _, err := rt.EditProject(context.Background(), ui.NewServer(0), NewEventChannel(), nil, project.UpdateInput{
		Slug:           project.GlobalSlug,
		DisplayName:    "Global Renamed",
		MemoryRepoPath: root,
		ModelBinary:    &binaryB,
		ModelPath:      &modelB,
	}, ""); err != nil {
		t.Fatalf("EditProject: %v", err)
	}

	if rt.applied == nil || rt.applied.runningModel.ModelPath != modelB {
		t.Fatalf("applied running model = %+v, want model B; the reload compared against store-reconstructed old values instead of the recorded applied state", rt.applied)
	}
	bin, args, _ := rt.llamaMgr.Args()
	if !strings.Contains(strings.Join(args, " "), modelB) {
		t.Fatalf("llama not reconfigured to model B: args=%v (the reload derived old from the already-mutated store)", args)
	}
	_ = bin
}

// TestProjectEdit_FailedReapplyRollsBack verifies that an active-project edit
// whose re-apply fails reports failure and restores the captured project row:
// the installed generation and recorded applied state stay live, and the store
// is rolled back to match. Without the rollback, the store would silently
// diverge from the live system — the exact 10.2 divergence this PR removes.
func TestProjectEdit_FailedReapplyRollsBack(t *testing.T) {
	cfg := config.Defaults()
	seedRequiredConfigFiles(t, &cfg)
	cfg.Project.ActiveProjectSlug = project.GlobalSlug

	rt, projects := appliedRuntimeForTest(t, &cfg, nil)
	root := projects.projects[project.GlobalSlug].MemoryRepoPath
	oldGen := rt.gen
	oldApplied := rt.applied
	oldCtx := rt.applied.runningModel.CtxSize

	// Corrupt the episode index manifest so candidate preparation fails during
	// the active edit's re-apply.
	manifestDir := filepath.Join(root, "index", "_episodes")
	if err := os.MkdirAll(manifestDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(manifestDir, "manifest.json"), []byte("{not json}"), 0o644); err != nil {
		t.Fatal(err)
	}

	newCtx := oldCtx + 1024
	_, err := rt.EditProject(context.Background(), ui.NewServer(0), NewEventChannel(), nil, project.UpdateInput{
		Slug:           project.GlobalSlug,
		DisplayName:    "Global Renamed",
		MemoryRepoPath: root,
		ModelCtxSize:   &newCtx,
	}, "")
	if err == nil {
		t.Fatal("active edit with a failed re-apply must report an error")
	}

	// The store rolled back to the captured project row.
	got := projects.projects[project.GlobalSlug]
	if got.DisplayName != "Global" {
		t.Fatalf("project display name = %q, want rollback to Global", got.DisplayName)
	}
	if got.ModelCtxSize != nil {
		t.Fatalf("project ctx override = %v, want rollback to nil", got.ModelCtxSize)
	}

	// The old generation and recorded applied state remain live.
	if rt.gen != oldGen {
		t.Fatal("failed re-apply replaced the installed generation")
	}
	if rt.applied != oldApplied {
		t.Fatal("failed re-apply replaced the recorded applied state")
	}
	if _, err := rt.activeMem.Read("rules.md"); err != nil {
		t.Fatalf("installed generation reader failed after rejected edit: %v", err)
	}
}

// TestProjectEdit_ActiveRepoIdentityCarriedThrough verifies that the
// active-repository identity decision is settled once and carried through the
// mutation: an alias repointed after the decision but before the workflow
// update cannot flip "same" into a move. Without the carried decision, the
// workflow would recompute the identity, observe "different", and execute an
// active move — the 10.1 violation — leaving the runtime and store divergent.
func TestProjectEdit_ActiveRepoIdentityCarriedThrough(t *testing.T) {
	cfg := config.Defaults()
	seedRequiredConfigFiles(t, &cfg)
	cfg.Project.ActiveProjectSlug = project.GlobalSlug

	rt, projects := appliedRuntimeForTest(t, &cfg, nil)
	oldPath := projects.projects[project.GlobalSlug].MemoryRepoPath

	base := t.TempDir()
	alias := filepath.Join(base, "alias")
	linkDir(t, oldPath, alias)

	// The repoint target does not exist yet; a move would create it.
	other := filepath.Join(base, "other-repo")

	rt.afterProjectIdentity = func() {
		if err := os.RemoveAll(alias); err != nil {
			t.Fatal(err)
		}
		linkDir(t, other, alias)
	}

	// The edit approves "same" at decision time (alias points at oldPath), and
	// the carried decision means the mutation runs no move even though the
	// alias now points at a different, not-yet-existing repository.
	if _, err := rt.EditProject(context.Background(), ui.NewServer(0), NewEventChannel(), nil, project.UpdateInput{
		Slug:           project.GlobalSlug,
		DisplayName:    "Global",
		MemoryRepoPath: alias,
	}, project.MemoryRepoModeMove); err != nil {
		t.Fatalf("EditProject: %v", err)
	}

	// No move executed: the repointed destination was never created.
	if _, err := os.Stat(other); !os.IsNotExist(err) {
		t.Fatalf("repointed destination %q was created by a move; the settled decision was not carried through", other)
	}
	// The original repository is untouched.
	if _, err := os.Stat(filepath.Join(oldPath, "rules.md")); err != nil {
		t.Fatalf("original repository files gone after the edit: %v", err)
	}
}

// TestProjectEdit_InactiveRepoMoveStillWorks verifies that an inactive
// project's repository can still be moved through the rooted MoveProjectRepo
// workflow, and that the active generation is untouched by the edit.
func TestProjectEdit_InactiveRepoMoveStillWorks(t *testing.T) {
	cfg := config.Defaults()
	seedRequiredConfigFiles(t, &cfg)
	cfg.Project.ActiveProjectSlug = project.GlobalSlug

	rt, projects := appliedRuntimeForTest(t, &cfg, nil)
	src := initRuntimeProjectRepo(t)
	// Give the source repo a distinguishing file so the copy can be verified.
	if err := os.WriteFile(filepath.Join(src, "known.txt"), []byte("moved"), 0o644); err != nil {
		t.Fatal(err)
	}
	projects.projects["demo"] = project.Project{
		Slug: "demo", DisplayName: "Demo", MemoryRepoPath: src,
	}
	dst := filepath.Join(t.TempDir(), "demo-moved")

	oldActiveMem := rt.activeMem
	if _, err := rt.EditProject(context.Background(), ui.NewServer(0), NewEventChannel(), nil, project.UpdateInput{
		Slug:           "demo",
		DisplayName:    "Demo",
		MemoryRepoPath: dst,
	}, project.MemoryRepoModeMove); err != nil {
		t.Fatalf("EditProject inactive move: %v", err)
	}

	if got := projects.projects["demo"].MemoryRepoPath; got != dst {
		t.Fatalf("demo memory repo path = %q, want %q", got, dst)
	}
	if b, err := os.ReadFile(filepath.Join(dst, "known.txt")); err != nil || string(b) != "moved" {
		t.Fatalf("moved repo contents = %q, %v; want %q", b, err, "moved")
	}
	// The active generation still targets the global repo.
	if rt.activeMem != oldActiveMem {
		t.Fatal("inactive edit replaced the active generation")
	}
	if _, err := rt.activeMem.Read("rules.md"); err != nil {
		t.Fatalf("active generation reader failed after inactive edit: %v", err)
	}
}
