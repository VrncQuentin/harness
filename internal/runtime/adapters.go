package runtime

import (
	"context"
	"errors"
	"io/fs"

	"github.com/vrnc/harness/internal/agent"
	"github.com/vrnc/harness/internal/inference"
	"github.com/vrnc/harness/internal/memory"
	"github.com/vrnc/harness/internal/prompt"
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
		info := ui.AgentInfo{
			Name:        a.Name,
			PersonaPath: a.PersonaPath,
			RulesPath:   a.RulesPath,
			NotesPath:   a.NotesPath,
		}
		if persona, err := readOptional(ad.mem, a.PersonaPath); err == nil {
			info.Persona = persona
		}
		if rules, err := readOptional(ad.mem, a.RulesPath); err == nil {
			info.Rules = rules
		}
		if notes, err := readOptional(ad.mem, a.NotesPath); err == nil {
			info.Notes = notes
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
		agentName = ad.rt.getActive()
	}
	if agentName == "" {
		return nil, errNoActiveAgent
	}
	msgs, _, err := ad.a.Assemble(ctx, agentName, conversation)
	return msgs, err
}
