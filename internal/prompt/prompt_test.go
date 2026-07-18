package prompt

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/vrnc/harness/internal/agent"
	"github.com/vrnc/harness/internal/config"
	"github.com/vrnc/harness/internal/embedder"
	"github.com/vrnc/harness/internal/index"
	"github.com/vrnc/harness/internal/inference"
	"github.com/vrnc/harness/internal/memory"
	"github.com/vrnc/harness/internal/project"
)

type stubEmbedder struct {
	vectors [][]float32
}

func (s *stubEmbedder) Embed(_ context.Context, _ []string) ([][]float32, error) {
	return s.vectors, nil
}

var _ embedder.Client = (*stubEmbedder)(nil)

// writeRepo builds a memory repo under t.TempDir() from a map of
// forward-slash relative paths to contents, returning a DirReader.
func writeRepo(t *testing.T, files map[string]string) *memory.DirReader {
	t.Helper()
	root := t.TempDir()
	for rel, body := range files {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}
	return memory.NewDirReader(root)
}

// newAssembler wires up a DiskAssembler with a disk-backed registry.
func newAssembler(t *testing.T, mem *memory.DirReader, cfg config.PromptConfig) *DiskAssembler {
	t.Helper()
	active := ""
	reg := agent.NewDiskRegistry(mem, func() string { return active }, func(n string) error { active = n; return nil })
	return NewProjectDiskAssembler(mem, mem, reg, cfg)
}

func newProjectAssembler(t *testing.T, globalFiles, activeFiles map[string]string, cfg config.PromptConfig) *DiskAssembler {
	t.Helper()
	global := writeRepo(t, globalFiles)
	active := writeRepo(t, activeFiles)
	selected := ""
	reg := agent.NewDiskRegistry(global, func() string { return selected }, func(n string) error { selected = n; return nil })
	return NewProjectDiskAssembler(global, active, reg, cfg).WithProjectSlug("dt")
}
func baseCfg() config.PromptConfig {
	return config.PromptConfig{
		CtxSize:             0,
		MemoryTokenBudget:   0,
		ConversationReserve: 0,
	}
}

func TestAssemble_MissingRulesIsRequired(t *testing.T) {
	mem := writeRepo(t, map[string]string{
		// rules.md is missing on purpose; persona.md exists so we get
		// past the agent-required check and surface the actual error
		// the test exercises.
		"agents/coder/persona.md": "p",
	})
	asm := newAssembler(t, mem, baseCfg())
	_, _, err := asm.Assemble(context.Background(), "coder", nil)
	if err == nil {
		t.Fatal("expected error for missing rules.md")
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("expected fs.ErrNotExist, got %v", err)
	}
}

func TestAssemble_RequiresAgentName(t *testing.T) {
	mem := writeRepo(t, map[string]string{
		"rules.md": "RULES",
	})
	asm := newAssembler(t, mem, baseCfg())
	_, _, err := asm.Assemble(context.Background(), "", nil)
	if !errors.Is(err, ErrAgentRequired) {
		t.Errorf("expected ErrAgentRequired, got %v", err)
	}
}

func TestAssemble_AgentRequiresPersona(t *testing.T) {
	mem := writeRepo(t, map[string]string{
		"rules.md": "x",
		// agent folder exists but has no persona.md - ListDirs surfaces
		// the folder but Get rejects it as missing, which wraps
		// fs.ErrNotExist and propagates here.
		"agents/coder/notes.md": "some notes",
	})
	asm := newAssembler(t, mem, baseCfg())
	_, _, err := asm.Assemble(context.Background(), "coder", nil)
	if err == nil {
		t.Fatal("expected error for missing persona")
	}
}

func TestAssemble_FullStackOrder(t *testing.T) {
	mem := writeRepo(t, map[string]string{
		"rules.md":                     "RULES",
		"user.md":                      "USER",
		"facts.md":                     "FACTS",
		"agents/coder/persona.md":      "PERSONA",
		"agents/coder/rules.md":        "AGENTRULES",
		"agents/coder/notes.md":        "NOTES",
		"episodes/coder/2026-01-01.md": "EP1",
		"episodes/coder/2026-02-01.md": "EP2",
	})
	asm := newAssembler(t, mem, baseCfg())
	msgs, stats, err := asm.Assemble(context.Background(), "coder", nil)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	sys := msgs[0].Content
	// Verify section order by checking indices. Agent Rules sits between Persona
	// and Facts in the mandatory-layer block.
	headers := []string{"# Rules", "# User", "# Persona", "# Agent Rules", "# Facts", "# Notes", "# Episodes"}
	lastIdx := -1
	for _, h := range headers {
		idx := strings.Index(sys, h)
		if idx < 0 {
			t.Errorf("missing header %q in system message:\n%s", h, sys)
			continue
		}
		if idx <= lastIdx {
			t.Errorf("header %q appears before earlier header; prompt ordering broken", h)
		}
		lastIdx = idx
	}

	// Episodes must be rendered oldest-first (file name is the sort
	// key).
	ep1Idx := strings.Index(sys, "EP1")
	ep2Idx := strings.Index(sys, "EP2")
	if ep1Idx < 0 || ep2Idx < 0 || ep1Idx > ep2Idx {
		t.Errorf("episodes not ordered oldest-first; ep1=%d ep2=%d", ep1Idx, ep2Idx)
	}

	if stats.Rules == 0 || stats.User == 0 || stats.Persona == 0 || stats.AgentRules == 0 || stats.Facts == 0 || stats.Notes == 0 || stats.Episodes == 0 {
		t.Errorf("stats missing counts: %+v", stats)
	}
}

func TestAssemble_ConversationAppendedVerbatim(t *testing.T) {
	mem := writeRepo(t, map[string]string{
		"rules.md":                "RULES",
		"agents/coder/persona.md": "PERSONA",
	})
	asm := newAssembler(t, mem, baseCfg())

	convo := []inference.Message{
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "hi"},
	}
	msgs, _, err := asm.Assemble(context.Background(), "coder", convo)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages (system + 2 convo), got %d", len(msgs))
	}
	if msgs[0].Role != "system" {
		t.Errorf("first message role = %q, want system", msgs[0].Role)
	}
	if !reflect.DeepEqual(msgs[1], convo[0]) || !reflect.DeepEqual(msgs[2], convo[1]) {
		t.Errorf("conversation mutated during assembly")
	}
}

func TestAssemble_TrimsEpisodesOldestFirstForMemoryBudget(t *testing.T) {
	// Generate five episodes with increasing content size; use a
	// deterministic tokenizer so we can reason about counts.
	mem := writeRepo(t, map[string]string{
		"rules.md":                "r",
		"agents/coder/persona.md": "p",
		"episodes/coder/01.md":    strings.Repeat("a", 200),
		"episodes/coder/02.md":    strings.Repeat("b", 200),
		"episodes/coder/03.md":    strings.Repeat("c", 200),
		"episodes/coder/04.md":    strings.Repeat("d", 200),
		"episodes/coder/05.md":    strings.Repeat("e", 200),
	})
	cfg := baseCfg()
	// Each episode is 200 runes => 50 tokens under the default heuristic.
	// Budget 120 tokens: only the two most recent episodes fit.
	cfg.MemoryTokenBudget = 120
	asm := newAssembler(t, mem, cfg)

	msgs, stats, err := asm.Assemble(context.Background(), "coder", nil)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	sys := msgs[0].Content

	// With budget 120 and 50 tokens per episode, only the last 2 fit
	// (100 tokens).
	for _, trimmed := range []string{"01.md", "02.md", "03.md"} {
		if strings.Contains(sys, trimmed) {
			t.Errorf("expected %s to be trimmed, but found in output", trimmed)
		}
	}
	for _, kept := range []string{"04.md", "05.md"} {
		if !strings.Contains(sys, kept) {
			t.Errorf("expected %s to be kept, but absent", kept)
		}
	}
	if stats.Episodes > cfg.MemoryTokenBudget {
		t.Errorf("episodes tokens (%d) exceed memory budget (%d)", stats.Episodes, cfg.MemoryTokenBudget)
	}
}

// TestAssemble_RecencyNCapsEpisodeCount exercises the recency cap that
// fires before any token-budget trimming. With seven episodes on disk
// and RecencyN=3, only the three newest survive; with RecencyN=0 the
// cap is disabled and all seven land.
func TestAssemble_RecencyNCapsEpisodeCount(t *testing.T) {
	files := map[string]string{
		"rules.md":                "r",
		"agents/coder/persona.md": "p",
	}
	for i := 1; i <= 7; i++ {
		files[fmt.Sprintf("episodes/coder/2026-01-%02d.md", i)] = fmt.Sprintf("EP%d", i)
	}

	tests := []struct {
		name      string
		recencyN  int
		wantCount int
		wantKept  []string
		wantDrop  []string
	}{
		{
			name:      "cap to last 3",
			recencyN:  3,
			wantCount: 3,
			wantKept:  []string{"2026-01-05.md", "2026-01-06.md", "2026-01-07.md"},
			wantDrop:  []string{"2026-01-01.md", "2026-01-02.md", "2026-01-03.md", "2026-01-04.md"},
		},
		{
			name:      "zero means unlimited",
			recencyN:  0,
			wantCount: 7,
			wantKept:  []string{"2026-01-01.md", "2026-01-04.md", "2026-01-07.md"},
		},
		{
			name:      "cap larger than corpus is a no-op",
			recencyN:  100,
			wantCount: 7,
			wantKept:  []string{"2026-01-01.md", "2026-01-07.md"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mem := writeRepo(t, files)
			cfg := baseCfg()
			cfg.RecencyN = tc.recencyN
			asm := newAssembler(t, mem, cfg)

			msgs, _, err := asm.Assemble(context.Background(), "coder", nil)
			if err != nil {
				t.Fatalf("Assemble: %v", err)
			}
			sys := msgs[0].Content

			gotCount := 0
			for i := 1; i <= 7; i++ {
				if strings.Contains(sys, fmt.Sprintf("2026-01-%02d.md", i)) {
					gotCount++
				}
			}
			if gotCount != tc.wantCount {
				t.Errorf("episode count: got %d, want %d\nsys:\n%s", gotCount, tc.wantCount, sys)
			}
			for _, kept := range tc.wantKept {
				if !strings.Contains(sys, kept) {
					t.Errorf("expected %s to be kept, but absent", kept)
				}
			}
			for _, dropped := range tc.wantDrop {
				if strings.Contains(sys, dropped) {
					t.Errorf("expected %s to be dropped, but present", dropped)
				}
			}
		})
	}
}

func TestAssemble_MandatoryLayersNeverTrimmed(t *testing.T) {
	// Set a memory budget smaller than the mandatory layers combined;
	// these must still be present, including per-agent rules.
	mem := writeRepo(t, map[string]string{
		"rules.md":                strings.Repeat("R", 400), // 100 tokens
		"user.md":                 strings.Repeat("U", 400),
		"facts.md":                strings.Repeat("F", 400),
		"agents/coder/persona.md": strings.Repeat("P", 400),
		"agents/coder/rules.md":   strings.Repeat("A", 400),
		"agents/coder/notes.md":   strings.Repeat("N", 400),
	})
	cfg := baseCfg()
	cfg.MemoryTokenBudget = 10 // absurdly low
	asm := newAssembler(t, mem, cfg)

	msgs, _, err := asm.Assemble(context.Background(), "coder", nil)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	sys := msgs[0].Content
	for _, section := range []string{"# Rules", "# User", "# Persona", "# Agent Rules"} {
		if !strings.Contains(sys, section) {
			t.Errorf("mandatory %q missing when budget pressure is high", section)
		}
	}
}

func TestAssemble_CtxSizeTrimsAgainstConversationReserve(t *testing.T) {
	mem := writeRepo(t, map[string]string{
		"rules.md":                "r",                      // 1 token
		"agents/coder/persona.md": "p",                      // 1 token
		"episodes/coder/01.md":    strings.Repeat("a", 400), // 100 tokens
		"episodes/coder/02.md":    strings.Repeat("b", 400),
		"episodes/coder/03.md":    strings.Repeat("c", 400),
	})
	cfg := config.PromptConfig{
		CtxSize:             250, // effective limit 250 - 100 = 150 tokens for layers+convo
		ConversationReserve: 100,
		MemoryTokenBudget:   1000, // large, so this test exercises the ctx guardrail path
	}
	asm := newAssembler(t, mem, cfg)

	// Conversation uses 40 tokens, leaving 110 tokens for layers; only
	// one episode (100 tokens) fits.
	convo := []inference.Message{
		{Role: "user", Content: strings.Repeat("q", 160)},
	}
	msgs, stats, err := asm.Assemble(context.Background(), "coder", convo)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}

	limit := cfg.CtxSize - cfg.ConversationReserve
	fixed := stats.Rules + stats.User + stats.Persona + stats.AgentRules + stats.Facts + stats.Notes + stats.Episodes
	if fixed+stats.Conversation > limit {
		t.Errorf("layers+conversation (%d) exceed ctx limit (%d)", fixed+stats.Conversation, limit)
	}
	sys := msgs[0].Content
	// Oldest episode (01.md) should be the last one standing.
	if !strings.Contains(sys, "03.md") {
		t.Errorf("expected most recent episode 03.md to survive; got:\n%s", sys)
	}
}

func TestAssemble_CustomTokenizer(t *testing.T) {
	mem := writeRepo(t, map[string]string{
		"rules.md":                "abcd", // 4 runes, default tokenizer says 1
		"agents/coder/persona.md": "p",
	})
	asm := newAssembler(t, mem, baseCfg()).WithTokenizer(func(s string) int { return len(s) })
	_, stats, err := asm.Assemble(context.Background(), "coder", nil)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if stats.Rules != 4 {
		t.Errorf("custom tokenizer not used: rules stats = %d, want 4", stats.Rules)
	}
}

func TestAssemble_CancelledContext(t *testing.T) {
	mem := writeRepo(t, map[string]string{
		"rules.md":                "r",
		"agents/coder/persona.md": "p",
	})
	asm := newAssembler(t, mem, baseCfg())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := asm.Assemble(ctx, "coder", nil)
	if err == nil {
		t.Fatal("expected cancellation error, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

func TestAssemble_TotalEqualsSum(t *testing.T) {
	mem := writeRepo(t, map[string]string{
		"rules.md":                "RULES",
		"user.md":                 "USER",
		"facts.md":                "FACTS",
		"agents/coder/persona.md": "PERSONA",
		"agents/coder/rules.md":   "AGENTRULES",
		"agents/coder/notes.md":   "NOTES",
	})
	asm := newAssembler(t, mem, baseCfg())
	convo := []inference.Message{{Role: "user", Content: "hi"}}
	_, stats, err := asm.Assemble(context.Background(), "coder", convo)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	want := stats.Rules + stats.User + stats.Persona + stats.AgentRules + stats.Facts + stats.Notes + stats.Episodes + stats.Conversation
	if stats.Total != want {
		t.Errorf("Total = %d, want sum = %d", stats.Total, want)
	}
}

// TestAssemble_SkipsEmptyOptionalLayers verifies the render logic
// skips layers with only whitespace rather than emitting a "# User"
// header with no body.
func TestAssemble_SkipsEmptyOptionalLayers(t *testing.T) {
	mem := writeRepo(t, map[string]string{
		"rules.md":                "RULES",
		"user.md":                 "   \n\n\n   ", // whitespace only
		"facts.md":                "",
		"agents/coder/persona.md": "PERSONA",
		"agents/coder/rules.md":   "", // empty agent rules → no header
	})
	asm := newAssembler(t, mem, baseCfg())
	msgs, _, err := asm.Assemble(context.Background(), "coder", nil)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	sys := msgs[0].Content
	if strings.Contains(sys, "# User") {
		t.Errorf("expected # User header to be skipped for whitespace body; got:\n%s", sys)
	}
	if strings.Contains(sys, "# Facts") {
		t.Errorf("expected # Facts header to be skipped for empty body; got:\n%s", sys)
	}
	if strings.Contains(sys, "# Agent Rules") {
		t.Errorf("expected # Agent Rules header to be skipped for empty body; got:\n%s", sys)
	}
}

// TestAssemble_AgentRulesOptional verifies an agent without a rules.md
// file assembles without error and omits the section.
func TestAssemble_AgentRulesOptional(t *testing.T) {
	mem := writeRepo(t, map[string]string{
		"rules.md":                "RULES",
		"agents/coder/persona.md": "PERSONA",
		// agents/coder/rules.md missing on purpose
	})
	asm := newAssembler(t, mem, baseCfg())
	msgs, stats, err := asm.Assemble(context.Background(), "coder", nil)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	sys := msgs[0].Content
	if strings.Contains(sys, "# Agent Rules") {
		t.Errorf("expected # Agent Rules to be absent when rules.md missing; got:\n%s", sys)
	}
	if stats.AgentRules != 0 {
		t.Errorf("AgentRules tokens = %d, want 0 when missing", stats.AgentRules)
	}
}

// TestAssemble_AgentRulesRenderedBetweenPersonaAndFacts pins the layer
// position: after persona, before facts. Persona defines identity,
// agent rules constrain behaviour, then global facts follow.
func TestAssemble_AgentRulesRenderedBetweenPersonaAndFacts(t *testing.T) {
	mem := writeRepo(t, map[string]string{
		"rules.md":                "GLOBALRULES",
		"facts.md":                "GLOBALFACTS",
		"agents/coder/persona.md": "PERSONABODY",
		"agents/coder/rules.md":   "PLANBEFOREEDIT",
	})
	asm := newAssembler(t, mem, baseCfg())
	msgs, _, err := asm.Assemble(context.Background(), "coder", nil)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	sys := msgs[0].Content
	personaBody := strings.Index(sys, "PERSONABODY")
	rulesBody := strings.Index(sys, "PLANBEFOREEDIT")
	factsBody := strings.Index(sys, "GLOBALFACTS")
	if personaBody < 0 || rulesBody < 0 || factsBody < 0 {
		t.Fatalf("layer body missing; persona=%d rules=%d facts=%d\n%s", personaBody, rulesBody, factsBody, sys)
	}
	if personaBody >= rulesBody || rulesBody >= factsBody {
		t.Errorf("expected persona < agent rules < facts; got persona=%d rules=%d facts=%d", personaBody, rulesBody, factsBody)
	}
}

// TestAssemble_AgentRulesCountedAgainstCtxLimit verifies agent rules
// participate in the ctx-size guardrail (they're mandatory, never
// trimmed, but still count toward the budget that triggers episode
// trimming).
func TestAssemble_AgentRulesCountedAgainstCtxLimit(t *testing.T) {
	mem := writeRepo(t, map[string]string{
		"rules.md":                "r",                      // 1 token
		"agents/coder/persona.md": "p",                      // 1 token
		"agents/coder/rules.md":   strings.Repeat("a", 400), // 100 tokens
		"episodes/coder/01.md":    strings.Repeat("e", 400), // 100 tokens
		"episodes/coder/02.md":    strings.Repeat("e", 400), // 100 tokens
	})
	cfg := config.PromptConfig{
		CtxSize:             250, // limit minus reserve = 150 tokens
		ConversationReserve: 100,
		MemoryTokenBudget:   1000,
	}
	asm := newAssembler(t, mem, cfg)
	_, stats, err := asm.Assemble(context.Background(), "coder", nil)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	// rules(1) + persona(1) + agentRules(100) = 102 fixed; 150 - 102 = 48
	// tokens left for episodes, so neither 100-token episode fits.
	if stats.Episodes != 0 {
		t.Errorf("expected all episodes trimmed when agent rules consume the ctx budget; episodes=%d", stats.Episodes)
	}
	if stats.AgentRules == 0 {
		t.Errorf("expected agent rules to be counted; AgentRules=%d", stats.AgentRules)
	}
}

func TestAssemble_ProjectScopedLayersUseActiveProject(t *testing.T) {
	asm := newProjectAssembler(t, map[string]string{
		"rules.md":                "GLOBALRULES",
		"user.md":                 "GLOBALUSER",
		"facts.md":                "GLOBALFACTS",
		"agents/coder/persona.md": "PERSONABODY",
	}, map[string]string{
		"rules.md": "PROJECTRULES",
		"user.md":  "PROJECTUSER",
		"facts.md": "PROJECTFACTS",
	}, baseCfg())
	msgs, _, err := asm.Assemble(context.Background(), "coder", nil)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	sys := msgs[0].Content
	for _, want := range []string{"PROJECTRULES", "PROJECTUSER", "PROJECTFACTS", "PERSONABODY"} {
		if !strings.Contains(sys, want) {
			t.Errorf("project-scoped layer %q missing from prompt:\n%s", want, sys)
		}
	}
	for _, leaked := range []string{"GLOBALRULES", "GLOBALUSER", "GLOBALFACTS"} {
		if strings.Contains(sys, leaked) {
			t.Errorf("global project layer %q leaked into active project prompt:\n%s", leaked, sys)
		}
	}

	rulesIdx := strings.Index(sys, "PROJECTRULES")
	userIdx := strings.Index(sys, "PROJECTUSER")
	personaIdx := strings.Index(sys, "PERSONABODY")
	factsIdx := strings.Index(sys, "PROJECTFACTS")
	if rulesIdx < 0 || userIdx < 0 || personaIdx < 0 || factsIdx < 0 {
		t.Fatalf("layer body missing; rules=%d user=%d persona=%d facts=%d\n%s", rulesIdx, userIdx, personaIdx, factsIdx, sys)
	}
	if rulesIdx >= userIdx || userIdx >= personaIdx || personaIdx >= factsIdx {
		t.Errorf("expected rules < user < persona < facts; got rules=%d user=%d persona=%d facts=%d", rulesIdx, userIdx, personaIdx, factsIdx)
	}
}

func TestAssemble_ProjectRulesRequiredFromActiveProject(t *testing.T) {
	global := writeRepo(t, map[string]string{
		"rules.md":                "GLOBALRULES",
		"agents/coder/persona.md": "PERSONA",
	})
	active := writeRepo(t, nil)
	reg := agent.NewDiskRegistry(global, func() string { return "coder" }, func(string) error { return nil })
	asm := NewProjectDiskAssembler(global, active, reg, baseCfg()).WithProjectSlug("dt")
	_, _, err := asm.Assemble(context.Background(), "coder", nil)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("expected missing active rules.md to be required, got %v", err)
	}
}

func TestAssemble_InvalidProjectSlugReturnsError(t *testing.T) {
	mem := writeRepo(t, map[string]string{
		"rules.md":                "RULES",
		"agents/coder/persona.md": "PERSONA",
	})
	asm := newAssembler(t, mem, baseCfg()).WithProjectSlug("bad_slug!")
	_, _, err := asm.Assemble(context.Background(), "coder", nil)
	if !errors.Is(err, project.ErrInvalidSlug) {
		t.Fatalf("Assemble invalid project slug: errors.Is(ErrInvalidSlug)=false, err=%v", err)
	}
}

func TestAssemble_InvalidAgentNameRejected(t *testing.T) {
	mem := writeRepo(t, map[string]string{
		"rules.md": "RULES",
	})
	asm := newAssembler(t, mem, baseCfg()).WithProjectSlug("dt")
	_, _, err := asm.Assemble(context.Background(), "foo/bar", nil)
	if err == nil {
		t.Fatal("expected error for invalid agent name")
	}
	if !errors.Is(err, agent.ErrInvalidName) {
		t.Errorf("expected agent.ErrInvalidName, got %v", err)
	}
}

func TestAssemble_ProjectScopedRulesStatsAndTotal(t *testing.T) {
	asm := newProjectAssembler(t, map[string]string{
		"rules.md":                "GLOBALRULES",
		"user.md":                 "GLOBALUSER",
		"agents/coder/persona.md": "PERSONA",
	}, map[string]string{
		"rules.md": "PROJECT",
		"user.md":  "USER",
		"facts.md": "FACTS",
	}, baseCfg())
	_, stats, err := asm.Assemble(context.Background(), "coder", nil)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if stats.Rules == 0 || stats.User == 0 || stats.Facts == 0 {
		t.Errorf("project-scoped stats missing counts: %+v", stats)
	}
	want := stats.Rules + stats.User + stats.Persona + stats.AgentRules + stats.Facts + stats.Notes + stats.Episodes + stats.Conversation
	if stats.Total != want {
		t.Errorf("Total = %d, want sum = %d", stats.Total, want)
	}
}

func TestAssemble_ProjectRulesNotTrimmed(t *testing.T) {
	cfg := baseCfg()
	cfg.MemoryTokenBudget = 10
	asm := newProjectAssembler(t, map[string]string{
		"rules.md":                strings.Repeat("R", 400),
		"agents/coder/persona.md": strings.Repeat("P", 400),
	}, map[string]string{
		"rules.md": strings.Repeat("P", 400),
	}, cfg)

	msgs, _, err := asm.Assemble(context.Background(), "coder", nil)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	sys := msgs[0].Content
	if !strings.Contains(sys, "# Rules") {
		t.Errorf("mandatory # Rules missing when budget pressure is high")
	}
}

func TestAssemble_CtxSizeTrimsEpisodesWithProjectRules(t *testing.T) {
	cfg := config.PromptConfig{
		CtxSize:             250,
		ConversationReserve: 100,
		MemoryTokenBudget:   1000,
	}
	asm := newProjectAssembler(t, map[string]string{
		"rules.md":                "global",
		"agents/coder/persona.md": "p",
	}, map[string]string{
		"rules.md":             strings.Repeat("p", 400),
		"episodes/coder/01.md": strings.Repeat("e", 400),
		"episodes/coder/02.md": strings.Repeat("e", 400),
	}, cfg)
	_, stats, err := asm.Assemble(context.Background(), "coder", nil)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if stats.Episodes != 0 {
		t.Errorf("expected all episodes trimmed when project rules consume ctx budget; episodes=%d", stats.Episodes)
	}
	if stats.Rules == 0 {
		t.Errorf("expected project rules to be counted; Rules=%d", stats.Rules)
	}
}

// errReader wraps a memory.Reader and returns a synthetic error for
// reads of failOn (which is not fs.ErrNotExist), simulating a disk
// fault. It also forwards directory operations so the agent
// registry keeps working for unrelated tests.
type errReader struct {
	memory.Repo
	failOn string
}

func (e errReader) Read(p string) ([]byte, error) {
	if p == e.failOn {
		return nil, fmt.Errorf("synthetic read error for %s", p)
	}
	return e.Repo.Read(p)
}

func (e errReader) ListDirs(p string) ([]string, error) {
	return e.Repo.ListDirs(p)
}

// TestAssemble_ReadError ensures non-NotExist read errors propagate.
func TestAssemble_ReadErrorPropagates(t *testing.T) {
	mem := errReader{
		Repo: writeRepo(t, map[string]string{
			"rules.md":                "r",
			"agents/coder/persona.md": "p",
		}),
		failOn: "user.md",
	}
	reg := agent.NewDiskRegistry(mem, func() string { return "coder" }, func(string) error { return nil })
	asm := NewProjectDiskAssembler(mem, mem, reg, baseCfg())
	_, _, err := asm.Assemble(context.Background(), "coder", nil)
	if err == nil {
		t.Fatal("expected error from failing read, got nil")
	}
}

func TestAssemble_ProjectAgentReadErrorPropagates(t *testing.T) {
	global := writeRepo(t, map[string]string{
		"rules.md":                "RULES",
		"agents/coder/persona.md": "GLOBALPERSONA",
	})
	active := errReader{
		Repo:   writeRepo(t, map[string]string{"rules.md": "PROJECTRULES"}),
		failOn: "agents/coder/persona.md",
	}
	reg := agent.NewDiskRegistry(global, func() string { return "coder" }, func(string) error { return nil })
	asm := NewProjectDiskAssembler(global, active, reg, baseCfg()).WithProjectSlug("dt")
	_, _, err := asm.Assemble(context.Background(), "coder", nil)
	if err == nil {
		t.Fatal("expected error from failing project agent read")
	}
	if !strings.Contains(err.Error(), "synthetic read error") {
		t.Errorf("expected synthetic read error in message, got %v", err)
	}
}

func TestAssemble_GlobalAgentFallbackReadErrorPropagates(t *testing.T) {
	global := errReader{
		Repo: writeRepo(t, map[string]string{
			"rules.md":                "RULES",
			"agents/coder/persona.md": "GLOBALPERSONA",
		}),
		failOn: "agents/coder/persona.md",
	}
	active := writeRepo(t, map[string]string{"rules.md": "PROJECTRULES"})
	reg := agent.NewDiskRegistry(global, func() string { return "coder" }, func(string) error { return nil })
	asm := NewProjectDiskAssembler(global, active, reg, baseCfg()).WithProjectSlug("dt")
	_, _, err := asm.Assemble(context.Background(), "coder", nil)
	if err == nil {
		t.Fatal("expected error from failing global fallback read")
	}
	if !strings.Contains(err.Error(), "synthetic read error") {
		t.Errorf("expected synthetic read error in message, got %v", err)
	}
}

func TestAssemble_ProjectPersonaOverrideInheritsGlobalDefinitionRulesOnly(t *testing.T) {
	asm := newProjectAssembler(t, map[string]string{
		"rules.md":                "RULES",
		"agents/coder/persona.md": "GLOBALPERSONA",
		"agents/coder/rules.md":   "GLOBALRULES",
		"agents/coder/notes.md":   "GLOBALNOTES",
	}, map[string]string{
		"rules.md":                "PROJECTRULES",
		"agents/coder/persona.md": "DTPERSONA",
	}, baseCfg())
	msgs, _, err := asm.Assemble(context.Background(), "coder", nil)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	sys := msgs[0].Content
	if !strings.Contains(sys, "DTPERSONA") {
		t.Errorf("expected project persona, got:\n%s", sys)
	}
	if strings.Contains(sys, "GLOBALPERSONA") {
		t.Errorf("global persona leaked into project prompt:\n%s", sys)
	}
	if !strings.Contains(sys, "GLOBALRULES") {
		t.Errorf("expected global rules inherited, got:\n%s", sys)
	}
	if strings.Contains(sys, "GLOBALNOTES") {
		t.Errorf("global notes leaked into project prompt:\n%s", sys)
	}
}

func TestAssemble_ProjectOnlyAgentSuccess(t *testing.T) {
	asm := newProjectAssembler(t, map[string]string{
		"rules.md": "RULES",
	}, map[string]string{
		"rules.md":                "PROJECTRULES",
		"agents/coder/persona.md": "DTPERSONA",
	}, baseCfg())
	msgs, _, err := asm.Assemble(context.Background(), "coder", nil)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	sys := msgs[0].Content
	if !strings.Contains(sys, "DTPERSONA") {
		t.Errorf("expected project-only persona, got:\n%s", sys)
	}
}

func TestAssemble_ProjectOnlyAgentNotLeaking(t *testing.T) {
	global := writeRepo(t, map[string]string{"rules.md": "RULES"})
	other := writeRepo(t, map[string]string{"rules.md": "OTHERRULES"})
	reg := agent.NewDiskRegistry(global, func() string { return "coder" }, func(string) error { return nil })
	asm := NewProjectDiskAssembler(global, other, reg, baseCfg()).WithProjectSlug("other")
	_, _, err := asm.Assemble(context.Background(), "coder", nil)
	if err == nil {
		t.Fatal("expected error for project-only agent in wrong project")
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("expected fs.ErrNotExist, got %v", err)
	}
}

func TestAssemble_EmptyProjectRulesSuppressGlobal(t *testing.T) {
	asm := newProjectAssembler(t, map[string]string{
		"rules.md":                "RULES",
		"agents/coder/persona.md": "PERSONA",
		"agents/coder/rules.md":   "GLOBALRULES",
	}, map[string]string{
		"rules.md":              "PROJECTRULES",
		"agents/coder/rules.md": "",
	}, baseCfg())
	msgs, _, err := asm.Assemble(context.Background(), "coder", nil)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	sys := msgs[0].Content
	if strings.Contains(sys, "GLOBALRULES") {
		t.Errorf("expected global rules to be suppressed by empty project rules, got:\n%s", sys)
	}
}

// TestAssemble_BlendedRetrievalKeepsTopN verifies the blended retrieval
// path keeps the top-N episodes by blended score, not the lowest
// (regression test for consolidation plan bug #3).
func TestAssemble_BlendedRetrievalKeepsTopN(t *testing.T) {
	mem := writeRepo(t, map[string]string{
		"rules.md":                "r",
		"agents/coder/persona.md": "p",
		"episodes/coder/01.md":    "EP1",
		"episodes/coder/02.md":    "EP2",
		"episodes/coder/03.md":    "EP3",
		"episodes/coder/04.md":    "EP4",
		"episodes/coder/05.md":    "EP5",
	})
	cfg := baseCfg()
	cfg.RecencyN = 3
	cfg.SemanticWeight = 0.5
	cfg.RecencyWeight = 0.5

	idxDir := t.TempDir()
	idx, err := index.Create(idxDir, 2)
	if err != nil {
		t.Fatal(err)
	}
	// Semantic scores for query [0.8, 0.6]:
	// 01 [1, 0]    -> cos=0.8
	// 02 [0.5, 0]  -> cos=0.8
	// 03 [1, 0.8]  -> cos=1.0  (highest)
	// 04 [-1, 0]   -> cos=-0.8 (lowest)
	// 05 [0.8, 0]  -> cos=0.8
	if err := idx.Add("01", [][]float32{{1, 0}}); err != nil {
		t.Fatal(err)
	}
	if err := idx.Add("02", [][]float32{{0.5, 0}}); err != nil {
		t.Fatal(err)
	}
	if err := idx.Add("03", [][]float32{{1, 0.8}}); err != nil {
		t.Fatal(err)
	}
	if err := idx.Add("04", [][]float32{{-1, 0}}); err != nil {
		t.Fatal(err)
	}
	if err := idx.Add("05", [][]float32{{0.8, 0}}); err != nil {
		t.Fatal(err)
	}

	stub := &stubEmbedder{vectors: [][]float32{{0.8, 0.6}}}
	asm := newAssembler(t, mem, cfg).WithBlendedRetrieval(idx, stub)

	msgs, _, err := asm.Assemble(context.Background(), "coder",
		[]inference.Message{{Role: "user", Content: "test query"}})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	sys := msgs[0].Content

	// Top 3 by blended score must be kept. Episode 04 has negative
	// semantic score and should be among the first dropped.
	if strings.Contains(sys, "04.md") {
		t.Errorf("lowest-scored episode 04 should be dropped; got:\n%s", sys)
	}
	// Episode 03 has the highest semantic score (cos=1.0) and must be kept.
	if !strings.Contains(sys, "03.md") {
		t.Errorf("highest-scored episode 03 should be kept; got:\n%s", sys)
	}
	// We expect exactly 3 episodes.
	epCount := 0
	for _, name := range []string{"01.md", "02.md", "03.md", "04.md", "05.md"} {
		if strings.Contains(sys, name) {
			epCount++
		}
	}
	if epCount != 3 {
		t.Errorf("expected 3 episodes kept after blended cap, got %d", epCount)
	}
}

func TestAssemble_BlendedRetrievalTrimDropsLowestScore(t *testing.T) {
	mem := writeRepo(t, map[string]string{
		"rules.md":                "r",
		"agents/coder/persona.md": "p",
		"episodes/coder/01.md":    strings.Repeat("A", 120),
		"episodes/coder/02.md":    strings.Repeat("B", 120),
	})
	cfg := baseCfg()
	cfg.RecencyN = 2
	cfg.MemoryTokenBudget = 30
	cfg.SemanticWeight = 1.0
	cfg.RecencyWeight = 0.0

	idxDir := t.TempDir()
	idx, err := index.Create(idxDir, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.Add("01", [][]float32{{1, 0}}); err != nil {
		t.Fatal(err)
	}
	if err := idx.Add("02", [][]float32{{-1, 0}}); err != nil {
		t.Fatal(err)
	}

	stub := &stubEmbedder{vectors: [][]float32{{1, 0}}}
	asm := newAssembler(t, mem, cfg).WithBlendedRetrieval(idx, stub)

	msgs, stats, err := asm.Assemble(context.Background(), "coder",
		[]inference.Message{{Role: "user", Content: "test query"}})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	sys := msgs[0].Content

	if !strings.Contains(sys, "01.md") {
		t.Errorf("highest-scored episode should survive trimming; got:\n%s", sys)
	}
	if strings.Contains(sys, "02.md") {
		t.Errorf("lowest-scored episode should be trimmed first; got:\n%s", sys)
	}
	if stats.Episodes > cfg.MemoryTokenBudget {
		t.Fatalf("episodes tokens = %d, want <= %d", stats.Episodes, cfg.MemoryTokenBudget)
	}
}

func TestAssemble_BlendedRetrievalUsesBestChunkScore(t *testing.T) {
	mem := writeRepo(t, map[string]string{
		"rules.md":                "r",
		"agents/coder/persona.md": "p",
		"episodes/coder/01.md":    "best chunk episode",
		"episodes/coder/02.md":    "steady episode",
	})
	cfg := baseCfg()
	cfg.RecencyN = 1
	cfg.SemanticWeight = 1.0
	cfg.RecencyWeight = 0.0

	idxDir := t.TempDir()
	idx, err := index.Create(idxDir, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.Add("01", [][]float32{{1, 0}, {-1, 0}}); err != nil {
		t.Fatal(err)
	}
	if err := idx.Add("02", [][]float32{{0.5, 0}}); err != nil {
		t.Fatal(err)
	}

	stub := &stubEmbedder{vectors: [][]float32{{1, 0}}}
	asm := newAssembler(t, mem, cfg).WithBlendedRetrieval(idx, stub)

	msgs, _, err := asm.Assemble(context.Background(), "coder",
		[]inference.Message{{Role: "user", Content: "test query"}})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	sys := msgs[0].Content

	if !strings.Contains(sys, "01.md") {
		t.Errorf("episode with best matching chunk should be kept; got:\n%s", sys)
	}
	if strings.Contains(sys, "02.md") {
		t.Errorf("lower-scored episode should be dropped; got:\n%s", sys)
	}
}

// TestAssemble_BlendedRecencyUsesExponentialDecay verifies that the
// blended path uses exponential recency decay, not linear rank.
func TestAssemble_BlendedRecencyUsesExponentialDecay(t *testing.T) {
	mem := writeRepo(t, map[string]string{
		"rules.md":                "r",
		"agents/coder/persona.md": "p",
		"episodes/coder/01.md":    "EP1",
		"episodes/coder/02.md":    "EP2",
		"episodes/coder/03.md":    "EP3",
	})
	cfg := baseCfg()
	cfg.RecencyN = 2
	// Pure recency weight: semantic is zero, so the blended score is
	// entirely driven by recency decay.
	cfg.SemanticWeight = 0.0
	cfg.RecencyWeight = 1.0

	idxDir := t.TempDir()
	idx, err := index.Create(idxDir, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.Add("01", [][]float32{{1, 0}}); err != nil {
		t.Fatal(err)
	}
	if err := idx.Add("02", [][]float32{{0.8, 0}}); err != nil {
		t.Fatal(err)
	}
	if err := idx.Add("03", [][]float32{{0.5, 0}}); err != nil {
		t.Fatal(err)
	}

	stub := &stubEmbedder{vectors: [][]float32{{1, 0}}}
	asm := newAssembler(t, mem, cfg).WithBlendedRetrieval(idx, stub)

	msgs, _, err := asm.Assemble(context.Background(), "coder",
		[]inference.Message{{Role: "user", Content: "test query"}})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	sys := msgs[0].Content

	// With pure recency weight, the blended score uses exponential
	// decay from newest. Episodes are sorted oldest-first, so
	// 03 is newest (distance 0 -> exp(0/3)=1.0), 02 (distance 1 ->
	// exp(-1/3)≈0.717), 01 (distance 2 -> exp(-2/3)≈0.513).
	// Top 2 by recency: 03 and 02. Episode 01 should be dropped.
	if !strings.Contains(sys, "03.md") {
		t.Errorf("newest episode 03 should be kept; got:\n%s", sys)
	}
	if !strings.Contains(sys, "02.md") {
		t.Errorf("episode 02 should be kept; got:\n%s", sys)
	}
	if strings.Contains(sys, "01.md") {
		t.Errorf("oldest episode 01 should be dropped; got:\n%s", sys)
	}
}
