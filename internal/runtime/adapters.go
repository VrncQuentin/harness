package runtime

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/vrnc/harness/internal/agent"
	"github.com/vrnc/harness/internal/agentloop"
	"github.com/vrnc/harness/internal/api"
	"github.com/vrnc/harness/internal/approvals"
	"github.com/vrnc/harness/internal/inference"
	"github.com/vrnc/harness/internal/memory"
	"github.com/vrnc/harness/internal/project"
	"github.com/vrnc/harness/internal/queue"
	"github.com/vrnc/harness/internal/reqid"
	"github.com/vrnc/harness/internal/session"
	"github.com/vrnc/harness/internal/tools"
	"github.com/vrnc/harness/internal/ui"
)

type uiAgentRegistryAdapter struct {
	reg            agent.Registry
	mem            memory.Reader
	getProjectSlug func() string
}

func (ad *uiAgentRegistryAdapter) List() ([]ui.AgentInfo, error) {
	agents, err := ad.reg.List()
	if err != nil {
		return nil, err
	}

	slug := ""
	if ad.getProjectSlug != nil {
		slug = ad.getProjectSlug()
	}

	projectAgents := make(map[string]bool)
	if slug != "" {
		dirPath := fmt.Sprintf("projects/%s/agents", slug)
		if dl, ok := ad.mem.(memory.DirLister); ok {
			names, err := dl.ListDirs(dirPath)
			if err == nil {
				for _, name := range names {
					if name != "" && name != "." && name != ".." {
						projectAgents[name] = true
					}
				}
			}
		}
	}

	out := make([]ui.AgentInfo, 0, len(agents)+len(projectAgents))
	seen := make(map[string]bool)
	for _, a := range agents {
		info, err := ad.Get(a.Name)
		if err != nil {
			continue
		}
		if projectAgents[a.Name] {
			info.Origin = "extends-global"
		} else {
			info.Origin = "global"
		}
		out = append(out, info)
		seen[a.Name] = true
	}

	// Add project-only agents that have no global counterpart.
	for name := range projectAgents {
		if seen[name] {
			continue
		}
		projectPath := fmt.Sprintf("projects/%s/agents/%s", slug, name)
		persona, _ := readOptional(ad.mem, projectPath+"/persona.md")
		rules, _ := readOptional(ad.mem, projectPath+"/rules.md")
		notes, _ := readOptional(ad.mem, projectPath+"/notes.md")
		out = append(out, ui.AgentInfo{
			Name:        name,
			PersonaPath: projectPath + "/persona.md",
			Persona:     persona,
			RulesPath:   projectPath + "/rules.md",
			Rules:       rules,
			NotesPath:   projectPath + "/notes.md",
			Notes:       notes,
			Origin:      "project-only",
		})
	}
	return out, nil
}

func (ad *uiAgentRegistryAdapter) Get(name string) (ui.AgentInfo, error) {
	a, err := ad.reg.Get(name)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return ui.AgentInfo{}, err
		}
		// Agent might be project-only; try the project path.
		info, projErr := ad.getProjectAgent(name)
		if projErr != nil {
			return ui.AgentInfo{}, err // return original global lookup error
		}
		return info, nil
	}
	info, err := ad.buildAgentInfo(a)
	if err != nil {
		return ui.AgentInfo{}, err
	}
	// Check if this agent is extended by the active project.
	slug := ""
	if ad.getProjectSlug != nil {
		slug = ad.getProjectSlug()
	}
	if slug != "" {
		projectPersonaPath := fmt.Sprintf("projects/%s/agents/%s/persona.md", slug, a.Name)
		if _, err := ad.mem.Read(projectPersonaPath); err == nil {
			info.Origin = "extends-global"
			return info, nil
		}
	}
	info.Origin = "global"
	return info, nil
}

func (ad *uiAgentRegistryAdapter) getProjectAgent(name string) (ui.AgentInfo, error) {
	slug := ""
	if ad.getProjectSlug != nil {
		slug = ad.getProjectSlug()
	}
	if slug == "" {
		return ui.AgentInfo{}, fmt.Errorf("no active project")
	}
	projectPath := fmt.Sprintf("projects/%s/agents/%s", slug, name)
	persona, _ := readOptional(ad.mem, projectPath+"/persona.md")
	rules, _ := readOptional(ad.mem, projectPath+"/rules.md")
	notes, _ := readOptional(ad.mem, projectPath+"/notes.md")
	if persona == "" && rules == "" && notes == "" {
		return ui.AgentInfo{}, fmt.Errorf("agent %q not found", name)
	}
	return ui.AgentInfo{
		Name:        name,
		PersonaPath: projectPath + "/persona.md",
		Persona:     persona,
		RulesPath:   projectPath + "/rules.md",
		Rules:       rules,
		NotesPath:   projectPath + "/notes.md",
		Notes:       notes,
		Origin:      "project-only",
	}, nil
}

func (ad *uiAgentRegistryAdapter) buildAgentInfo(a agent.Agent) (ui.AgentInfo, error) {
	info := ui.AgentInfo{
		Name:        a.Name,
		PersonaPath: a.PersonaPath,
		RulesPath:   a.RulesPath,
		NotesPath:   a.NotesPath,
	}
	persona, err := readOptional(ad.mem, a.PersonaPath)
	if err != nil {
		return info, err
	}
	info.Persona = persona
	rules, err := readOptional(ad.mem, a.RulesPath)
	if err != nil {
		return info, err
	}
	info.Rules = rules
	notes, err := readOptional(ad.mem, a.NotesPath)
	if err != nil {
		return info, err
	}
	info.Notes = notes
	return info, nil
}

func readOptional(mem memory.Reader, relPath string) (string, error) {
	b, err := mem.Read(relPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	return string(b), nil
}

func (ad *uiAgentRegistryAdapter) Active() string {
	return ad.reg.Active()
}

func (ad *uiAgentRegistryAdapter) SetActive(name string) error {
	return ad.reg.SetActive(name)
}

func (ad *uiAgentRegistryAdapter) Create(name string) error {
	_, err := ad.reg.Create(name)
	return err
}

func (ad *uiAgentRegistryAdapter) WritePersona(name string, body []byte) error {
	return ad.reg.WritePersona(name, body)
}

func (ad *uiAgentRegistryAdapter) WriteRules(name string, body []byte) error {
	return ad.reg.WriteRules(name, body)
}

func (ad *uiAgentRegistryAdapter) WriteNotes(name string, body []byte) error {
	return ad.reg.WriteNotes(name, body)
}

func (ad *uiAgentRegistryAdapter) Delete(name string) error {
	return ad.reg.Delete(name)
}

var errNoActiveAgent = errors.New("api: no agent specified and no active agent configured (set one in /agents)")

type apiAssemblerAdapter struct {
	rt *Runtime
}

func (ad *apiAssemblerAdapter) Assemble(ctx context.Context, agentName string, conversation []inference.Message) ([]inference.Message, error) {
	if agentName == "" {
		agentName = ad.rt.getActiveAgent()
	}
	if agentName == "" {
		return nil, errNoActiveAgent
	}
	asm := ad.rt.getAssembler()
	if asm == nil {
		return nil, errors.New("api: prompt assembler unavailable")
	}
	msgs, _, err := asm.Assemble(ctx, agentName, conversation)
	return msgs, err
}

// chatRunnerAdapter satisfies ui.ChatRunner against the same assembler +
// queue used by the API server. It exists so the ui package never imports
// inference or queue directly: this file translates between the small
// ChatMessage/ChatToken DTOs and the runtime types.
//
// M3 extends the adapter to mint or accept a session id per call. The
// id is returned to the UI so the browser can pin it to subsequent
// stream + save requests, and the assistant turn is appended to the
// live session as tokens arrive.
type chatRunnerAdapter struct {
	asm *apiAssemblerAdapter
	q   *queue.Queue
	mgr *session.Manager
}

// Run assembles the prompt, enqueues a request, and returns a channel of
// translated tokens plus the session id (newly minted on first turn,
// echoed back on subsequent turns). Errors before dispatch are returned
// synchronously and mapped to the ui sentinel set so the handler can
// pick the right HTTP status without inspecting queue/prompt error
// types.
func (ad *chatRunnerAdapter) Run(ctx context.Context, agentName, sessionID string, conversation []ui.ChatMessage) (string, <-chan ui.ChatToken, error) {
	msgs := make([]inference.Message, len(conversation))
	for i, m := range conversation {
		msgs[i] = inference.Message{Role: m.Role, Content: m.Content}
	}

	// Resolve the active agent up front so the session is bound to the
	// same value the assembler will use.
	resolvedAgent := agentName
	if resolvedAgent == "" {
		resolvedAgent = ad.asm.rt.getActiveAgent()
	}
	if resolvedAgent == "" {
		return "", nil, ui.ErrChatNoAgent
	}

	// Mint or attach to a session id so the manager has somewhere to
	// stash the assistant turn. We append the user-side conversation
	// (everything but the placeholder assistant we are about to fill in)
	// before dispatch so a save on the next click captures it even if
	// the user navigates away mid-stream.
	id := sessionID
	if ad.mgr != nil {
		if id == "" {
			s := ad.mgr.Start(resolvedAgent)
			id = s.ID
		} else if ad.mgr.Snapshot(id) == nil {
			// The browser pinned an id we don't know - either it was a
			// resume that never landed in this process, or a stale
			// reference. Either way, mint a fresh session bound to the
			// active agent and replay onto it so the conversation is
			// not silently discarded.
			s := ad.mgr.Start(resolvedAgent)
			id = s.ID
		}
		appendUserSide(ad.mgr, id, msgs)
	}

	reqID := fmt.Sprintf("uichat-%d", time.Now().UnixNano())
	ctx = reqid.WithID(ctx, reqID)

	assembled, err := ad.asm.Assemble(ctx, resolvedAgent, msgs)
	if err != nil {
		if errors.Is(err, errNoActiveAgent) {
			return "", nil, ui.ErrChatNoAgent
		}
		return "", nil, err
	}

	respCh := make(chan inference.Token, 64)
	if err := ad.q.Enqueue(queue.Request{
		ID:       reqID,
		Messages: assembled,
		Response: respCh,
		Ctx:      ctx,
	}); err != nil {
		switch {
		case errors.Is(err, queue.ErrQueueFull):
			return "", nil, ui.ErrChatQueueFull
		case errors.Is(err, queue.ErrStopped), errors.Is(err, queue.ErrNoClient):
			return "", nil, ui.ErrChatUnavailable
		}
		return "", nil, err
	}

	out := make(chan ui.ChatToken, 64)
	go func() {
		defer close(out)
		var assistant string
		for tok := range respCh {
			if tok.Content != "" {
				assistant += tok.Content
			}
			select {
			case out <- ui.ChatToken{Content: tok.Content, Done: tok.Done, Err: tok.Err}:
			case <-ctx.Done():
				return
			}
			if tok.Done || tok.Err != nil {
				if ad.mgr != nil && id != "" && tok.Err == nil && assistant != "" {
					if err := ad.mgr.Append(id, inference.Message{
						Role:    "assistant",
						Content: assistant,
					}); err != nil {
						slog.Warn("session: append assistant turn", "id", id, "err", err)
					}
				}
				return
			}
		}
		// Channel closed without a Done token - capture whatever was
		// streamed so the save can still confirm partial progress.
		if ad.mgr != nil && id != "" && assistant != "" {
			if err := ad.mgr.Append(id, inference.Message{
				Role:    "assistant",
				Content: assistant,
			}); err != nil {
				slog.Warn("session: append assistant turn (closed)", "id", id, "err", err)
			}
		}
	}()
	return id, out, nil
}

// appendUserSide appends the user-side delta of the conversation onto
// the live session. The browser POSTs the entire transcript each turn
// (it is stateless on the wire), so we walk the sequence and only
// append messages newer than the manager's current count for that
// session.
func appendUserSide(mgr *session.Manager, id string, conversation []inference.Message) {
	snap := mgr.Snapshot(id)
	if snap == nil {
		return
	}
	if len(conversation) <= len(snap.Conversation) {
		return
	}
	for _, m := range conversation[len(snap.Conversation):] {
		if err := mgr.Append(id, m); err != nil {
			slog.Warn("session: append user-side", "id", id, "err", err)
			return
		}
	}
}

// uiSessionStoreAdapter implements ui.SessionStore against the live
// session manager. It hides the manager type from the ui package so
// the import graph stays one-way.
type uiSessionStoreAdapter struct {
	mgr       *session.Manager
	getActive func() string
}

// Save persists the live session and returns the result.
func (ad *uiSessionStoreAdapter) Save(ctx context.Context, id string) (ui.SessionSaveResult, error) {
	if ad.mgr == nil {
		return ui.SessionSaveResult{}, ui.ErrSessionUnavailable
	}
	res, err := ad.mgr.Save(ctx, id)
	if err != nil {
		return ui.SessionSaveResult{}, err
	}
	return ui.SessionSaveResult{
		ID:          res.ID,
		EpisodePath: res.EpisodePath,
		Summary:     res.Summary,
		SavedAt:     res.SavedAt,
		SaveSeq:     res.SaveSeq,
	}, nil
}

// Records returns the most recent saved sessions for agent.
func (ad *uiSessionStoreAdapter) Records(agent string) ([]ui.SessionRecord, error) {
	if ad.mgr == nil {
		return nil, nil
	}
	if agent == "" {
		agent = ad.getActive()
	}
	if agent == "" {
		return nil, nil
	}
	recs, err := ad.mgr.Records(agent)
	if err != nil {
		return nil, err
	}
	out := make([]ui.SessionRecord, 0, len(recs))
	for _, r := range recs {
		out = append(out, ui.SessionRecord{
			ID:          r.ID,
			Agent:       r.Agent,
			StartedAt:   r.StartedAt,
			SavedAt:     r.SavedAt,
			SaveSeq:     r.SaveSeq,
			EpisodePath: r.EpisodePath,
		})
	}
	return out, nil
}

// Conversation hydrates the .json sidecar for the given record. Returns
// ErrSessionConversationLost when the sidecar is missing so the UI can
// disable the resume row instead of crashing.
func (ad *uiSessionStoreAdapter) Conversation(agent, id string) ([]ui.ChatMessage, error) {
	if ad.mgr == nil {
		return nil, ui.ErrSessionUnavailable
	}
	msgs, err := ad.mgr.SidecarConversation(agent, id)
	if err != nil {
		if errors.Is(err, session.ErrConversationLost) {
			return nil, ui.ErrSessionConversationLost
		}
		return nil, err
	}
	out := make([]ui.ChatMessage, len(msgs))
	for i, m := range msgs {
		out[i] = ui.ChatMessage{Role: m.Role, Content: m.Content}
	}
	return out, nil
}

func (ad *uiSessionStoreAdapter) LiveConversation(id string) ([]ui.ChatMessage, error) {
	if ad.mgr == nil {
		return nil, ui.ErrSessionUnavailable
	}
	snap := ad.mgr.Snapshot(id)
	if snap == nil {
		return nil, ui.ErrSessionUnknown
	}
	out := make([]ui.ChatMessage, 0, len(snap.Conversation))
	for _, m := range snap.Conversation {
		if m.Role == "" || m.Content == "" {
			continue
		}
		out = append(out, ui.ChatMessage{Role: m.Role, Content: m.Content})
	}
	return out, nil
}

// Resume registers the session id with the manager so subsequent
// streams append onto the resumed conversation.
func (ad *uiSessionStoreAdapter) Resume(id string) error {
	if ad.mgr == nil {
		return ui.ErrSessionUnavailable
	}
	if _, err := ad.mgr.Resume(id); err != nil {
		if errors.Is(err, session.ErrConversationLost) {
			return ui.ErrSessionConversationLost
		}
		if errors.Is(err, session.ErrUnknownSession) {
			return ui.ErrSessionUnknown
		}
		return err
	}
	return nil
}

// apiSessionAdapter implements api.SessionRecorder so the API server
// can mint a fresh session per /v1/chat/completions request and append
// the user-side messages plus the assistant turn. Designed M3-minimal
// so an external client's per-call episodes are recorded without coupling
// to the client's own session lifecycle (M4 will replace this with a
// smarter mapping).
type apiSessionAdapter struct {
	mgr *session.Manager
}

func (a *apiSessionAdapter) Start(agentName string) api.Session {
	if a.mgr == nil {
		return api.Session{}
	}
	s := a.mgr.Start(agentName)
	return api.Session{ID: s.ID, Agent: s.Agent}
}

func (a *apiSessionAdapter) Append(id, role, content string) error {
	if a.mgr == nil {
		return errors.New("session: api adapter has no manager")
	}
	return a.mgr.Append(id, inference.Message{Role: role, Content: content})
}

type taskRunnerAdapter struct {
	rt       *Runtime
	registry *tools.Registry
	asm      *apiAssemblerAdapter
	q        *queue.Queue
	// evl is the M7 permission evaluator. When nil, no approval checks are
	// performed and all enabled tools dispatch immediately.
	evl        *approvals.Evaluator
	enginesMu  sync.Mutex
	engines    map[string]*agentloop.Engine // sessionID → engine
}

// queuedInferClient wraps a Queue so the agent loop routes through the
// bounded channel with backpressure + WAL instead of calling the
// llama-server inference client directly.
type queuedInferClient struct {
	q   *queue.Queue
	raw inference.Client // fallback for Health()
}

func (c *queuedInferClient) Complete(ctx context.Context, req inference.CompletionRequest) (<-chan inference.Token, error) {
	ch := make(chan inference.Token, 64)
	if err := c.q.Enqueue(queue.Request{
		ID:          fmt.Sprintf("task-%d", time.Now().UnixNano()),
		Model:       req.Model,
		Messages:    req.Messages,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		MaxTokens:   req.MaxTokens,
		Tools:       req.Tools,
		ToolChoice:  req.ToolChoice,
		Response:    ch,
		Ctx:         ctx,
	}); err != nil {
		return nil, err
	}
	return ch, nil
}

func (c *queuedInferClient) Health(ctx context.Context) error {
	if c.raw != nil {
		return c.raw.Health(ctx)
	}
	return nil
}

func (ad *taskRunnerAdapter) RunTask(ctx context.Context, agentName string, sessionID string, conversation []ui.ChatMessage) (string, <-chan agentloop.Event, error) {
	if agentName == "" {
		agentName = ad.rt.getActiveAgent()
	}
	if agentName == "" {
		return "", nil, ui.ErrTaskNoAgent
	}

	msgs := make([]inference.Message, len(conversation))
	for i, m := range conversation {
		msgs[i] = inference.Message{Role: m.Role, Content: m.Content}
	}

	// Route through the Prompt Assembler so the model receives rules,
	// persona, memory, and episodes — not just raw conversation.
	var assembled []inference.Message
	if ad.asm != nil {
		var err error
		assembled, err = ad.asm.Assemble(ctx, agentName, msgs)
		if err != nil {
			return "", nil, fmt.Errorf("task: assemble: %w", err)
		}
	} else {
		assembled = msgs
	}

	// Mint or attach to a session.
	mgr := ad.rt.SessionManager()
	id := sessionID
	if mgr != nil {
		if id == "" {
			s := mgr.Start(agentName)
			id = s.ID
		} else if mgr.Snapshot(id) == nil {
			s := mgr.Start(agentName)
			id = s.ID
		}
		if len(msgs) == 1 {
			if err := mgr.Append(id, msgs[0]); err != nil {
				slog.Warn("session: append task user turn", "id", id, "err", err)
			}
		} else {
			appendUserSide(mgr, id, msgs)
		}
	}

	inferClient := ad.rt.getInferClient()
	if inferClient == nil && ad.q == nil {
		return "", nil, fmt.Errorf("inference client not ready")
	}

	// When a Queue is wired, route through it for backpressure + WAL.
	// Otherwise fall back to direct inference (useful for testing).
	var loopClient inference.Client
	if ad.q != nil {
		loopClient = &queuedInferClient{q: ad.q, raw: inferClient}
	} else {
		loopClient = inferClient
	}

	// Resolve sandbox roots from the active project's directories.
	var sandboxRoots []string
	ad.rt.mu.Lock()
	slug := ad.rt.cfg.Project.ActiveProjectSlug
	loopCfg := ad.rt.cfg.Loop
	ad.rt.mu.Unlock()
	if slug != "" && ad.rt.projectStore != nil {
		if fs, ok := ad.rt.projectStore.(project.Store); ok {
			dirs, err := fs.ListDirectories(slug)
			if err == nil {
				for _, d := range dirs {
					sandboxRoots = append(sandboxRoots, d.Path)
				}
			}
		}
	}
	if len(sandboxRoots) == 0 {
		ad.rt.mu.Lock()
		repoPath := ad.rt.cfg.Memory.RepoPath
		ad.rt.mu.Unlock()
		if repoPath != "" {
			sandboxRoots = append(sandboxRoots, repoPath)
		}
	}

	toolCtx := tools.Context{
		ProjectSlug:    slug,
		SandboxRoots:   sandboxRoots,
		SessionID:      id,
		CallerIdentity: "agent:" + agentName,
		Ctx:            ctx,
	}

	engine := agentloop.NewEngine(loopClient, ad.registry, loopCfg, toolCtx)
	if ad.evl != nil {
		engine.WithApprovals(ad.evl)
	}

	// Register engine so approval decisions can be routed back.
	ad.registerEngine(id, engine)

	rawEvch := make(chan agentloop.Event, 64)
	evch := make(chan agentloop.Event, 64)
	go func() {
		if err := engine.Run(ctx, assembled, rawEvch); err != nil {
			slog.Warn("task engine", "err", err)
		}
	}()
	go func() {
		defer close(evch)
		defer ad.unregisterEngine(id)
		var events []agentloop.Event
		for ev := range rawEvch {
			events = append(events, ev)
			select {
			case evch <- ev:
			case <-ctx.Done():
				return
			}
		}
		if mgr != nil && id != "" {
			recordTaskEvents(mgr, id, events)
		}
	}()

	return id, evch, nil
}

func (ad *taskRunnerAdapter) registerEngine(sessionID string, engine *agentloop.Engine) {
	ad.enginesMu.Lock()
	if ad.engines == nil {
		ad.engines = make(map[string]*agentloop.Engine)
	}
	ad.engines[sessionID] = engine
	ad.enginesMu.Unlock()
}

func (ad *taskRunnerAdapter) unregisterEngine(sessionID string) {
	ad.enginesMu.Lock()
	delete(ad.engines, sessionID)
	ad.enginesMu.Unlock()
}

// ApplyApproval routes a user decision to the correct running engine.
func (ad *taskRunnerAdapter) ApplyApproval(sessionID, approvalID, decision string) error {
	ad.enginesMu.Lock()
	engine, ok := ad.engines[sessionID]
	ad.enginesMu.Unlock()
	if !ok {
		return fmt.Errorf("task: no active engine for session %q", sessionID)
	}
	var d approvals.Decision
	switch decision {
	case "allow":
		d = approvals.Allowed
	case "reject":
		d = approvals.Denied
	default:
		return fmt.Errorf("task: unknown decision %q", decision)
	}
	return engine.ApplyApproval(approvalID, d)
}

func recordTaskEvents(mgr *session.Manager, id string, events []agentloop.Event) {
	var assistant strings.Builder
	flushAssistant := func() {
		if assistant.Len() == 0 {
			return
		}
		if err := mgr.Append(id, inference.Message{Role: "assistant", Content: assistant.String()}); err != nil {
			slog.Warn("session: append task assistant turn", "id", id, "err", err)
		}
		assistant.Reset()
	}

	toolSeq := 0
	for _, ev := range events {
		switch ev.Type {
		case agentloop.EvtText:
			assistant.WriteString(ev.Content)
		case agentloop.EvtToolCall:
			flushAssistant()
			toolSeq++
			if err := mgr.Append(id, inference.Message{
				Role: "assistant",
				ToolCalls: []inference.ToolCall{{
					ID:   fmt.Sprintf("task_%d", toolSeq),
					Type: "function",
					Function: inference.ToolCallFunction{
						Name:      ev.ToolID,
						Arguments: ev.ToolArgs,
					},
				}},
			}); err != nil {
				slog.Warn("session: append task tool call", "id", id, "err", err)
			}
		case agentloop.EvtToolResult:
			content := ev.ToolResult
			if ev.ToolError != "" {
				content = "ERROR: " + ev.ToolError
			}
			if err := mgr.Append(id, inference.Message{
				Role:       "tool",
				ToolCallID: fmt.Sprintf("task_%d", toolSeq),
				Name:       ev.ToolID,
				Content:    content,
			}); err != nil {
				slog.Warn("session: append task tool result", "id", id, "err", err)
			}
		}
	}
	flushAssistant()
}

func (rt *Runtime) getInferClient() inference.Client {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.inferClient
}
