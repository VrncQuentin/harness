package git

import (
	"reflect"
	"testing"
)

func TestBuildMessage_Deterministic(t *testing.T) {
	cases := []struct {
		name    string
		tags    map[string]string
		summary string
		want    string
	}{
		{
			name:    "no tags",
			tags:    map[string]string{},
			summary: "hello world",
			want:    "hello world",
		},
		{
			name:    "single tag",
			tags:    map[string]string{"agent": "coder"},
			summary: "wrote an episode",
			want:    "[agent:coder] wrote an episode",
		},
		{
			name:    "two tags sorted by key",
			tags:    map[string]string{"type": "episode", "agent": "coder"},
			summary: "hello",
			want:    "[agent:coder] [type:episode] hello",
		},
		{
			name:    "empty key dropped",
			tags:    map[string]string{"": "x", "k": "v"},
			summary: "s",
			want:    "[k:v] s",
		},
		{
			name:    "empty value dropped",
			tags:    map[string]string{"k": "", "ok": "ay"},
			summary: "s",
			want:    "[ok:ay] s",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got1 := BuildMessage(tc.tags, tc.summary)
			if got1 != tc.want {
				t.Fatalf("first call: got %q, want %q", got1, tc.want)
			}
			// Run several times: map iteration order is randomised, so
			// repeated calls cover the determinism guarantee.
			for i := 0; i < 8; i++ {
				if g := BuildMessage(tc.tags, tc.summary); g != got1 {
					t.Fatalf("iteration %d: %q != %q", i, g, got1)
				}
			}
		})
	}
}

func TestParseMessage(t *testing.T) {
	cases := []struct {
		name        string
		input       string
		wantTags    map[string]string
		wantSummary string
	}{
		{
			name:        "plain text without brackets",
			input:       "just a summary",
			wantTags:    map[string]string{},
			wantSummary: "just a summary",
		},
		{
			name:        "single tag",
			input:       "[agent:coder] hello",
			wantTags:    map[string]string{"agent": "coder"},
			wantSummary: "hello",
		},
		{
			name:        "two tags",
			input:       "[agent:coder] [type:episode] wrote an episode",
			wantTags:    map[string]string{"agent": "coder", "type": "episode"},
			wantSummary: "wrote an episode",
		},
		{
			name:        "value containing spaces and colons",
			input:       "[note:hello: world] body",
			wantTags:    map[string]string{"note": "hello: world"},
			wantSummary: "body",
		},
		{
			name:        "unclosed leading bracket falls through",
			input:       "[bad summary",
			wantTags:    map[string]string{},
			wantSummary: "[bad summary",
		},
		{
			name:        "missing colon in leading bracket falls through",
			input:       "[broken] still",
			wantTags:    map[string]string{},
			wantSummary: "[broken] still",
		},
		{
			name:        "later brackets are not parsed",
			input:       "[agent:coder] body with [later:tag]",
			wantTags:    map[string]string{"agent": "coder"},
			wantSummary: "body with [later:tag]",
		},
		{
			name:        "empty summary after tags",
			input:       "[agent:coder] ",
			wantTags:    map[string]string{"agent": "coder"},
			wantSummary: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tags, summary := ParseMessage(tc.input)
			if !reflect.DeepEqual(tags, tc.wantTags) {
				t.Errorf("tags: got %v, want %v", tags, tc.wantTags)
			}
			if summary != tc.wantSummary {
				t.Errorf("summary: got %q, want %q", summary, tc.wantSummary)
			}
		})
	}
}

func TestParseMessage_RoundTripsBuildMessage(t *testing.T) {
	cases := []struct {
		name    string
		tags    map[string]string
		summary string
	}{
		{name: "no tags", tags: map[string]string{}, summary: "hello world"},
		{name: "single tag", tags: map[string]string{"agent": "coder"}, summary: "did a thing"},
		{name: "multiple tags", tags: map[string]string{"agent": "reviewer", "type": "episode"}, summary: "reviewed"},
		{name: "value with spaces", tags: map[string]string{"note": "hello world"}, summary: "body"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			built := BuildMessage(tc.tags, tc.summary)
			gotTags, gotSummary := ParseMessage(built)
			if !reflect.DeepEqual(gotTags, tc.tags) {
				t.Errorf("tags after round-trip: got %v, want %v", gotTags, tc.tags)
			}
			if gotSummary != tc.summary {
				t.Errorf("summary after round-trip: got %q, want %q", gotSummary, tc.summary)
			}
		})
	}
}
