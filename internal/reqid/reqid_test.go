package reqid

import (
	"context"
	"testing"
)

func TestFromReturnsEmptyWhenUnset(t *testing.T) {
	if got := From(context.Background()); got != "" {
		t.Fatalf("From unset = %q, want empty", got)
	}
}

func TestWithIDStoresRequestID(t *testing.T) {
	ctx := WithID(context.Background(), "chatcmpl-123")
	if got := From(ctx); got != "chatcmpl-123" {
		t.Fatalf("From = %q, want chatcmpl-123", got)
	}
}

func TestWithIDDoesNotMutateParentContext(t *testing.T) {
	parent := context.Background()
	child := WithID(parent, "req-1")
	if got := From(parent); got != "" {
		t.Fatalf("parent From = %q, want empty", got)
	}
	if got := From(child); got != "req-1" {
		t.Fatalf("child From = %q, want req-1", got)
	}
}

func TestWithIDStoresEmptyIDVerbatim(t *testing.T) {
	ctx := WithID(context.Background(), "")
	if got := From(ctx); got != "" {
		t.Fatalf("From empty id = %q, want empty", got)
	}
}
