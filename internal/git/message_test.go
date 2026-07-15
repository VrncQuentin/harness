package git

import "testing"

func TestBuildMessage_Deterministic(t *testing.T) {
	tags := map[string]string{"type": "episode", "agent": "coder"}
	want := "[agent:coder] [type:episode] first session summary"
	if got := BuildMessage(tags, "first session summary"); got != want {
		t.Fatalf("BuildMessage() = %q, want %q", got, want)
	}
	for i := 0; i < 20; i++ {
		if got := BuildMessage(tags, "first session summary"); got != want {
			t.Fatalf("BuildMessage() iteration %d = %q, want %q", i, got, want)
		}
	}
}

func TestBuildMessage_DropsEmptyTags(t *testing.T) {
	got := BuildMessage(map[string]string{"agent": "coder", "": "bad", "type": ""}, "summary")
	want := "[agent:coder] summary"
	if got != want {
		t.Fatalf("BuildMessage() = %q, want %q", got, want)
	}
}

func TestBuildMessage_AllowsPlainSummary(t *testing.T) {
	got := BuildMessage(nil, "summary only")
	if got != "summary only" {
		t.Fatalf("BuildMessage(nil) = %q", got)
	}
}
