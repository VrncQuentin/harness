package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/vrnc/harness/internal/agent"
	"github.com/vrnc/harness/internal/agentloop"
	"github.com/vrnc/harness/internal/api"
	"github.com/vrnc/harness/internal/approvals"
	"github.com/vrnc/harness/internal/httpclient"
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
	globalMem      memory.Repo
	activeMem      memory.Repo
	getProjectSlug func() string
	setActive      func(string) error
}

func (ad *uiAgentRegistryAdapter) List() ([]ui.AgentInfo, error) {
	agents, err := ad.projectRegistry().List()
	if err != nil {
		return nil, err
	}
	out := make([]ui.AgentInfo, 0, len(agents))
	for _, a := range agents {
		out = append(out, projectAgentInfo(a))
	}
	return out, nil
}

func (ad *uiAgentRegistryAdapter) Get(name string) (ui.AgentInfo, error) {
	a, err := ad.projectRegistry().Get(name)
	if err != nil {
		return ui.AgentInfo{}, err
	}
	return projectAgentInfo(a), nil
}

func (ad *uiAgentRegistryAdapter) projectRegistry() *agent.ProjectRegistry {
	slug := ""
	if ad.getProjectSlug != nil {
		slug = ad.getProjectSlug()
	}
	return &agent.ProjectRegistry{
		Global:      ad.reg,
		GlobalMem:   ad.globalMem,
		ActiveMem:   ad.activeMem,
		ProjectSlug: slug,
		GlobalSlug:  project.GlobalSlug,
		SetActiveFn: ad.setActive,
	}
}

func projectAgentInfo(a agent.ProjectAgent) ui.AgentInfo {
	return ui.AgentInfo{
		Name:        a.Name,
		PersonaPath: a.Persona.Path,
		Persona:     a.Persona.Content,
		RulesPath:   a.Rules.Path,
		Rules:       a.Rules.Content,
		NotesPath:   a.Notes.Path,
		Notes:       a.Notes.Content,
		Origin:      a.Origin,
	}
}

func (ad *uiAgentRegistryAdapter) Active() string {
	return ad.projectRegistry().Active()
}

func (ad *uiAgentRegistryAdapter) SetActive(name string) error {
	return ad.projectRegistry().SetActive(name)
}

func (ad *uiAgentRegistryAdapter) Create(name string) error {
	_, err := ad.projectRegistry().Create(name)
	return err
}

func (ad *uiAgentRegistryAdapter) WritePersona(name string, body []byte) error {
	return ad.projectRegistry().WritePersona(name, body)
}

func (ad *uiAgentRegistryAdapter) WriteRules(name string, body []byte) error {
	return ad.projectRegistry().WriteRules(name, body)
}

func (ad *uiAgentRegistryAdapter) WriteNotes(name string, body []byte) error {
	return ad.projectRegistry().WriteNotes(name, body)
}

func (ad *uiAgentRegistryAdapter) Delete(name string) error {
	return ad.projectRegistry().Delete(name)
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

func chatMessagesToInference(conversation []ui.ChatMessage) []inference.Message {
	msgs := make([]inference.Message, len(conversation))
	for i, m := range conversation {
		msgs[i] = inference.Message{Role: m.Role, Content: m.Content}
	}
	return msgs
}

// chatRunnerAdapter satisfies ui.ChatRunner against the same assembler +
// queue used by the API server. It exists so the ui package never imports
// inference or queue directly: this file translates between the small
// ChatMessage/ChatToken DTOs and the runtime types.
//
// The adapter mints or accepts a session id per call. The id is returned
// to the UI so the browser can pin it to subsequent stream + save
// requests, and the assistant turn is appended to the live session as
// tokens arrive.
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
	msgs := chatMessagesToInference(conversation)

	// Resolve the active agent up front so the session is bound to the
	// same value the assembler will use.
	resolvedAgent := agentName
	if resolvedAgent == "" {
		resolvedAgent = ad.asm.rt.getActiveAgent()
	}
	if resolvedAgent == "" {
		return "", nil, ui.ErrChatNoAgent
	}

	// Mint or attach to a session id and replay any user-side delta before
	// dispatch so a save on the next click captures the turn even if the user
	// navigates away mid-stream.
	id := attachSessionTurn(ad.mgr, sessionID, resolvedAgent, msgs)

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
		Completion: inference.CompletionRequest{
			Messages: assembled,
		},
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
		flushAssistant := func(logMessage string) {
			appendAssistantContent(ad.mgr, id, assistant, logMessage)
		}
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
				if tok.Err == nil {
					flushAssistant("session: append assistant turn")
				}
				return
			}
		}
		// Channel closed without a Done token - capture whatever was
		// streamed so the save can still confirm partial progress.
		flushAssistant("session: append assistant turn (closed)")
	}()
	return id, out, nil
}

func attachSessionTurn(mgr *session.Manager, sessionID, agentName string, msgs []inference.Message) string {
	id := sessionID
	if mgr == nil {
		return id
	}
	if id == "" || mgr.Snapshot(id) == nil {
		s := mgr.Start(agentName)
		id = s.ID
	}
	appendUserSide(mgr, id, msgs)
	return id
}
func appendAssistantContent(mgr *session.Manager, id, content, logMessage string) {
	if mgr == nil || id == "" || content == "" {
		return
	}
	if err := mgr.Append(id, inference.Message{Role: "assistant", Content: content}); err != nil {
		slog.Warn(logMessage, "id", id, "err", err)
	}
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

// apiSessionAdapter implements api.SessionRecorder so the API server can
// mint a fresh session per /v1/chat/completions request and append the
// user-side messages plus the assistant turn. API requests are recorded
// independently from any client-side conversation lifecycle.
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

func (a *apiSessionAdapter) Save(ctx context.Context, id string) error {
	if a.mgr == nil {
		return errors.New("session: api adapter has no manager")
	}
	_, err := a.mgr.Save(ctx, id)
	return err
}

func (a *apiSessionAdapter) End(id string) {
	if a.mgr == nil {
		return
	}
	a.mgr.End(id)
}

type taskRunnerAdapter struct {
	rt       *Runtime
	registry *tools.Registry
	asm      *apiAssemblerAdapter
	q        *queue.Queue
	// approvalLayers seed a fresh permission evaluator for each task engine.
	// The evaluator session layer is mutable, so sharing an evaluator would leak
	// "always" approvals across task sessions.
	approvalLayers []approvals.Layer
	metrics        agentloop.MetricsRecorder
	// gov is the stateless governor applied to every task engine. Nil means no transforms.
	gov       agentloop.Governor
	enginesMu sync.Mutex
	engines   map[string]*agentloop.Engine // sessionID → engine
	cancels   map[string]context.CancelFunc
	dones     map[string]chan struct{}
}

// queuedInferClient wraps a Queue so the agent loop routes through the
// bounded in-process channel instead of calling the llama-server inference
// client directly.
type queuedInferClient struct {
	q *queue.Queue
}

func (c *queuedInferClient) Complete(ctx context.Context, req inference.CompletionRequest) (<-chan inference.Token, error) {
	ch := make(chan inference.Token, 64)
	if err := c.q.Enqueue(queue.Request{
		Completion: req,
		Response:   ch,
		Ctx:        ctx,
	}); err != nil {
		return nil, err
	}
	return ch, nil
}

func (ad *taskRunnerAdapter) newApprovalEvaluator() *approvals.Evaluator {
	if len(ad.approvalLayers) == 0 {
		return nil
	}
	layers := append([]approvals.Layer(nil), ad.approvalLayers...)
	return approvals.NewEvaluator(layers...)
}

func (ad *taskRunnerAdapter) RunTask(ctx context.Context, agentName string, sessionID string, conversation []ui.ChatMessage) (string, <-chan ui.TaskEvent, error) {
	if agentName == "" {
		agentName = ad.rt.getActiveAgent()
	}
	if agentName == "" {
		return "", nil, ui.ErrTaskNoAgent
	}

	msgs := chatMessagesToInference(conversation)

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

	mgr := ad.rt.SessionManager()
	id := attachSessionTurn(mgr, sessionID, agentName, msgs)

	if ad.q == nil {
		return "", nil, fmt.Errorf("task queue not ready")
	}
	loopClient := &queuedInferClient{q: ad.q}

	// Resolve sandbox roots from the active project's directories.
	var sandboxRoots []string
	ad.rt.mu.Lock()
	slug := ad.rt.cfg.Project.ActiveProjectSlug
	loopCfg := ad.rt.cfg.Loop
	ad.rt.mu.Unlock()
	if slug != "" && ad.rt.projectStore != nil {
		dirs, err := ad.rt.projectStore.ListDirectories(slug)
		if err == nil {
			for _, d := range dirs {
				sandboxRoots = append(sandboxRoots, d.Path)
			}
		}
	}
	// Collect memory repo paths for the C2 scope predicate (git write tools).
	var memoryRepoPaths []string
	if ad.rt.projectStore != nil {
		if projs, err := ad.rt.projectStore.List(true); err == nil {
			for _, p := range projs {
				if p.MemoryRepoPath != "" {
					memoryRepoPaths = append(memoryRepoPaths, p.MemoryRepoPath)
				}
			}
		}
	}
	loopCtx, cancelLoop := context.WithCancel(ctx)

	toolCtx := tools.CallInfo{
		ProjectSlug:     slug,
		SandboxRoots:    sandboxRoots,
		MemoryRepoPaths: memoryRepoPaths,
		SessionID:       id,
		CallerIdentity:  "agent:" + agentName,
		HTTPClient:      httpclient.New(),
	}

	engine := agentloop.NewEngine(loopClient, ad.registry, loopCfg, toolCtx)
	if ad.metrics != nil {
		engine.WithMetrics(ad.metrics)
	}
	if evl := ad.newApprovalEvaluator(); evl != nil {
		engine.WithApprovals(evl)
	}
	if ad.gov != nil {
		engine.WithGovernor(ad.gov)
	}

	// Register engine so approval decisions can be routed back.
	done := make(chan struct{})
	ad.registerEngine(id, engine, cancelLoop, done)

	rawEvch := make(chan agentloop.Event, 64)
	evch := make(chan ui.TaskEvent, 64)
	go func() {
		if err := engine.Run(loopCtx, assembled, rawEvch); err != nil {
			slog.Warn("task engine", "err", err)
		}
	}()
	go func() {
		defer close(evch)
		defer close(done)
		defer ad.unregisterEngine(id, done)
		var events []agentloop.Event
		defer func() {
			if mgr != nil && id != "" {
				recordTaskEvents(mgr, id, events)
			}
		}()
		for ev := range rawEvch {
			events = append(events, ev)
			select {
			case evch <- mapTaskEvent(ev):
			case <-loopCtx.Done():
				return
			}
		}
	}()

	return id, evch, nil
}

func (ad *taskRunnerAdapter) registerEngine(sessionID string, engine *agentloop.Engine, cancel context.CancelFunc, done chan struct{}) {
	var previousCancel context.CancelFunc
	var previousDone chan struct{}

	ad.enginesMu.Lock()
	if ad.engines == nil {
		ad.engines = make(map[string]*agentloop.Engine)
	}
	if ad.cancels == nil {
		ad.cancels = make(map[string]context.CancelFunc)
	}
	if ad.dones == nil {
		ad.dones = make(map[string]chan struct{})
	}
	previousCancel = ad.cancels[sessionID]
	previousDone = ad.dones[sessionID]
	ad.engines[sessionID] = engine
	ad.cancels[sessionID] = cancel
	ad.dones[sessionID] = done
	ad.enginesMu.Unlock()

	if previousCancel != nil {
		previousCancel()
	}
	if previousDone != nil {
		select {
		case <-previousDone:
		case <-time.After(2 * time.Second):
			slog.Warn("task: previous engine did not stop before replacement", "session", sessionID)
		}
	}
}

func (ad *taskRunnerAdapter) unregisterEngine(sessionID string, done chan struct{}) {
	ad.enginesMu.Lock()
	if ad.dones[sessionID] != done {
		ad.enginesMu.Unlock()
		return
	}
	cancel := ad.cancels[sessionID]
	delete(ad.engines, sessionID)
	delete(ad.cancels, sessionID)
	delete(ad.dones, sessionID)
	ad.enginesMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (ad *taskRunnerAdapter) CancelAll(ctx context.Context) error {
	ad.enginesMu.Lock()
	cancels := make([]context.CancelFunc, 0, len(ad.cancels))
	dones := make([]chan struct{}, 0, len(ad.dones))
	for id, cancel := range ad.cancels {
		cancels = append(cancels, cancel)
		if done := ad.dones[id]; done != nil {
			dones = append(dones, done)
		}
	}
	ad.enginesMu.Unlock()

	for _, cancel := range cancels {
		cancel()
	}
	for _, done := range dones {
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (ad *taskRunnerAdapter) CancelTask(sessionID string) error {
	ad.enginesMu.Lock()
	cancel, ok := ad.cancels[sessionID]
	ad.enginesMu.Unlock()
	if !ok || cancel == nil {
		return fmt.Errorf("task: no active engine for session %q", sessionID)
	}
	cancel()
	return nil
}

// ApplyApproval routes a user decision to the correct running engine.
func (ad *taskRunnerAdapter) ApplyApproval(sessionID, approvalID, decision string) error {
	ad.enginesMu.Lock()
	engine, ok := ad.engines[sessionID]
	ad.enginesMu.Unlock()
	if !ok {
		return fmt.Errorf("task: no active engine for session %q", sessionID)
	}
	var resp approvals.ApprovalResponse
	switch decision {
	case "allow":
		resp = approvals.ApprovalResponse{Decision: approvals.Allowed, Remember: false}
	case "reject":
		resp = approvals.ApprovalResponse{Decision: approvals.Denied, Remember: false}
	case "always":
		resp = approvals.ApprovalResponse{Decision: approvals.Allowed, Remember: true}
	default:
		return fmt.Errorf("task: unknown decision %q", decision)
	}
	return engine.ApplyApproval(approvalID, resp)
}

func mapTaskEvent(ev agentloop.Event) ui.TaskEvent {
	return ui.TaskEvent{
		Turn:             ev.Turn,
		Type:             ev.Type,
		Content:          ev.Content,
		ToolID:           ev.ToolID,
		ToolArgs:         ev.ToolArgs,
		ToolResult:       ev.ToolResult,
		ToolError:        ev.ToolError,
		ApprovalID:       ev.ApprovalID,
		ApprovalReason:   ev.ApprovalReason,
		ApprovalDecision: ev.ApprovalDecision,
		ApprovalScope:    ev.ApprovalScope,
		Origin:           ev.Origin,
		Terminate:        ev.Terminate,
	}
}

type taskEventAppender interface {
	Append(id string, msg inference.Message) error
}

const (
	approvalAuditMessageName  = "approval"
	approvalAuditNeededFormat = "[approval_needed #%d] id=%s tool=%s reason=%q args=%s"
	approvalAuditResultFormat = "[approval #%d] id=%s tool=%s decision=%s scope=%s reason=%q"
)

func recordTaskEvents(mgr taskEventAppender, id string, events []agentloop.Event) {
	var assistant strings.Builder
	flushAssistant := func() {
		if assistant.Len() == 0 || mgr == nil || id == "" {
			return
		}
		if err := mgr.Append(id, inference.Message{Role: "assistant", Content: assistant.String()}); err != nil {
			slog.Warn("session: append task assistant turn", "id", id, "err", err)
		}
		assistant.Reset()
	}

	toolSeq := 0
	approvalSeq := 0
	approvalNumbers := make(map[string]int)
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
		case agentloop.EvtApprovalNeeded:
			flushAssistant()
			approvalSeq++
			seq := approvalSeq
			if ev.ApprovalID != "" {
				approvalNumbers[ev.ApprovalID] = seq
			}
			trail := fmt.Sprintf(approvalAuditNeededFormat, seq, ev.ApprovalID, ev.ToolID, ev.ApprovalReason, ev.ToolArgs)
			if err := mgr.Append(id, inference.Message{
				Role:    "system",
				Name:    approvalAuditMessageName,
				Content: trail,
			}); err != nil {
				slog.Warn("session: append approval needed", "id", id, "err", err)
			}
		case agentloop.EvtApproval:
			seq := 0
			if ev.ApprovalID != "" {
				seq = approvalNumbers[ev.ApprovalID]
				delete(approvalNumbers, ev.ApprovalID)
			}
			if seq == 0 {
				approvalSeq++
				seq = approvalSeq
			}
			decision := ev.ApprovalDecision
			if decision == "" {
				decision = approvals.Allowed.String()
				if ev.ToolError == approvals.Denied.String() || ev.ToolError == "denied" {
					decision = approvals.Denied.String()
				}
			}
			scope := ev.ApprovalScope
			if scope == "" {
				scope = approvals.ApprovalScopeOnce
			}
			trail := fmt.Sprintf(approvalAuditResultFormat, seq, ev.ApprovalID, ev.ToolID, decision, scope, ev.ApprovalReason)
			if err := mgr.Append(id, inference.Message{
				Role:    "system",
				Name:    approvalAuditMessageName,
				Content: trail,
			}); err != nil {
				slog.Warn("session: append approval result", "id", id, "err", err)
			}
		}
	}
	flushAssistant()
}
