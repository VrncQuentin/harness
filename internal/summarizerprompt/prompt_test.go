package summarizerprompt

import (
	"strings"
	"testing"
)

func TestDefaultPromptIsUsable(t *testing.T) {
	if strings.TrimSpace(Default) == "" {
		t.Fatal("Default prompt must not be empty")
	}
	for _, want := range []string{"third-person summary", "Do not include the conversation verbatim"} {
		if !strings.Contains(Default, want) {
			t.Fatalf("Default prompt missing %q", want)
		}
	}
}
