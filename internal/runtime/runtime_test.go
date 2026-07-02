package runtime

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/vrnc/harness/internal/agent"
	"github.com/vrnc/harness/internal/agentloop"
	"github.com/vrnc/harness/internal/approvals"
	"github.com/vrnc/harness/internal/config"
	"github.com/vrnc/harness/internal/index"
	"github.com/vrnc/harness/internal/inference"
	"github.com/vrnc/harness/internal/memory"
	"github.com/vrnc/harness/internal/proc"
	"github.com/vrnc/harness/internal/queue"
	"github.com/vrnc/harness/internal/ui"
)

func TestNewStoresInitialConfig(t *testing.T) {
	cfg := config.Defaults()
	cfg.Agent.Active = "coder"

	rt := New(cfg, nil, LogRings{})

	if got := rt.getActiveAgent(); got != "coder" {
		t.Fatalf("active agent = %q, want coder", got)
	}
}

func TestNewEventChannelUsesRuntimeBuffer(t *testing.T) {
	ch := NewEventChannel()

	if cap(ch) != EventBufferSize {
		t.Fatalf("event channel cap = %d, want %d", cap(ch), EventBufferSize)
	}
}

func TestRestartCallbacksTolerateMissingManagers(t *testing.T) {
	rt := New(config.Defaults(), nil, LogRings{})

	rt.RestartLlama()
	rt.RestartEmbedder()
}

func TestStartMemoryAndAPIInvalidRepoDoesNotBindAPI(t *testing.T) {
	port := freeTCPPort(t)
	cfg := config.Defaults()
	cfg.Memory.RepoPath = t.TempDir()
	cfg.API.Enabled = true
	cfg.API.Port = port

	rt := New(cfg, nil, LogRings{})
	rt.reqQueue = queue.New(1, "", nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rt.startMemoryAndAPI(ctx, ui.NewServer(0), nil)

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("API bound despite invalid memory repo: %v", err)
	}
	_ = ln.Close()
}

func TestPushStatusNilManagerSkipsSetter(t *testing.T) {
	called := false
	pushStatus(nil, "llama-server", func(ui.ProcessStatus) {
		called = true
	})
	if called {
		t.Fatalf("setter invoked for nil manager")
	}
}

func TestPushStatusPopulatesStatusFromManager(t *testing.T) {
	mgr := proc.NewManager(proc.ManagerConfig{Name: "llama-server"})

	var got ui.ProcessStatus
	pushStatus(mgr, "llama-server", func(st ui.ProcessStatus) {
		got = st
	})

	if got.Name != "llama-server" {
		t.Errorf("Name = %q, want llama-server", got.Name)
	}
	// A freshly-built manager has zero state for the rest; we only assert
	// that pushStatus copied the snapshot through, not the manager's logic.
	if got.Running || got.Healthy || got.Failed || got.RestartCount != 0 {
		t.Errorf("fresh manager produced non-zero status: %+v", got)
	}
}

func TestUIAgentRegistryAdapterListMatchesGet(t *testing.T) {
	mem := newMemoryRepo(t, map[string]string{
		"agents/coder/persona.md":    "coder persona",
		"agents/coder/rules.md":      "coder rules",
		"agents/coder/notes.md":      "coder notes",
		"agents/reviewer/persona.md": "reviewer persona",
	})
	var active string
	reg := agent.NewDiskRegistry(mem,
		func() string { return active },
		func(name string) error { active = name; return nil },
	)
	ad := &uiAgentRegistryAdapter{reg: reg, mem: mem}

	list, err := ad.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("List returned %d agents, want 2", len(list))
	}

	for _, listed := range list {
		fromGet, err := ad.Get(listed.Name)
		if err != nil {
			t.Errorf("Get(%q): %v", listed.Name, err)
			continue
		}
		if !reflect.DeepEqual(listed, fromGet) {
			t.Errorf("List entry for %q diverges from Get:\n list = %+v\n  get = %+v",
				listed.Name, listed, fromGet)
		}
	}
}

func TestUIAgentRegistryAdapterListTreatsMissingFilesAsEmpty(t *testing.T) {
	// Only persona.md is on disk; rules and notes are absent. The adapter
	// must surface the agent with empty Rules/Notes rather than skip it.
	mem := newMemoryRepo(t, map[string]string{
		"agents/coder/persona.md": "P",
	})
	var active string
	reg := agent.NewDiskRegistry(mem,
		func() string { return active },
		func(name string) error { active = name; return nil },
	)
	ad := &uiAgentRegistryAdapter{reg: reg, mem: mem}

	list, err := ad.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 || list[0].Name != "coder" {
		t.Fatalf("List = %+v, want one agent named coder", list)
	}
	if list[0].Persona != "P" {
		t.Errorf("Persona = %q, want P", list[0].Persona)
	}
	if list[0].Rules != "" {
		t.Errorf("Rules = %q, want empty (file missing)", list[0].Rules)
	}
	if list[0].Notes != "" {
		t.Errorf("Notes = %q, want empty (file missing)", list[0].Notes)
	}
}

func TestIndexRebuilderCreatesMissingEpisodeIndex(t *testing.T) {
	root := t.TempDir()
	episodePath := filepath.Join(root, "projects", "global", "episodes", "coder", "ep1.md")
	if err := os.MkdirAll(filepath.Dir(episodePath), 0o755); err != nil {
		t.Fatalf("MkdirAll episode dir: %v", err)
	}
	if err := os.WriteFile(episodePath, []byte("episode body"), 0o644); err != nil {
		t.Fatalf("WriteFile episode: %v", err)
	}
	indexDir := filepath.Join(root, "projects", "global", "index", "_episodes")
	called := false
	rb := &indexRebuilder{
		mem:      memory.NewDirReader(root),
		emb:      stubEmbedder{vec: []float32{1, 0}},
		indexDir: indexDir,
		slug:     "global",
		onRebuilt: func(idx *index.Index) {
			called = true
			if !idx.Contains("ep1") {
				t.Errorf("rebuilt index missing ep1")
			}
		},
	}

	if err := rb.Rebuild(context.Background()); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if rb.idx == nil {
		t.Fatal("rebuilder did not retain created index")
	}
	if !called {
		t.Fatal("onRebuilt callback was not called")
	}
	opened, err := index.Open(indexDir)
	if err != nil {
		t.Fatalf("Open rebuilt index: %v", err)
	}
	if !opened.Contains("ep1") {
		t.Fatal("rebuilt index does not contain ep1")
	}
}

func TestTaskRunnerApprovalEvaluatorIsPerRun(t *testing.T) {
	ad := &taskRunnerAdapter{approvalLayers: []approvals.Layer{approvals.DefaultLayer()}}
	one := ad.newApprovalEvaluator()
	two := ad.newApprovalEvaluator()
	if one == nil || two == nil {
		t.Fatal("expected evaluators")
	}

	one.AddSessionRule(approvals.Rule{
		ToolID:   "file_write",
		Decision: approvals.Allowed,
		Source:   "session: always allowed",
	})

	if dec, _ := one.Evaluate("file_write", ""); dec != approvals.Allowed {
		t.Fatalf("first evaluator session rule not applied, got %s", dec)
	}
	if dec, _ := two.Evaluate("file_write", ""); dec != approvals.Ask {
		t.Fatalf("session approval leaked into another evaluator, got %s", dec)
	}
}

func TestRecordTaskEventsPersistsApprovalAuditDetails(t *testing.T) {
	rec := &recordingAppender{}
	events := []agentloop.Event{
		{
			Type:           agentloop.EvtApprovalNeeded,
			ToolID:         "shell_exec",
			ToolArgs:       `{"command":"git status"}`,
			ApprovalID:     "shell_exec-1-1",
			ApprovalReason: "builtin: shell commands require approval",
		},
		{
			Type:             agentloop.EvtApproval,
			ToolID:           "shell_exec",
			ApprovalID:       "shell_exec-1-1",
			ApprovalReason:   "builtin: shell commands require approval",
			ApprovalDecision: approvals.Allowed.String(),
			ApprovalScope:    approvals.ApprovalScopeAlways,
		},
	}

	recordTaskEvents(rec, "sid", events)
	if len(rec.messages) != 2 {
		t.Fatalf("expected 2 audit messages, got %d", len(rec.messages))
	}

	needed := rec.messages[0]
	if needed.Role != "system" || needed.Name != approvalAuditMessageName {
		t.Fatalf("approval-needed message role/name = %q/%q", needed.Role, needed.Name)
	}
	for _, want := range []string{agentloop.EvtApprovalNeeded, "id=shell_exec-1-1", "tool=shell_exec", "reason=\"builtin: shell commands require approval\"", `args={"command":"git status"}`} {
		if !strings.Contains(needed.Content, want) {
			t.Errorf("approval-needed audit missing %q in %q", want, needed.Content)
		}
	}

	result := rec.messages[1]
	for _, want := range []string{approvalAuditMessageName, "id=shell_exec-1-1", "tool=shell_exec", "decision=" + approvals.Allowed.String(), "scope=" + approvals.ApprovalScopeAlways, "reason=\"builtin: shell commands require approval\""} {
		if !strings.Contains(result.Content, want) {
			t.Errorf("approval result audit missing %q in %q", want, result.Content)
		}
	}
}

type recordingAppender struct {
	messages []inference.Message
}

func (r *recordingAppender) Append(_ string, msg inference.Message) error {
	r.messages = append(r.messages, msg)
	return nil
}

type stubEmbedder struct {
	vec []float32
}

func (s stubEmbedder) Embed(_ context.Context, chunks []string) ([][]float32, error) {
	out := make([][]float32, len(chunks))
	for i := range out {
		out[i] = append([]float32(nil), s.vec...)
	}
	return out, nil
}

func (s stubEmbedder) Health(context.Context) error { return nil }

// newMemoryRepo creates a temp directory populated with files (relative paths
// using forward slashes) and returns a memory.DirReader rooted at it.
func newMemoryRepo(t *testing.T, files map[string]string) *memory.DirReader {
	t.Helper()
	root := t.TempDir()
	for rel, body := range files {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatalf("MkdirAll %s: %v", abs, err)
		}
		if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
			t.Fatalf("WriteFile %s: %v", abs, err)
		}
	}
	return memory.NewDirReader(root)
}

func freeTCPPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen on ephemeral port: %v", err)
	}
	defer func() { _ = ln.Close() }()
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener addr is %T, want *net.TCPAddr", ln.Addr())
	}
	return addr.Port
}
