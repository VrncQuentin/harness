package assets

import (
	"html/template"
	"io/fs"
	"strings"
	"testing"
)

// TestTemplatesParse parses every embedded template file individually and as a
// single set so a broken {{...}} directive fails CI rather than at runtime when
// NewServer panics on first request.
func TestTemplatesParse(t *testing.T) {
	entries, err := fs.ReadDir(TemplateFS, "templates")
	if err != nil {
		t.Fatalf("read templates dir: %v", err)
	}

	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".html") {
			continue
		}
		names = append(names, "templates/"+e.Name())
	}
	if len(names) == 0 {
		t.Fatal("no templates found under templates/*.html — embed pattern broken?")
	}

	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			if _, err := template.New("").ParseFS(TemplateFS, name); err != nil {
				t.Fatalf("parse %s: %v", name, err)
			}
		})
	}

	if _, err := template.New("").ParseFS(TemplateFS, names...); err != nil {
		t.Fatalf("parse all templates as a set: %v", err)
	}
}

// TestStaticAssetsPresent guards against an //go:embed pattern that silently
// stops matching the files the UI serves at /static/*.
func TestStaticAssetsPresent(t *testing.T) {
	want := []string{"static/app.css", "static/app.js"}
	for _, name := range want {
		if _, err := fs.Stat(StaticFS, name); err != nil {
			t.Errorf("missing static asset %s: %v", name, err)
		}
	}
}

func TestAppJSUsesMultiplexedEventsOnly(t *testing.T) {
	b, err := fs.ReadFile(StaticFS, "static/app.js")
	if err != nil {
		t.Fatalf("read app.js: %v", err)
	}
	body := string(b)
	if !strings.Contains(body, "new EventSource('/events')") {
		t.Fatal("app.js does not subscribe to multiplexed /events stream")
	}
	if strings.Contains(body, "/logs/") {
		t.Fatal("app.js still references removed /logs/* streams")
	}
}
