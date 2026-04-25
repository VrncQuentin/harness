package prompt

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vrnc/harness/internal/agent"
	"github.com/vrnc/harness/internal/config"
	"github.com/vrnc/harness/internal/inference"
	"github.com/vrnc/harness/internal/memory"
)

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
	return NewDiskAssembler(mem, reg, cfg)
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
		"global/rules.md": "RULES",
	})
	asm := newAssembler(t, mem, baseCfg())
	_, _, err := asm.Assemble(context.Background(), "", nil)
	if !errors.Is(err, ErrAgentRequired) {
		t.Errorf("expected ErrAgentRequired, got %v", err)
	}
}

func TestAssemble_AgentRequiresPersona(t *testing.T) {
	mem := writeRepo(t, map[string]string{
		"global/rules.md": "x",
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
		"global/rules.md":                     "RULES",
		"global/user.md":                      "USER",
		"global/facts.md":                     "FACTS",
		"agents/coder/persona.md":             "PERSONA",
		"agents/coder/notes.md":               "NOTES",
		"agents/coder/episodes/2026-01-01.md": "EP1",
		"agents/coder/episodes/2026-02-01.md": "EP2",
	})
	asm := newAssembler(t, mem, baseCfg())
	msgs, stats, err := asm.Assemble(context.Background(), "coder", nil)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	sys := msgs[0].Content
	// Verify section order by checking indices.
	headers := []string{"# Rules", "# User", "# Persona", "# Facts", "# Notes", "# Episodes"}
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

	if stats.Rules == 0 || stats.Persona == 0 || stats.Episodes == 0 {
		t.Errorf("stats missing counts: %+v", stats)
	}
}

func TestAssemble_ConversationAppendedVerbatim(t *testing.T) {
	mem := writeRepo(t, map[string]string{
		"global/rules.md":         "RULES",
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
	if msgs[1] != convo[0] || msgs[2] != convo[1] {
		t.Errorf("conversation mutated during assembly")
	}
}

func TestAssemble_TrimsEpisodesOldestFirstForMemoryBudget(t *testing.T) {
	// Generate five episodes with increasing content size; use a
	// deterministic tokenizer so we can reason about counts.
	mem := writeRepo(t, map[string]string{
		"global/rules.md":             "r",
		"agents/coder/persona.md":     "p",
		"agents/coder/episodes/01.md": strings.Repeat("a", 200),
		"agents/coder/episodes/02.md": strings.Repeat("b", 200),
		"agents/coder/episodes/03.md": strings.Repeat("c", 200),
		"agents/coder/episodes/04.md": strings.Repeat("d", 200),
		"agents/coder/episodes/05.md": strings.Repeat("e", 200),
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

func TestAssemble_RulesPersonaUserNeverTrimmed(t *testing.T) {
	// Set a memory budget that is smaller than rules+user+persona
	// together; these must still be present.
	mem := writeRepo(t, map[string]string{
		"global/rules.md":         strings.Repeat("R", 400), // 100 tokens
		"global/user.md":          strings.Repeat("U", 400),
		"global/facts.md":         strings.Repeat("F", 400),
		"agents/coder/persona.md": strings.Repeat("P", 400),
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
	for _, section := range []string{"# Rules", "# User", "# Persona"} {
		if !strings.Contains(sys, section) {
			t.Errorf("mandatory %q missing when budget pressure is high", section)
		}
	}
}

func TestAssemble_CtxSizeTrimsAgainstConversationReserve(t *testing.T) {
	mem := writeRepo(t, map[string]string{
		"global/rules.md":             "r",                      // 1 token
		"agents/coder/persona.md":     "p",                      // 1 token
		"agents/coder/episodes/01.md": strings.Repeat("a", 400), // 100 tokens
		"agents/coder/episodes/02.md": strings.Repeat("b", 400),
		"agents/coder/episodes/03.md": strings.Repeat("c", 400),
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
	fixed := stats.Rules + stats.User + stats.Persona + stats.Facts + stats.Notes + stats.Episodes
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
		"global/rules.md":         "abcd", // 4 runes, default tokenizer says 1
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
		"global/rules.md":         "r",
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
		"global/rules.md":         "RULES",
		"global/user.md":          "USER",
		"global/facts.md":         "FACTS",
		"agents/coder/persona.md": "PERSONA",
		"agents/coder/notes.md":   "NOTES",
	})
	asm := newAssembler(t, mem, baseCfg())
	convo := []inference.Message{{Role: "user", Content: "hi"}}
	_, stats, err := asm.Assemble(context.Background(), "coder", convo)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	want := stats.Rules + stats.User + stats.Persona + stats.Facts + stats.Notes + stats.Episodes + stats.Conversation
	if stats.Total != want {
		t.Errorf("Total = %d, want sum = %d", stats.Total, want)
	}
}

// TestAssemble_SkipsEmptyOptionalLayers verifies the render logic
// skips layers with only whitespace rather than emitting a "# User"
// header with no body.
func TestAssemble_SkipsEmptyOptionalLayers(t *testing.T) {
	mem := writeRepo(t, map[string]string{
		"global/rules.md":         "RULES",
		"global/user.md":          "   \n\n\n   ", // whitespace only
		"global/facts.md":         "",
		"agents/coder/persona.md": "PERSONA",
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
}

// TestAssemble_ReadError ensures non-NotExist read errors propagate.
func TestAssemble_ReadErrorPropagates(t *testing.T) {
	mem := errReader{
		Reader: writeRepo(t, map[string]string{
			"global/rules.md":         "r",
			"agents/coder/persona.md": "p",
		}),
		failOn: "global/user.md",
	}
	reg := agent.NewDiskRegistry(mem, func() string { return "coder" }, func(string) error { return nil })
	asm := NewDiskAssembler(mem, reg, baseCfg())
	_, _, err := asm.Assemble(context.Background(), "coder", nil)
	if err == nil {
		t.Fatal("expected error from failing read, got nil")
	}
}

// errReader wraps a memory.Reader and returns a synthetic error for
// reads of failOn (which is not fs.ErrNotExist), simulating a disk
// fault. It also forwards DirLister when available so the agent
// registry keeps working for unrelated tests.
type errReader struct {
	memory.Reader
	failOn string
}

func (e errReader) Read(p string) ([]byte, error) {
	if p == e.failOn {
		return nil, fmt.Errorf("synthetic read error on %s", p)
	}
	return e.Reader.Read(p)
}

func (e errReader) ListDirs(rel string) ([]string, error) {
	if dl, ok := e.Reader.(memory.DirLister); ok {
		return dl.ListDirs(rel)
	}
	return nil, nil
}
