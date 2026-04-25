// Package reqid threads a per-request identifier through context.Context
// so log entries from the api handler, prompt assembler, queue, and
// inference client can be grouped after the fact. The api handler is the
// canonical producer: it generates an OpenAI-shaped chatcmpl-* id and
// attaches it before calling downstream code.
package reqid

import "context"

// ctxKey is unexported so the only way to attach a request id is via
// WithID, preventing accidental key collisions.
type ctxKey struct{}

// WithID returns a copy of ctx that carries id. Empty ids are stored
// verbatim; readers can decide whether to log a missing id.
func WithID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// From returns the request id stored in ctx, or "" if none was set.
func From(ctx context.Context) string {
	v, _ := ctx.Value(ctxKey{}).(string)
	return v
}
