// Package prompt assembles the layered system prompt sent to the model.
// Layers are stacked in a fixed order and budgeted against the memory
// token limit and the conversation reserve; trimming is oldest-episode
// first, and the mandatory layers (rules, user, persona) are never
// dropped.
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
	"github.com/vrnc/harness/internal/inference"
	"github.com/vrnc/harness/internal/memory"
)

// Well-known paths inside the memory repo. Kept as package-level
// constants so tests and hot-reload share the same strings.
const (
	rulesPath = "global/rules.md"
	userPath  = "global/user.md"
	factsPath = "global/facts.md"

	episodesGlob = "agents/%s/episodes/*.md"
)

// Static markdown section headers. Each optional layer is skipped if
// its file is missing or empty, so these are only written when the
// layer contributes content.
const (
	rulesHeader    = "# Rules"
	userHeader     = "# User"
	personaHeader  = "# Persona"
	factsHeader    = "# Facts"
	notesHeader    = "# Notes"
	episodesHeader = "# Episodes"
)

// LayerStats reports token counts per layer and the overall total for
// the assembled output. It is logged and exposed to the UI logs page
// so the operator can see exactly what went into the prompt.
type LayerStats struct {
	Rules        int
	User         int
	Persona      int
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
	mem       memory.Reader
	reg       agent.Registry
	cfg       config.PromptConfig
	logger    *slog.Logger
	tokenizer func(string) int
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

// defaultTokenize is the rune-quarter heuristic. Fast, deterministic,
// and close enough for budgeting against caps that already carry
// generous headroom.
func defaultTokenize(s string) int {
	return (utf8.RuneCountInString(s) + 3) / 4
}

// Assemble builds the final message slice for agentName given
// conversation. It returns the system prompt prepended to the
// conversation, the per-layer token counts, and any error that halts
// assembly (missing required layer). Missing optional layers produce
// empty sections rather than errors.
func (a *DiskAssembler) Assemble(ctx context.Context, agentName string, conversation []inference.Message) ([]inference.Message, LayerStats, error) {
	if err := ctx.Err(); err != nil {
		return nil, LayerStats{}, fmt.Errorf("prompt: assemble cancelled: %w", err)
	}

	layers, err := a.loadLayers(agentName)
	if err != nil {
		return nil, LayerStats{}, err
	}

	convoTokens := a.countMessages(conversation)
	stats := a.trim(&layers, convoTokens)
	stats.Conversation = convoTokens
	stats.Total = stats.Rules + stats.User + stats.Persona + stats.Facts + stats.Notes + stats.Episodes + stats.Conversation

	system := renderSystem(layers)
	out := make([]inference.Message, 0, len(conversation)+1)
	if system != "" {
		out = append(out, inference.Message{Role: "system", Content: system})
	}
	out = append(out, conversation...)
	return out, stats, nil
}

// rawLayers is the intermediate form - strings with their token counts -
// used between loading and rendering. The per-episode split lets the
// trimmer drop oldest-first without re-reading the files.
type rawLayers struct {
	rules    string
	user     string
	persona  string
	facts    string
	notes    string
	episodes []episode
}

type episode struct {
	path    string
	content string
	tokens  int
}

// loadLayers reads every file that contributes to the system prompt.
// Required files missing (rules.md, and persona.md when agentName is
// non-empty) surface as wrapped errors; optional files missing produce
// empty sections.
func (a *DiskAssembler) loadLayers(agentName string) (rawLayers, error) {
	var lay rawLayers

	rules, err := a.readRequired(rulesPath)
	if err != nil {
		return rawLayers{}, err
	}
	lay.rules = rules

	if content, err := a.readOptional(userPath); err != nil {
		return rawLayers{}, err
	} else {
		lay.user = content
	}

	if agentName != "" {
		ag, err := a.reg.Get(agentName)
		if err != nil {
			return rawLayers{}, fmt.Errorf("prompt: resolve agent %q: %w", agentName, err)
		}
		persona, err := a.readRequired(ag.PersonaPath)
		if err != nil {
			return rawLayers{}, err
		}
		lay.persona = persona

		notes, err := a.readOptional(ag.NotesPath)
		if err != nil {
			return rawLayers{}, err
		}
		lay.notes = notes

		eps, err := a.loadEpisodes(agentName)
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

// loadEpisodes reads every *.md file under agents/<name>/episodes/
// sorted oldest-first (lexicographic file name matches the ISO
// timestamp naming convention from docs/architecture.md).
func (a *DiskAssembler) loadEpisodes(agentName string) ([]episode, error) {
	pattern := fmt.Sprintf(episodesGlob, agentName)
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
// overall budget against conversation reserve. Layers 1-3 (rules,
// user, persona) are never trimmed. Episodes are dropped oldest-first
// until the memory budget is respected and the total budget fits.
// Returns the stats computed against the final layer set.
func (a *DiskAssembler) trim(lay *rawLayers, convoTokens int) LayerStats {
	stats := a.statsFor(lay)

	memBudget := a.cfg.MemoryTokenBudget
	if memBudget > 0 {
		for stats.Facts+stats.Notes+stats.Episodes > memBudget && len(lay.episodes) > 0 {
			lay.episodes = lay.episodes[1:]
			stats = a.statsFor(lay)
		}
	}

	if a.cfg.CtxSize > 0 {
		// The live conversation must not consume the reserve meant for
		// the model's next response. Anything already in the prompt
		// (system layers + current conversation) must fit inside
		// CtxSize - ConversationReserve.
		limit := a.cfg.CtxSize - a.cfg.ConversationReserve
		if limit < 0 {
			limit = 0
		}
		for a.totalFixed(&stats)+convoTokens > limit && len(lay.episodes) > 0 {
			lay.episodes = lay.episodes[1:]
			stats = a.statsFor(lay)
		}
	}

	// If even zero episodes exceeds the memory budget, we don't drop
	// facts or notes - the roadmap is explicit: warn, don't silently
	// collapse promoted knowledge.
	if memBudget > 0 && stats.Facts+stats.Notes+stats.Episodes > memBudget {
		a.logger.Warn("prompt: memory budget exceeded after trimming episodes",
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
	return s.Rules + s.User + s.Persona + s.Facts + s.Notes + s.Episodes
}

// statsFor recounts layer tokens from the current raw layers.
func (a *DiskAssembler) statsFor(lay *rawLayers) LayerStats {
	var s LayerStats
	s.Rules = a.tokenizer(lay.rules)
	s.User = a.tokenizer(lay.user)
	s.Persona = a.tokenizer(lay.persona)
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
	writeSection(&b, personaHeader, lay.persona)
	writeSection(&b, factsHeader, lay.facts)
	writeSection(&b, notesHeader, lay.notes)
	if len(lay.episodes) > 0 {
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
		b.WriteString("\n")
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
