package session

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/VrncQuentin/harness/internal/inference"
	"github.com/VrncQuentin/harness/internal/summarizerprompt"
)

// Summarizer turns a conversation into a markdown summary by issuing a
// fresh inference call. The call deliberately bypasses the prompt
// assembler (no recency layer, no persona) and the request queue (the
// summarizer does not compete with user-facing requests for queue
// slots) - the goal is a clean factual summary of the conversation
// alone, scheduled with its own concurrency.
type Summarizer struct {
	client  inference.Client
	prompt  SummarizerPromptFunc
	timeout time.Duration
}

// NewSummarizer wires a Summarizer with the given inference client and
// system-prompt fetcher. timeout caps how long Summarize waits for
// tokens to drain; pass 0 to use the default.
func NewSummarizer(client inference.Client, prompt SummarizerPromptFunc, timeout time.Duration) *Summarizer {
	if prompt == nil {
		prompt = func() string { return "" }
	}
	if timeout <= 0 {
		timeout = summarizerTimeout
	}
	return &Summarizer{
		client:  client,
		prompt:  prompt,
		timeout: timeout,
	}
}

// Summarize sends conversation through the inference client and
// returns the joined token stream as a markdown body. Any
// error - empty conversation, inference failure, context cancelled, or
// an explicit error token mid-stream - is returned without falling
// back to "no summary": refusing the save is better than committing
// garbage.
func (s *Summarizer) Summarize(ctx context.Context, conversation []inference.Message) (string, error) {
	if len(conversation) == 0 {
		return "", errors.New("session: summarize: conversation is empty")
	}

	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	system := strings.TrimSpace(s.prompt())
	if system == "" {
		system = summarizerprompt.Default
	}

	msgs := make([]inference.Message, 0, len(conversation)+1)
	msgs = append(msgs, inference.Message{Role: "system", Content: system})
	msgs = append(msgs, conversation...)

	tokens, err := s.client.Complete(ctx, inference.CompletionRequest{
		Messages: msgs,
		Stream:   true,
	})
	if err != nil {
		return "", fmt.Errorf("session: summarize: %w", err)
	}

	var out strings.Builder
	for tok := range tokens {
		if tok.Err != nil {
			return "", fmt.Errorf("session: summarize: %w", tok.Err)
		}
		if tok.Done {
			break
		}
		if tok.Content == "" {
			continue
		}
		out.WriteString(tok.Content)
	}
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("session: summarize: %w", err)
	}
	body := strings.TrimSpace(out.String())
	if body == "" {
		return "", errors.New("session: summarize: empty response from model")
	}
	return body, nil
}
