package runtime

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/vrnc/harness/internal/agent"
	"github.com/vrnc/harness/internal/inference"
	"github.com/vrnc/harness/internal/memory"
	"github.com/vrnc/harness/internal/prompt"
	"github.com/vrnc/harness/internal/queue"
	"github.com/vrnc/harness/internal/reqid"
	"github.com/vrnc/harness/internal/ui"
)

type uiAgentRegistryAdapter struct {
	reg agent.Registry
	mem memory.Reader
}

func (ad *uiAgentRegistryAdapter) List() ([]ui.AgentInfo, error) {
	agents, err := ad.reg.List()
	if err != nil {
		return nil, err
	}
	out := make([]ui.AgentInfo, 0, len(agents))
	for _, a := range agents {
		info, err := ad.Get(a.Name)
		if err != nil {
			continue
		}
		out = append(out, info)
	}
	return out, nil
}

func (ad *uiAgentRegistryAdapter) Get(name string) (ui.AgentInfo, error) {
	a, err := ad.reg.Get(name)
	if err != nil {
		return ui.AgentInfo{}, err
	}
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
type chatRunnerAdapter struct {
	asm *apiAssemblerAdapter
	q   *queue.Queue
}

// Run assembles the prompt, enqueues a request, and returns a channel of
// translated tokens. Errors before dispatch are returned synchronously
// and mapped to the ui sentinel set so the handler can pick the right
// HTTP status without inspecting queue/prompt error types.
func (ad *chatRunnerAdapter) Run(ctx context.Context, agentName string, conversation []ui.ChatMessage) (<-chan ui.ChatToken, error) {
	msgs := make([]inference.Message, len(conversation))
	for i, m := range conversation {
		msgs[i] = inference.Message{Role: m.Role, Content: m.Content}
	}

	reqID := fmt.Sprintf("uichat-%d", time.Now().UnixNano())
	ctx = reqid.WithID(ctx, reqID)

	assembled, err := ad.asm.Assemble(ctx, agentName, msgs)
	if err != nil {
		if errors.Is(err, errNoActiveAgent) {
			return nil, ui.ErrChatNoAgent
		}
		return nil, err
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
			return nil, ui.ErrChatQueueFull
		case errors.Is(err, queue.ErrStopped), errors.Is(err, queue.ErrNoClient):
			return nil, ui.ErrChatUnavailable
		}
		return nil, err
	}

	out := make(chan ui.ChatToken, 64)
	go func() {
		defer close(out)
		for tok := range respCh {
			select {
			case out <- ui.ChatToken{Content: tok.Content, Done: tok.Done, Err: tok.Err}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}
