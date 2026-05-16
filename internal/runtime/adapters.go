package runtime

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"time"

	"github.com/vrnc/harness/internal/agent"
	"github.com/vrnc/harness/internal/api"
	"github.com/vrnc/harness/internal/inference"
	"github.com/vrnc/harness/internal/memory"
	"github.com/vrnc/harness/internal/prompt"
	"github.com/vrnc/harness/internal/queue"
	"github.com/vrnc/harness/internal/reqid"
	"github.com/vrnc/harness/internal/session"
	"github.com/vrnc/harness/internal/ui"
)

type uiAgentRegistryAdapter struct {
	reg             agent.Registry
	mem             memory.Reader
	getProjectSlug  func() string
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
	a  *prompt.DiskAssembler
	rt *Runtime
}

func (ad *apiAssemblerAdapter) Assemble(ctx context.Context, agentName string, conversation []inference.Message) ([]inference.Message, error) {
	if agentName == "" {
		agentName = ad.rt.getActiveAgent()
	}
	if agentName == "" {
		return nil, errNoActiveAgent
	}
	msgs, _, err := ad.a.Assemble(ctx, agentName, conversation)
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
