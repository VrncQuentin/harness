// Package api implements the optional OpenAI-compatible HTTP server that lets
// external OpenAI-compatible clients talk to the harness. It mirrors a tiny
// slice of the OpenAI surface - /v1/chat/completions (streaming) and
// /v1/models - and routes every request through the local prompt Assembler
// and inference queue so memory + persona injection is transparent to callers.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/VrncQuentin/harness/internal/inference"
	"github.com/VrncQuentin/harness/internal/queue"
	"github.com/VrncQuentin/harness/internal/reqid"
)

// Assembler builds the final message list (rules/persona/memory/conversation)
// handed to the inference backend. Kept narrow on purpose: main.go wires a
// concrete implementation and we only need the one method here.
type Assembler interface {
	Assemble(ctx context.Context, agentName string, conversation []inference.Message) ([]inference.Message, error)
}

// Enqueuer submits a request to the shared inference queue. We accept the
// interface rather than *queue.Queue so tests can drive completion behaviour
// without a real worker loop.
type Enqueuer interface {
	Enqueue(req queue.Request) error
}

// SessionRecorder is the optional surface the API server uses to record
// one harness session per /v1/chat/completions request. The request
// messages and assistant response are appended so API traffic can produce
// the same episode records as browser chat.
type SessionRecorder interface {
	Start(agent string) Session
	Append(id string, role, content string) error
	Save(ctx context.Context, id string) error
	End(id string)
}

// Session is the small subset of session.Session the API needs.
type Session struct {
	ID    string
	Agent string
}

// Server is the API HTTP server. Zero value is not usable; build one with
// NewServer.
type Server struct {
	port      int
	asm       Assembler
	q         Enqueuer
	rec       SessionRecorder
	startTime time.Time
	logger    *slog.Logger

	httpSrv *http.Server
}

// NewServer constructs a Server bound to the given port. The caller owns
// enable/disable: main.go decides whether to call Start based on the config
// flag, this type always assumes it should serve when asked.
//
// rec may be nil; the server then runs without session recording.
func NewServer(port int, asm Assembler, q Enqueuer, rec SessionRecorder) *Server {
	return &Server{
		port:      port,
		asm:       asm,
		q:         q,
		rec:       rec,
		startTime: time.Now(),
		logger:    slog.Default().With(slog.String("component", "api")),
	}
}

// SetSessionRecorder replaces the session recorder. It is safe to call
// while the server is running.
func (s *Server) SetSessionRecorder(rec SessionRecorder) {
	s.rec = rec
}

// Start binds and begins serving in a background goroutine. It returns early
// on bind failure so main.go can surface the error to the UI instead of
// silently never listening. Shutdown is triggered by cancelling ctx.
func (s *Server) Start(ctx context.Context) error {
	srv := &http.Server{
		// Loopback only, matching the UI server. The OpenAI-compatible API
		// is unauthenticated; exposing it beyond the host would hand any
		// machine on the network free inference and session writes.
		Addr:    fmt.Sprintf("127.0.0.1:%d", s.port),
		Handler: s.handler(),
	}

	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		return fmt.Errorf("api: bind %s: %w", srv.Addr, err)
	}

	s.httpSrv = srv

	go func() {
		if err := srv.Serve(ln); unexpectedServeError(err) {
			s.logger.Error("api serve failed", slog.Any("err", err))
		}
	}()

	go func() {
		<-ctx.Done()
		s.Stop()
	}()

	return nil
}

func unexpectedServeError(err error) bool {
	return err != nil && !errors.Is(err, http.ErrServerClosed)
}

// Stop gracefully shuts the server down. Idempotent: safe to call before
// Start or more than once.
func (s *Server) Stop() {
	if s.httpSrv == nil {
		return
	}
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = s.httpSrv.Shutdown(shutCtx)
}

// handler returns the mux. Exposed at package level (lowercase) so tests can
// drive handler logic via httptest.NewServer without going through a bind.
func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", s.handleChatCompletions)
	mux.HandleFunc("/v1/models", s.handleModels)
	return mux
}

// chatRequest is the OpenAI-compatible request body. Agent is a harness
// extension; the X-Harness-Agent header takes precedence if both are set.
type chatRequest struct {
	inference.CompletionRequest
	Agent string `json:"agent,omitempty"`
}

// apiError is the OpenAI-compatible error envelope.
type apiError struct {
	Error apiErrorBody `json:"error"`
}

type apiErrorBody struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
}

// chatChunk is a single streaming chunk of /v1/chat/completions.
type chatChunk struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []chunkChoice `json:"choices"`
}

type chunkChoice struct {
	Index        int        `json:"index"`
	Delta        chunkDelta `json:"delta"`
	FinishReason *string    `json:"finish_reason"`
}

type chunkDelta struct {
	Content string `json:"content,omitempty"`
}

// handleChatCompletions is POST /v1/chat/completions. Streaming is required.
func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, apiErrorBody{
			Message: "method not allowed",
			Type:    "invalid_request_error",
		})
		return
	}

	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, apiErrorBody{
			Message: fmt.Sprintf("invalid JSON: %s", err.Error()),
			Type:    "invalid_request_error",
		})
		return
	}

	// Non-streaming responses are not supported by this API surface.
	if !req.Stream {
		writeJSONError(w, http.StatusBadRequest, apiErrorBody{
			Message: "non-streaming not supported",
			Type:    "invalid_request_error",
		})
		return
	}

	// Header wins over body for agent selection so OpenAI-compatible clients
	// can pin the agent without touching the JSON body.
	agent := req.Agent
	if h := r.Header.Get("X-Harness-Agent"); h != "" {
		agent = h
	}

	modelEcho := req.Model
	if modelEcho == "" {
		modelEcho = "harness"
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSONError(w, http.StatusInternalServerError, apiErrorBody{
			Message: "streaming unsupported",
			Type:    "server_error",
		})
		return
	}

	// Mint the request id up front so prompt assembly, queue dispatch,
	// and inference all log under the same correlation key. The id
	// doubles as the OpenAI-shaped chat completion id echoed in the
	// streamed chunks.
	reqID := fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	ctx := reqid.WithID(r.Context(), reqID)
	logger := s.logger.With(slog.String("request_id", reqID))

	assembled, err := s.asm.Assemble(ctx, agent, req.Messages)
	if err != nil {
		logger.Error("assemble failed", slog.String("agent", agent), slog.Any("err", err))
		writeJSONError(w, http.StatusInternalServerError, apiErrorBody{
			Message: err.Error(),
			Type:    "server_error",
		})
		return
	}

	// Buffer generously so the queue worker is never blocked by a slow client;
	// the HTTP response loop drains this at its own pace and client disconnect
	// cancels r.Context() which the queue honours via the Request.Ctx field.
	respCh := make(chan inference.Token, 64)

	qReq := queue.Request{
		Completion: inference.CompletionRequest{
			Model:       req.Model,
			Messages:    assembled,
			Temperature: req.Temperature,
			TopP:        req.TopP,
			MaxTokens:   req.MaxTokens,
		},
		Response: respCh,
		Ctx:      ctx,
	}

	if err := s.q.Enqueue(qReq); err != nil {
		if errors.Is(err, queue.ErrQueueFull) {
			writeJSONError(w, http.StatusTooManyRequests, apiErrorBody{
				Message: "queue at capacity",
				Type:    "rate_limit_error",
				Code:    "queue_full",
			})
			return
		}
		if errors.Is(err, queue.ErrStopped) {
			writeJSONError(w, http.StatusServiceUnavailable, apiErrorBody{
				Message: "queue is shutting down",
				Type:    "server_error",
				Code:    "queue_stopped",
			})
			return
		}
		logger.Error("enqueue failed", slog.Any("err", err))
		writeJSONError(w, http.StatusInternalServerError, apiErrorBody{
			Message: err.Error(),
			Type:    "server_error",
		})
		return
	}

	// Past the point of no return: headers must flush now so the client sees
	// the stream even before the first token arrives from llama-server.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// Mint one session per call so each API request can land an episode in git.
	// External clients do not currently pin an API session id across requests.
	var sess Session
	if s.rec != nil {
		sess = s.rec.Start(agent)
		// Append the request's user-side messages so the eventual
		// summary captures what was asked.
		for _, m := range req.Messages {
			if m.Role == "" || m.Content == "" {
				continue
			}
			if err := s.rec.Append(sess.ID, m.Role, m.Content); err != nil {
				logger.Warn("session append (request)", slog.Any("err", err))
			}
		}
	}

	s.streamTokensWithSession(ctx, w, flusher, respCh, reqID, modelEcho, sess)
}

// streamTokensWithSession is streamTokens plus an optional Session
// that receives the assistant's joined content as a single Append once
// the stream ends. Pass a zero-value Session to skip recording.
func (s *Server) streamTokensWithSession(ctx context.Context, w http.ResponseWriter, flusher http.Flusher, respCh <-chan inference.Token, reqID, modelEcho string, sess Session) {
	stopReason := "stop"
	var assistant strings.Builder
	if s.rec != nil && sess.ID != "" {
		defer s.finalizeSession(context.WithoutCancel(ctx), sess, &assistant)
	}
	for tok := range respCh {
		if tok.Err != nil {
			s.logger.Error("token stream error", slog.String("id", reqID), slog.Any("err", tok.Err))
			writeSSEError(w, flusher, tok.Err.Error())
			return
		}
		if tok.Done {
			break
		}
		if tok.Content == "" {
			continue
		}
		assistant.WriteString(tok.Content)
		chunk := chatChunk{
			ID:      reqID,
			Object:  "chat.completion.chunk",
			Created: time.Now().Unix(),
			Model:   modelEcho,
			Choices: []chunkChoice{{
				Index: 0,
				Delta: chunkDelta{Content: tok.Content},
			}},
		}
		if writeSSEChunk(w, flusher, chunk) != nil {
			return
		}
	}

	// Final chunk: empty delta + finish_reason=stop, then the canonical
	// [DONE] sentinel so OpenAI-shaped clients know the stream is closed.
	final := chatChunk{
		ID:      reqID,
		Object:  "chat.completion.chunk",
		Created: time.Now().Unix(),
		Model:   modelEcho,
		Choices: []chunkChoice{{
			Index:        0,
			Delta:        chunkDelta{},
			FinishReason: &stopReason,
		}},
	}
	if writeSSEChunk(w, flusher, final) != nil {
		return
	}
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	flusher.Flush()

}

func (s *Server) finalizeSession(ctx context.Context, sess Session, assistant *strings.Builder) {
	if assistant != nil && assistant.Len() > 0 {
		if err := s.rec.Append(sess.ID, "assistant", assistant.String()); err != nil {
			s.logger.Warn("session append (assistant)", slog.Any("err", err))
		}
	}
	if err := s.rec.Save(ctx, sess.ID); err != nil {
		s.logger.Warn("session save (api)", slog.Any("err", err))
	}
	s.rec.End(sess.ID)
}

// modelsResponse is the /v1/models payload.
type modelsResponse struct {
	Object string      `json:"object"`
	Data   []modelInfo `json:"data"`
}

type modelInfo struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// handleModels is GET /v1/models. It returns the harness model alias used
// by OpenAI-compatible clients for discovery.
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, apiErrorBody{
			Message: "method not allowed",
			Type:    "invalid_request_error",
		})
		return
	}
	resp := modelsResponse{
		Object: "list",
		Data: []modelInfo{{
			ID:      "harness",
			Object:  "model",
			Created: s.startTime.Unix(),
			OwnedBy: "harness",
		}},
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// writeJSONError writes status + apiError envelope. Used before any streaming
// header is set, i.e. the request has not yet transitioned into SSE mode.
func writeJSONError(w http.ResponseWriter, status int, body apiErrorBody) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(apiError{Error: body})
}

// writeSSEChunk JSON-encodes chunk and emits a single SSE data event. Returns
// the first write error so callers can bail out of the stream.
func writeSSEChunk(w http.ResponseWriter, flusher http.Flusher, chunk chatChunk) error {
	payload, err := json.Marshal(chunk)
	if err != nil {
		return fmt.Errorf("api: marshal chunk: %w", err)
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

// writeSSEError emits an OpenAI-shaped error object inside the SSE stream.
// Called after the 200 OK has already gone out, so we can't change the
// status code anymore.
func writeSSEError(w http.ResponseWriter, flusher http.Flusher, msg string) {
	payload, err := json.Marshal(apiError{Error: apiErrorBody{
		Message: msg,
		Type:    "server_error",
	}})
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(w, "data: %s\n\n", payload)
	flusher.Flush()
}
