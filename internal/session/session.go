// Package session owns the lifecycle of an in-progress conversation:
// in-memory state, save-and-summarize, and recovery-on-resume.
//
// A session has a stable ID minted on first save: a sanitized RFC3339
// timestamp (UTC, hyphens instead of colons) so the same string is a
// valid Windows filename and the episode filename stem.
//
// Saves are append-only and last-wins-by-ID:
//   - the summary is regenerated and written to episodes/
//     <agent>/<id>.md (committed to git, single file per commit)
//   - the raw conversation is written to episodes/
//     <agent>/<id>.json (working-tree-only, intentionally uncommitted)
//   - two records per save are appended to sessions.jsonl: an explicit
//     pending record once the sidecar is durable, and an explicit complete
//     record once the episode is published and committed, both carrying the
//     same monotonic attempt identifier. Recovery selects the winning record
//     per session by attempt then state precedence, never by timestamps,
//     empty paths, or physical log order (see log.go).
//
// The project slug is set via ManagerDeps.ProjectSlug at construction
// time; paths are computed from the manager's stored value.
package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/VrncQuentin/harness/internal/git"
	"github.com/VrncQuentin/harness/internal/inference"
	"github.com/VrncQuentin/harness/internal/memory"
	"github.com/VrncQuentin/harness/internal/project"
)

const (
	// episodesRootRel is the repo-relative root directory for saved episodes.
	episodesRootRel = "episodes"
	// sessionsLogRel is the repo-relative append-only session log path.
	sessionsLogRel = "sessions.jsonl"
	// SessionsLogRel is the exported alias used by packages that read the
	// log without going through a Manager (e.g. per-project session listing).
	SessionsLogRel = sessionsLogRel
	// episodeFileSuffix is the extension used for episode markdown files.
	episodeFileSuffix = ".md"
	// episodeSidecarSuffix is the extension used for raw conversation sidecars.
	episodeSidecarSuffix = ".json"
)

// idTimeFormat is the time.Format layout used to mint a session ID.
// Hyphens replace colons so the same string doubles as a filename on
// Windows; the trailing Z marks UTC.
const idTimeFormat = "2006-01-02T15-04-05.000000000Z"

// summarizerTimeout caps how long a single Save waits for the
// summarizer call to drain. Long enough that a slow first-token does
// not abort the save, short enough that a wedged inference path still
// fails the user's click rather than hanging forever.
const summarizerTimeout = 60 * time.Second

// Sentinel errors callers may want to distinguish.
var (
	// ErrUnknownSession is returned by Append/Save/End when the caller
	// passes an ID the manager has never seen.
	ErrUnknownSession = errors.New("session: unknown id")
	// ErrConversationLost is returned by Resume when the session's .md
	// summary is committed but the .json sidecar is missing - typical
	// after a fresh clone of the memory repo. The UI uses errors.Is to
	// disable the resume row instead of crashing.
	ErrConversationLost = errors.New("session: conversation sidecar missing")
)

// Session is one in-progress conversation. All fields are immutable
// after Start except Conversation, which the manager appends to.
type Session struct {
	ID           string
	Agent        string
	Project      string
	StartedAt    time.Time
	Conversation []inference.Message

	// saveSeq counts how many times Save has succeeded for this session.
	// Persisted into each sessions.jsonl record so the log carries an
	// ordering fingerprint independent of wall clock skew.
	saveSeq int
	// attempt is the last save attempt allocated for this session, including
	// failed ones. It is the in-memory high-water mark that keeps allocated
	// attempt identifiers strictly increasing (a failed attempt is never
	// reused). Restored from the log on Resume.
	attempt int
}

// SaveResult is the outcome of a successful Save. Callers use it to
// confirm to the user (UI toast, API response) that the episode landed.
type SaveResult struct {
	ID          string
	EpisodePath string
	Summary     string
	// EpisodeBody is the exact rendered markdown written to EpisodePath.
	// Indexing consumers hash and chunk this, not Summary, so save-time
	// embedding and a later on-disk rebuild agree on episode identity.
	EpisodeBody string
	SavedAt     time.Time
	SaveSeq     int
	// Attempt is the monotonic save-attempt identifier this save consumed.
	// It includes the failed attempts that preceded it.
	Attempt int
}

// MetricsRecorder is the narrow surface session needs from the metrics
// recorder. Kept as an interface so tests can drop in a fake without
// pulling in a real *sql.DB.
type MetricsRecorder interface {
	SessionCount(n int) error
	EpisodeCount(n int) error
	GitCommitLatencyMS(d time.Duration) error
}

// FileWriter is the subset of memory.FileWriter session needs to write
// episode artifacts. Restated locally so the manager can be exercised
// against a stub without a real DirReader.
type FileWriter interface {
	WriteFile(relPath string, data []byte) error
}

// FileReader is the subset of memory.Repo session needs to count
// episode files and hydrate a sidecar on Resume.
type FileReader interface {
	Read(relPath string) ([]byte, error)
	Walk(relPath string) ([]memory.Entry, error)
}

// Committer is the subset of *git.Repo session uses. Tests inject a
// fake to record commit messages without spinning up a real repo.
type Committer interface {
	Commit(msg string, files []string) (string, error)
}

// SummarizerPromptFunc returns the live summarizer system prompt. The
// manager calls this on every Save so /config edits propagate without
// rebuilding the manager.
type SummarizerPromptFunc func() string

// AfterSaveFunc is an optional callback invoked after a successful Save
// (including the git commit). The runtime wires it to trigger embed-on-commit.
type AfterSaveFunc func(ctx context.Context, result SaveResult) error

// ManagerDeps bundles the dependencies a Manager needs. Constructed by
// the runtime wiring; tests build it inline.
type ManagerDeps struct {
	Repo             Committer
	Writer           FileWriter
	Reader           FileReader
	Appender         LogAppender
	Inference        inference.Client
	Metrics          MetricsRecorder
	SummarizerPrompt SummarizerPromptFunc
	AfterSave        AfterSaveFunc
	Now              func() time.Time // optional; defaults to time.Now
}

// Manager owns the live in-memory sessions and orchestrates save/resume
// against the memory repo. Safe for concurrent use.
type Manager struct {
	mu          sync.Mutex
	sessions    map[string]*Session
	knownIDs    map[string]struct{}
	issuedIDs   map[string]struct{}
	saveMu      sync.Mutex
	deps        ManagerDeps
	summarizer  *Summarizer
	projectSlug string
}

// episodeMarkdownPath returns the repo-relative path to an episode .md
// file for the given agent and session id.
func episodeMarkdownPath(agent, id string) string {
	return path.Join(episodesRootRel, agent, id+episodeFileSuffix)
}

// episodeSidecarPath returns the repo-relative path to an episode .json
// sidecar for the given agent and session id.
func episodeSidecarPath(agent, id string) string {
	return path.Join(episodesRootRel, agent, id+episodeSidecarSuffix)
}

// NewManager constructs a Manager from deps. The caller is expected to
// have already validated the project memory repo so the
// FileWriter/Reader/Committer/Inference references are non-nil.
func NewManager(deps ManagerDeps, projectSlug string) (*Manager, error) {
	if deps.Repo == nil {
		return nil, errors.New("session: ManagerDeps.Repo is required")
	}
	if deps.Writer == nil {
		return nil, errors.New("session: ManagerDeps.Writer is required")
	}
	if deps.Reader == nil {
		return nil, errors.New("session: ManagerDeps.Reader is required")
	}
	if deps.Appender == nil {
		return nil, errors.New("session: ManagerDeps.Appender is required")
	}
	if deps.Inference == nil {
		return nil, errors.New("session: ManagerDeps.Inference is required")
	}
	if deps.SummarizerPrompt == nil {
		deps.SummarizerPrompt = func() string { return "" }
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if projectSlug == "" {
		projectSlug = project.GlobalSlug
	}
	return &Manager{
		sessions:    make(map[string]*Session),
		knownIDs:    make(map[string]struct{}),
		issuedIDs:   make(map[string]struct{}),
		deps:        deps,
		summarizer:  NewSummarizer(deps.Inference, deps.SummarizerPrompt, summarizerTimeout),
		projectSlug: projectSlug,
	}, nil
}

// Start mints a new session bound to agent and registers it as live.
// The returned *Session is a snapshot copy so callers cannot mutate the
// manager's internal state by accident; mutations go through Append.
func (m *Manager) Start(agent string) *Session {
	now := m.deps.Now().UTC()
	id := now.Format(idTimeFormat)

	m.mu.Lock()
	defer m.mu.Unlock()

	// Guard against the (extremely unlikely) clock-collision case where
	// two Starts in the same UTC second want the same ID. Append a
	// disambiguator so the second session does not clobber the first.
	if _, exists := m.issuedIDs[id]; exists {
		base := id
		for n := 1; ; n++ {
			candidate := fmt.Sprintf("%s-%d", base, n)
			if _, issued := m.issuedIDs[candidate]; !issued {
				id = candidate
				break
			}
		}
	}

	s := &Session{
		ID:           id,
		Agent:        agent,
		Project:      m.projectSlug,
		StartedAt:    now,
		Conversation: nil,
	}
	m.sessions[id] = s
	m.issuedIDs[id] = struct{}{}
	return cloneSession(s)
}

// Resume hydrates a previously saved session from its sidecar JSON. It
// returns the session in memory (registered as live) so further Append
// calls extend it. ErrConversationLost is returned when only the .md
// committed history remains - typical for a fresh clone.
func (m *Manager) Resume(id string) (*Session, error) {
	if id == "" {
		return nil, fmt.Errorf("session: resume: empty id")
	}
	rec, err := m.findLatestRecord(id)
	if err != nil {
		return nil, err
	}
	if rec == nil {
		return nil, fmt.Errorf("session: resume %s: %w", id, ErrUnknownSession)
	}
	sidecarPath := episodeSidecarPath(rec.Agent, rec.ID)
	body, err := m.deps.Reader.Read(sidecarPath)
	if err != nil {
		return nil, fmt.Errorf("session: resume %s: %w", id, ErrConversationLost)
	}
	conv, err := decodeConversation(body)
	if err != nil {
		return nil, fmt.Errorf("session: resume %s: decode sidecar: %w", id, err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	s := &Session{
		ID:           rec.ID,
		Agent:        rec.Agent,
		Project:      rec.Project,
		StartedAt:    rec.StartedAt,
		Conversation: conv,
		saveSeq:      rec.SaveSeq,
		// Restore the attempt high-water mark from the winning record so the
		// next allocation is strictly greater than every record for this
		// session (including a legacy record's save_seq key).
		attempt: effectiveAttempt(*rec),
	}
	m.sessions[rec.ID] = s
	m.knownIDs[rec.ID] = struct{}{}
	m.issuedIDs[rec.ID] = struct{}{}
	return cloneSession(s), nil
}

// Append adds msg to the live session identified by id. Returns
// ErrUnknownSession if the manager has never seen that id (callers must
// Start or Resume first).
func (m *Manager) Append(id string, msg inference.Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return fmt.Errorf("session: append %s: %w", id, ErrUnknownSession)
	}
	s.Conversation = append(s.Conversation, msg)
	return nil
}

// Snapshot returns a copy of the live session keyed by id, or nil if
// the manager has never seen it. Used by the UI handler to fetch the
// transcript without exposing the manager's mutex.
func (m *Manager) Snapshot(id string) *Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return nil
	}
	return cloneSession(s)
}

// End drops the in-memory state for id without saving. Used when the
// browser explicitly clicks "New session" after the previous one has
// already been saved (or when the user does not want to persist a
// throwaway).
func (m *Manager) End(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, id)
}

// Save persists the live session under an explicit, durable recovery-state
// lifecycle:
//
//  1. allocate a monotonic save attempt (before any fallible work);
//  2. durably publish the raw conversation sidecar;
//  3. append and fsync an explicit pending record for that attempt;
//  4. run summarization;
//  5. publish and commit the episode;
//  6. append and fsync an explicit complete record for the same attempt.
//
// A summarizer, episode-publication, or commit failure leaves a discoverable
// pending session whose raw sidecar can be resumed; a complete record is only
// ever appended after the episode is published and committed. The returned
// SaveResult lets the caller (UI handler) confirm to the browser that the
// episode landed.
//
// Save is serialized by a manager-wide save lock (saveMu): only one save
// runs at a time across all sessions, so summarization, artifact writes,
// the git commit, the sessions.jsonl appends, and the after-save hook never
// interleave. The short manager mutex (mu) is taken only to read the live
// session and later to bump saveSeq/attempt/knownIDs; it is released for the
// duration of the I/O so Append and Snapshot stay responsive mid-save.
func (m *Manager) Save(ctx context.Context, id string) (SaveResult, error) {
	m.saveMu.Lock()
	defer m.saveMu.Unlock()
	m.mu.Lock()
	s, ok := m.sessions[id]
	if !ok {
		m.mu.Unlock()
		return SaveResult{}, fmt.Errorf("session: save %s: %w", id, ErrUnknownSession)
	}
	if len(s.Conversation) == 0 {
		m.mu.Unlock()
		return SaveResult{}, fmt.Errorf("session: save %s: conversation is empty", id)
	}
	// Allocate the monotonic save attempt and bump the live session before
	// any fallible work, so a failed attempt is never reused by a retry: each
	// retry consumes a fresh, strictly larger attempt.
	attempt := s.attempt + 1
	s.attempt = attempt
	// Snapshot before releasing the lock so the summarizer call sees a
	// stable copy, and so the caller cannot Append while we are mid-save.
	snap := cloneSession(s)
	_, alreadyKnown := m.knownIDs[id]
	m.mu.Unlock()

	episodePath := episodeMarkdownPath(snap.Agent, snap.ID)
	sidecarPath := episodeSidecarPath(snap.Agent, snap.ID)

	// 1. Durably publish the raw conversation sidecar first. From this point
	// the recovery material exists: a summarizer or episode-publication
	// failure can only leave the session resumable, never undiscoverable.
	sidecarBytes, err := encodeConversation(snap.Conversation)
	if err != nil {
		return SaveResult{}, fmt.Errorf("session: encode sidecar %s: %w", sidecarPath, err)
	}
	if err := m.deps.Writer.WriteFile(sidecarPath, sidecarBytes); err != nil {
		// No pending record: nothing to discover, and nothing to resume from
		// that was not already recoverable.
		return SaveResult{}, fmt.Errorf("session: write sidecar %s: %w", sidecarPath, err)
	}

	// 2. Append and fsync the explicit pending record. The session is now
	// discoverable and resumable from the sidecar.
	pendingRec := Record{
		ID:        snap.ID,
		Agent:     snap.Agent,
		Project:   snap.Project,
		StartedAt: snap.StartedAt,
		SavedAt:   m.deps.Now().UTC(),
		SaveSeq:   snap.saveSeq,
		Attempt:   attempt,
		State:     StatePending,
	}
	if err := AppendRecord(m.deps.Appender, sessionsLogRel, pendingRec); err != nil {
		return SaveResult{}, fmt.Errorf("session: append pending record: %w", err)
	}

	// 3. Summarize. On failure the pending record remains and the session is
	// recovered from the sidecar.
	summary, err := m.summarizer.Summarize(ctx, snap.Conversation)
	if err != nil {
		return SaveResult{}, fmt.Errorf("session: summarize %s: %w", id, err)
	}
	if strings.TrimSpace(summary) == "" {
		return SaveResult{}, fmt.Errorf("session: summarize %s: summarizer returned empty body", id)
	}

	// 4. Publish and commit the episode.
	body := renderEpisodeBody(snap.ID, summary)
	if err := m.deps.Writer.WriteFile(episodePath, []byte(body)); err != nil {
		return SaveResult{}, fmt.Errorf("session: write episode %s: %w", episodePath, err)
	}
	commitMsg := git.BuildMessage(
		map[string]string{"agent": snap.Agent, "type": "episode"},
		firstLine(summary),
	)
	commitStart := time.Now()
	_, err = m.deps.Repo.Commit(commitMsg, []string{episodePath})
	commitDur := time.Since(commitStart)
	if err != nil {
		return SaveResult{}, fmt.Errorf("session: commit %s: %w", episodePath, err)
	}

	// 5. Append and fsync the explicit complete record for the same attempt.
	// The episode is published and committed before this line, so a complete
	// record always describes a fully durable save.
	saveSeq := snap.saveSeq + 1
	completeRec := Record{
		ID:          snap.ID,
		Agent:       snap.Agent,
		Project:     snap.Project,
		StartedAt:   snap.StartedAt,
		SavedAt:     m.deps.Now().UTC(),
		SaveSeq:     saveSeq,
		EpisodePath: episodePath,
		Attempt:     attempt,
		State:       StateComplete,
	}
	if err := AppendRecord(m.deps.Appender, sessionsLogRel, completeRec); err != nil {
		return SaveResult{}, fmt.Errorf("session: append complete record: %w", err)
	}

	// Re-acquire the lock to bump the live session's saveSeq and update the
	// known-ids set. Doing this after the writes land avoids a half-saved
	// state that future calls to Save would mistake for progress. attempt was
	// already bumped when it was allocated, so only saveSeq advances here.
	m.mu.Lock()
	if live, ok := m.sessions[id]; ok {
		live.saveSeq = saveSeq
	}
	m.knownIDs[id] = struct{}{}
	knownCount := len(m.knownIDs)
	m.mu.Unlock()

	if rec := m.deps.Metrics; rec != nil {
		// SessionCount only bumps when this is the first save of the
		// id; otherwise the gauge is unchanged. EpisodeCount is the
		// count of distinct .md files under the episodes root.
		if !alreadyKnown {
			_ = rec.SessionCount(knownCount)
		}
		_ = rec.EpisodeCount(m.countEpisodeFiles())
		_ = rec.GitCommitLatencyMS(commitDur)
	}

	result := SaveResult{
		ID:          snap.ID,
		EpisodePath: episodePath,
		Summary:     summary,
		EpisodeBody: body,
		SavedAt:     completeRec.SavedAt,
		SaveSeq:     saveSeq,
		Attempt:     attempt,
	}

	if m.deps.AfterSave != nil {
		if err := m.deps.AfterSave(ctx, result); err != nil {
			slog.Warn("session: after-save hook", "id", id, "err", err)
		}
	}

	return result, nil
}

// FlushAll saves every live session under the given ctx. Used by the runtime
// shutdown flush and the memory/API rebuild path. Errors per-session are
// logged via the returned error joiner; one bad save does not stop the rest.
func (m *Manager) FlushAll(ctx context.Context) error {
	m.mu.Lock()
	ids := make([]string, 0, len(m.sessions))
	for id, s := range m.sessions {
		if len(s.Conversation) == 0 {
			// Empty conversations are not interesting to persist; skip
			// rather than fail the flush.
			continue
		}
		ids = append(ids, id)
	}
	m.mu.Unlock()
	sort.Strings(ids) // deterministic order helps tests

	var errs []error
	for _, id := range ids {
		if _, err := m.Save(ctx, id); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return errors.Join(errs...)
}

// Records returns the deduped list of saved sessions for the given
// agent, newest-first by SavedAt. Used by the chat resume picker.
// An empty slice + nil error means "no sessions yet".
func (m *Manager) Records(agent string) ([]Record, error) {
	all, err := m.allRecords()
	if err != nil {
		return nil, err
	}
	deduped := LatestPerID(all)
	out := make([]Record, 0, len(deduped))
	for _, r := range deduped {
		if r.Agent != agent {
			continue
		}
		out = append(out, r)
	}
	sortByNewest(out)
	return out, nil
}

// SidecarConversation reads the .json sidecar for the given record and
// returns the raw conversation. ErrConversationLost is returned when
// the sidecar is missing.
func (m *Manager) SidecarConversation(agent, id string) ([]inference.Message, error) {
	if !validAgent(agent) || !validID(id) {
		return nil, fmt.Errorf("session: sidecar %s/%s: invalid argument", agent, id)
	}
	sidecarPath := episodeSidecarPath(agent, id)
	body, err := m.deps.Reader.Read(sidecarPath)
	if err != nil {
		return nil, fmt.Errorf("session: sidecar %s: %w", sidecarPath, ErrConversationLost)
	}
	return decodeConversation(body)
}

// allRecords returns every record in the sessions log (oldest-first).
// Tolerates a missing log (returns an empty slice).
func (m *Manager) allRecords() ([]Record, error) {
	return ReadAll(m.deps.Reader, sessionsLogRel)
}

func (m *Manager) findLatestRecord(id string) (*Record, error) {
	all, err := m.allRecords()
	if err != nil {
		return nil, err
	}
	// Select the winning record by the explicit recovery state — highest
	// effective attempt, complete superseding pending for the same attempt —
	// never by physical log order or wall-clock timestamps. Malformed records
	// are skipped so they can never supersede valid history.
	var latest *Record
	for i := range all {
		if all[i].ID != id {
			continue
		}
		if !validRecord(all[i]) {
			continue
		}
		if latest == nil || supersedes(*latest, all[i]) {
			cp := all[i]
			latest = &cp
		}
	}
	return latest, nil
}

// countEpisodeFiles walks the episodes root and counts .md files. The
// count is used as the EpisodeCount metric. Cheap enough to compute on
// every save - the tree is small and this keeps discovery simple.
func (m *Manager) countEpisodeFiles() int {
	entries, err := m.deps.Reader.Walk(episodesRootRel)
	if err != nil {
		return 0
	}
	count := 0
	for _, e := range entries {
		if e.Dir {
			continue
		}
		if !strings.HasSuffix(e.Path, episodeFileSuffix) {
			continue
		}
		count++
	}
	return count
}

// cloneSession returns a defensive copy of s so callers cannot mutate
// the manager's internal state by holding the returned pointer.
func cloneSession(s *Session) *Session {
	if s == nil {
		return nil
	}
	conv := append([]inference.Message(nil), s.Conversation...)
	cp := *s
	cp.Conversation = conv
	return &cp
}

// firstLine returns the first non-empty line of text, trimmed. Used as
// the human-readable summary in the commit message.
func firstLine(text string) string {
	for _, line := range strings.Split(text, "\n") {
		t := strings.TrimSpace(line)
		if t != "" {
			return t
		}
	}
	return "session episode"
}

// renderEpisodeBody composes the .md body for an episode. The body is
// the summarizer's markdown with a small header so a future reader can
// tell at a glance which session produced the file.
func renderEpisodeBody(id, summary string) string {
	var b strings.Builder
	b.WriteString("# Episode ")
	b.WriteString(id)
	b.WriteString("\n\n")
	b.WriteString(strings.TrimRight(summary, "\n"))
	b.WriteByte('\n')
	return b.String()
}

// validAgent rejects names that contain a path separator so a malicious
// id passed via the resume endpoint cannot escape the agent directory.
// Empty names are also rejected because the path templates would render
// "<agent>" literally.
func validAgent(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	if strings.ContainsAny(name, `/\`) {
		return false
	}
	return true
}

// validID enforces the same restriction for the session id. The id is
// minted internally so rejection should be vanishingly rare; we still
// check it before stitching it into a path so a hand-crafted resume URL
// cannot steer reads.
func validID(id string) bool {
	if id == "" || id == "." || id == ".." {
		return false
	}
	if strings.ContainsAny(id, `/\`) {
		return false
	}
	return true
}
