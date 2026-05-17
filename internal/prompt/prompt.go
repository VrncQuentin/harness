// Package prompt assembles the layered system prompt sent to the model.
// Layers are stacked in a fixed order and budgeted against the memory
// token limit and the conversation reserve; trimming is oldest-episode
// first, and fixed prompt layers are never dropped.
//
// Chat template formatting is the job of llama-server's
// apply_chat_template endpoint, not this package - we return a
// []inference.Message ready for the /v1/chat/completions body.
package prompt

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/vrnc/harness/internal/agent"
	"github.com/vrnc/harness/internal/config"
	"github.com/vrnc/harness/internal/embedder"
	"github.com/vrnc/harness/internal/index"
	"github.com/vrnc/harness/internal/inference"
	"github.com/vrnc/harness/internal/memory"
	"github.com/vrnc/harness/internal/project"
	"github.com/vrnc/harness/internal/reqid"
)

// ErrAgentRequired is returned by Assemble when called with an empty
// agent name. The persona layer is mandatory, so an empty agent means
// either the user has not selected one yet or the API caller forgot to
// set agent / X-Harness-Agent. Surface as an error rather than silently
// dropping the persona section.
var ErrAgentRequired = errors.New("prompt: agent name is required")

// Well-known paths inside the memory repo. Kept as package-level
// constants so tests and hot-reload share the same strings.
const (
	rulesPath = "global/rules.md"
	userPath  = "global/user.md"
	factsPath = "global/facts.md"

	// projectRulesPath points at the optional active-project rules layer.
	// The global project's file is optional too: it is not seeded, but is
	// honored when the user authors it.
	projectRulesPath = "projects/%s/rules.md"

	// episodesGlobPattern is the format template for globbing episode
	// files. The first parameter is the project slug, the second is the
	// agent name.
	episodesGlobPattern = "projects/%s/episodes/%s/*.md"
)

// Static markdown section headers. Each optional layer is skipped if
// its file is missing or empty, so these are only written when the
// layer contributes content.
const (
	rulesHeader        = "# Rules"
	userHeader         = "# User"
	projectRulesHeader = "# Project Rules"
	personaHeader      = "# Persona"
	agentRulesHeader   = "# Agent Rules"
	factsHeader        = "# Facts"
	notesHeader        = "# Notes"
	episodesHeader     = "# Episodes"
)

// LayerStats reports token counts per layer and the overall total for
// the assembled output. It is logged and exposed to the UI logs page
// so the operator can see exactly what went into the prompt.
type LayerStats struct {
	Rules        int
	User         int
	ProjectRules int
	Persona      int
	AgentRules   int
	Facts        int
	Notes        int
	Episodes     int
	Conversation int
	Total        int
}

// Assembler builds a complete message slice for a given agent and
// conversation. Implementations must be safe for concurrent use.
type Assembler interface {
	Assemble(ctx context.Context, agentName string, conversation []inference.Message) ([]inference.Message, LayerStats, error)
}

// DiskAssembler reads layer files from a memory.Reader and assembles
// them against a PromptConfig. It holds no mutable state - the active
// agent is passed per call so switching agents is a constant-time
// operation. Safe for concurrent use: Assemble only reads from fields
// set once at construction.
type DiskAssembler struct {
	mem         memory.Reader
	reg         agent.Registry
	cfg         config.PromptConfig
	projectSlug string
	logger      *slog.Logger
	tokenizer   func(string) int
	idx         *index.Index
	emb         embedder.Client
}

var _ Assembler = (*DiskAssembler)(nil)

// NewDiskAssembler returns an assembler that reads from mem and
// resolves agents through reg. The prompt config controls the memory
// token budget and conversation reserve.
func NewDiskAssembler(mem memory.Reader, reg agent.Registry, cfg config.PromptConfig) *DiskAssembler {
	return &DiskAssembler{
		mem:       mem,
		reg:       reg,
		cfg:       cfg,
		logger:    slog.Default(),
		tokenizer: defaultTokenize,
	}
}

// WithLogger returns a shallow copy with a custom slog.Logger. Passing
// nil swaps in slog.Default so callers don't have to nil-check.
func (a *DiskAssembler) WithLogger(l *slog.Logger) *DiskAssembler {
	cp := *a
	if l == nil {
		l = slog.Default()
	}
	cp.logger = l
	return &cp
}

// WithTokenizer returns a shallow copy with a custom token counter.
// Passing nil restores the rune-quarter heuristic.
func (a *DiskAssembler) WithTokenizer(f func(string) int) *DiskAssembler {
	cp := *a
	if f == nil {
		f = defaultTokenize
	}
	cp.tokenizer = f
	return &cp
}

// WithProjectSlug returns a shallow copy with the active project slug.
// The assembler resolves an empty slug to the global project at load time.
func (a *DiskAssembler) WithProjectSlug(slug string) *DiskAssembler {
	cp := *a
	cp.projectSlug = slug
	return &cp
}

// WithBlendedRetrieval returns a shallow copy with semantic retrieval
// support. When idx and emb are non-nil, loadEpisodes uses the last user
// message as a query for ANN search, blending similarity with recency.
func (a *DiskAssembler) WithBlendedRetrieval(idx *index.Index, emb embedder.Client) *DiskAssembler {
	cp := *a
	cp.idx = idx
	cp.emb = emb
	return &cp
}

// defaultTokenize is the rune-quarter heuristic. Fast, deterministic,
// and close enough for budgeting against caps that already carry
// generous headroom.
func defaultTokenize(s string) int {
	return (utf8.RuneCountInString(s) + 3) / 4
}

// EstimateTokens returns the rune-quarter token estimate for s. It is
// the same heuristic the assembler uses internally, exposed so other
// callers (the UI memory page) can show consistent numbers without
// pulling in a real tokenizer.
func EstimateTokens(s string) int {
	return defaultTokenize(s)
}

// Assemble builds the final message slice for agentName given
// conversation. It returns the system prompt prepended to the
// conversation, the per-layer token counts, and any error that halts
// assembly (missing required layer). Missing optional layers produce
// empty sections rather than errors.
//
// agentName is required: the persona layer is mandatory, so callers
// must resolve the active agent before invoking. ErrAgentRequired is
// returned when it is empty.
//
// Log entries are tagged with the request id from ctx (see
// internal/reqid) so the api handler, queue dispatcher, and assembler
// share a correlation key in the logs.
func (a *DiskAssembler) Assemble(ctx context.Context, agentName string, conversation []inference.Message) ([]inference.Message, LayerStats, error) {
	logger := a.loggerFor(ctx)

	if err := ctx.Err(); err != nil {
		return nil, LayerStats{}, fmt.Errorf("prompt: assemble cancelled: %w", err)
	}
	if agentName == "" {
		return nil, LayerStats{}, ErrAgentRequired
	}

	logger.Debug("prompt: assembling", "agent", agentName, "conversation_messages", len(conversation))

	query := ""
	for i := len(conversation) - 1; i >= 0; i-- {
		if conversation[i].Role == "user" {
			query = conversation[i].Content
			break
		}
	}

	layers, err := a.loadLayers(ctx, agentName, query)
	if err != nil {
		logger.Debug("prompt: load layers failed", "agent", agentName, "err", err)
		return nil, LayerStats{}, err
	}

	convoTokens := a.countMessages(conversation)
	stats := a.trim(logger, &layers, convoTokens)
	stats.Conversation = convoTokens
	stats.Total = stats.Rules + stats.User + stats.ProjectRules + stats.Persona + stats.AgentRules + stats.Facts + stats.Notes + stats.Episodes + stats.Conversation

	system := renderSystem(layers)
	out := make([]inference.Message, 0, len(conversation)+1)
	if system != "" {
		out = append(out, inference.Message{Role: "system", Content: system})
	}
	out = append(out, conversation...)

	logger.Debug("prompt: assembled",
		"agent", agentName,
		"rules_tokens", stats.Rules,
		"user_tokens", stats.User,
		"project_rules_tokens", stats.ProjectRules,
		"persona_tokens", stats.Persona,
		"agent_rules_tokens", stats.AgentRules,
		"facts_tokens", stats.Facts,
		"notes_tokens", stats.Notes,
		"episodes_tokens", stats.Episodes,
		"conversation_tokens", stats.Conversation,
		"total_tokens", stats.Total,
		"episodes_kept", len(layers.episodes),
		"recency_n", a.cfg.RecencyN,
	)

	return out, stats, nil
}

// loggerFor returns the configured logger augmented with the request
// id pulled from ctx. The id is omitted when ctx carries none so test
// code without the api handler in front still gets clean output.
func (a *DiskAssembler) loggerFor(ctx context.Context) *slog.Logger {
	if id := reqid.From(ctx); id != "" {
		return a.logger.With("request_id", id)
	}
	return a.logger
}

// rawLayers is the intermediate form - strings with their token counts -
// used between loading and rendering. The per-episode split lets the
// trimmer drop oldest-first without re-reading the files.
type rawLayers struct {
	rules        string
	user         string
	projectRules string
	persona      string
	agentRules   string
	facts        string
	notes        string
	episodes     []episode
}

type episode struct {
	path    string
	content string
	tokens  int
	score   float64
}

// loadLayers reads every file that contributes to the system prompt.
// Required files missing (rules.md, and persona.md when agentName is
// non-empty) surface as wrapped errors; optional files missing produce
// empty sections.
func (a *DiskAssembler) loadLayers(ctx context.Context, agentName string, query string) (rawLayers, error) {
	var lay rawLayers

	rules, err := a.readRequired(rulesPath)
	if err != nil {
		return rawLayers{}, err
	}
	lay.rules = rules

	user, err := a.readOptional(userPath)
	if err != nil {
		return rawLayers{}, err
	}
	lay.user = user

	slug := a.projectSlug
	if slug == "" {
		slug = project.GlobalSlug
	}
	if err := project.ValidateSlug(slug); err != nil {
		return rawLayers{}, fmt.Errorf("prompt: invalid project slug %q: %w", slug, err)
	}
	projectRules, err := a.readOptional(fmt.Sprintf(projectRulesPath, slug))
	if err != nil {
		return rawLayers{}, err
	}
	lay.projectRules = projectRules

	if agentName != "" {
		if err := agent.ValidateName(agentName); err != nil {
			return rawLayers{}, fmt.Errorf("prompt: invalid agent name %q: %w", agentName, err)
		}

		var globalAgent agent.Agent
		globalExists := false
		if ag, err := a.reg.Get(agentName); err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				return rawLayers{}, fmt.Errorf("prompt: resolve agent %q: %w", agentName, err)
			}
		} else {
			globalAgent = ag
			globalExists = true
		}

		persona, personaFound, err := a.resolveAgentFile(slug, agentName, "persona.md", globalAgent, globalExists)
		if err != nil {
			return rawLayers{}, err
		}
		if !personaFound {
			return rawLayers{}, fmt.Errorf("prompt: agent %q persona missing in project %q: %w", agentName, slug, fs.ErrNotExist)
		}
		lay.persona = persona

		agentRules, _, err := a.resolveAgentFile(slug, agentName, "rules.md", globalAgent, globalExists)
		if err != nil {
			return rawLayers{}, err
		}
		lay.agentRules = agentRules

		notes, _, err := a.resolveAgentFile(slug, agentName, "notes.md", globalAgent, globalExists)
		if err != nil {
			return rawLayers{}, err
		}
		lay.notes = notes

		eps, err := a.loadEpisodes(ctx, agentName, query)
		if err != nil {
			return rawLayers{}, err
		}
		lay.episodes = eps
	}

	facts, err := a.readOptional(factsPath)
	if err != nil {
		return rawLayers{}, err
	}
	lay.facts = facts

	return lay, nil
}

// loadEpisodes reads every *.md file under projects/global/episodes/<name>/
// sorted oldest-first (lexicographic file name matches the ISO
// timestamp naming convention from docs/architecture.md).
//
// When PromptConfig.RecencyN > 0 only the last N entries (the newest)
// are returned, so the budget-driven trim further down sees a slice
// already capped to recency. RecencyN <= 0 means unlimited.
func (a *DiskAssembler) loadEpisodes(ctx context.Context, agentName string, query string) ([]episode, error) {
	slug := a.projectSlug
	if slug == "" {
		slug = project.GlobalSlug
	}
	pattern := fmt.Sprintf(episodesGlobPattern, slug, agentName)
	paths, err := a.mem.Glob(pattern)
	if err != nil {
		return nil, fmt.Errorf("prompt: glob episodes: %w", err)
	}
	// path.Match returns sorted paths via our Reader, but be explicit
	// in case a future Reader drops the guarantee.
	sort.Strings(paths)
	out := make([]episode, 0, len(paths))
	for _, p := range paths {
		content, err := a.readOptional(p)
		if err != nil {
			return nil, err
		}
		if content == "" {
			continue
		}
		out = append(out, episode{
			path:    p,
			content: content,
			tokens:  a.tokenizer(content),
		})
	}

	// Blended retrieval: semantic similarity + recency.
	if a.idx != nil && a.emb != nil && len(out) > 0 && query != "" {
		vecs, err := a.emb.Embed(ctx, []string{query})
		if err == nil && len(vecs) > 0 {
			results, err := a.idx.Search(vecs[0], len(out)*2)
			if err == nil && len(results) > 0 {
				scores := make(map[string]float64, len(results))
				for _, r := range results {
					scores[r.SHA] = float64(r.Score)
				}
				n := len(out)
				for i := range out {
					semScore := scores[extractID(out[i].path)]
					recScore := float64(i+1) / float64(n)
					out[i].score = a.cfg.SemanticWeight*semScore +
						a.cfg.RecencyWeight*recScore
				}
				sort.SliceStable(out, func(i, j int) bool {
					return out[i].score > out[j].score
				})
			}
		}
	}

	if n := a.cfg.RecencyN; n > 0 && len(out) > n {
		out = out[len(out)-n:]
	}
	return out, nil
}

// readRequired returns the contents of p or an error wrapping
// fs.ErrNotExist when the file is missing. The error is prefixed so
// callers get a useful message without unwrapping.
func (a *DiskAssembler) readRequired(p string) (string, error) {
	b, err := a.mem.Read(p)
	if err != nil {
		return "", fmt.Errorf("prompt: read required %s: %w", p, err)
	}
	return string(b), nil
}

// readOptional returns the contents of p, or an empty string when the
// file is missing. Any other error is propagated.
func (a *DiskAssembler) readOptional(p string) (string, error) {
	b, err := a.mem.Read(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("prompt: read optional %s: %w", p, err)
	}
	return string(b), nil
}

func (a *DiskAssembler) resolveAgentFile(slug, agentName, fileName string, globalAgent agent.Agent, globalExists bool) (string, bool, error) {
	projPath := fmt.Sprintf("projects/%s/agents/%s/%s", slug, agentName, fileName)
	b, err := a.mem.Read(projPath)
	if err == nil {
		return string(b), true, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return "", false, fmt.Errorf("prompt: read project agent file %s: %w", projPath, err)
	}

	if globalExists {
		var globalPath string
		switch fileName {
		case "persona.md":
			globalPath = globalAgent.PersonaPath
		case "rules.md":
			globalPath = globalAgent.RulesPath
		case "notes.md":
			globalPath = globalAgent.NotesPath
		default:
			globalPath = path.Join("agents", agentName, fileName)
		}
		b, err = a.mem.Read(globalPath)
		if err == nil {
			return string(b), true, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", false, fmt.Errorf("prompt: read global agent file %s: %w", globalPath, err)
		}
	}

	return "", false, nil
}

// countMessages estimates tokens across a conversation. Role tokens are
// ignored on purpose; llama-server counts them via apply_chat_template
// when it actually builds the request, so double-counting would make
// the budget harder to reason about.
func (a *DiskAssembler) countMessages(msgs []inference.Message) int {
	total := 0
	for _, m := range msgs {
		total += a.tokenizer(m.Content)
	}
	return total
}

// trim enforces the memory token budget and, when ctx_size is set, the
// overall budget against conversation reserve. Fixed prompt layers are
// never trimmed. Episodes are dropped oldest-first until the memory budget
// is respected and the total budget fits. Returns the stats computed
// against the final layer set.
func (a *DiskAssembler) trim(logger *slog.Logger, lay *rawLayers, convoTokens int) LayerStats {
	stats := a.statsFor(lay)

	memBudget := a.cfg.MemoryTokenBudget
	if memBudget > 0 {
		for stats.Facts+stats.Notes+stats.Episodes > memBudget && len(lay.episodes) > 0 {
			dropped := lay.episodes[0]
			lay.episodes = lay.episodes[1:]
			logger.Debug("prompt: trimming episode for memory budget",
				"path", dropped.path,
				"tokens", dropped.tokens,
				"memory_token_budget", memBudget,
			)
			stats = a.statsFor(lay)
		}
	}

	if a.cfg.CtxSize > 0 {
		// The live conversation must not consume the reserve meant for
		// the model's next response. Anything already in the prompt
		// (system layers + current conversation) must fit inside
		// CtxSize - ConversationReserve.
		limit := max(a.cfg.CtxSize-a.cfg.ConversationReserve, 0)
		for a.totalFixed(&stats)+convoTokens > limit && len(lay.episodes) > 0 {
			dropped := lay.episodes[0]
			lay.episodes = lay.episodes[1:]
			logger.Debug("prompt: trimming episode for ctx limit",
				"path", dropped.path,
				"tokens", dropped.tokens,
				"ctx_limit", limit,
			)
			stats = a.statsFor(lay)
		}
	}

	// If even zero episodes exceeds the memory budget, we don't drop
	// facts or notes - the roadmap is explicit: warn, don't silently
	// collapse promoted knowledge.
	if memBudget > 0 && stats.Facts+stats.Notes+stats.Episodes > memBudget {
		logger.Warn("prompt: memory budget exceeded after trimming episodes",
			"memory_token_budget", memBudget,
			"facts_tokens", stats.Facts,
			"notes_tokens", stats.Notes,
			"episodes_tokens", stats.Episodes,
		)
	}

	return stats
}

// totalFixed returns the sum of all layer tokens except conversation,
// used under the ctx_size guardrail.
func (a *DiskAssembler) totalFixed(s *LayerStats) int {
	return s.Rules + s.User + s.ProjectRules + s.Persona + s.AgentRules + s.Facts + s.Notes + s.Episodes
}

// statsFor recounts layer tokens from the current raw layers.
func (a *DiskAssembler) statsFor(lay *rawLayers) LayerStats {
	var s LayerStats
	s.Rules = a.tokenizer(lay.rules)
	s.User = a.tokenizer(lay.user)
	s.ProjectRules = a.tokenizer(lay.projectRules)
	s.Persona = a.tokenizer(lay.persona)
	s.AgentRules = a.tokenizer(lay.agentRules)
	s.Facts = a.tokenizer(lay.facts)
	s.Notes = a.tokenizer(lay.notes)
	for _, ep := range lay.episodes {
		s.Episodes += ep.tokens
	}
	return s
}

// renderSystem concatenates the present layers into a single system
// message with markdown section headers. Empty layers are skipped so
// the resulting prompt never has a header without a body.
func renderSystem(lay rawLayers) string {
	var b strings.Builder
	writeSection(&b, rulesHeader, lay.rules)
	writeSection(&b, userHeader, lay.user)
	writeSection(&b, projectRulesHeader, lay.projectRules)
	writeSection(&b, personaHeader, lay.persona)
	writeSection(&b, agentRulesHeader, lay.agentRules)
	writeSection(&b, factsHeader, lay.facts)
	writeSection(&b, notesHeader, lay.notes)
	if len(lay.episodes) > 0 {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(episodesHeader)
		b.WriteString("\n\n")
		for i, ep := range lay.episodes {
			if i > 0 {
				b.WriteString("\n\n")
			}
			b.WriteString("## ")
			b.WriteString(path.Base(ep.path))
			b.WriteString("\n\n")
			b.WriteString(strings.TrimRight(ep.content, "\n"))
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func writeSection(b *strings.Builder, header, content string) {
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		return
	}
	if b.Len() > 0 {
		b.WriteString("\n\n")
	}
	b.WriteString(header)
	b.WriteString("\n\n")
	b.WriteString(strings.TrimRight(content, "\n"))
}

func extractID(epPath string) string {
	base := path.Base(epPath)
	return strings.TrimSuffix(base, ".md")
}
