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
	want := []string{"static/app.css", "static/status.css", "static/chat.css", "static/task.css", "static/agents.css", "static/memory.css", "static/projects.css", "static/config.css", "static/htmx.min.js", "static/htmx-ext-sse.js"}
	for _, name := range want {
		if _, err := fs.Stat(StaticFS, name); err != nil {
			t.Errorf("missing static asset %s: %v", name, err)
		}
	}
}

func TestNoCustomAppJS(t *testing.T) {
	if _, err := fs.Stat(StaticFS, "static/app.js"); err == nil {
		t.Fatal("static/app.js should not be embedded; UI behavior must be server-rendered htmx/SSE")
	}
	b, err := fs.ReadFile(TemplateFS, "templates/layout.html")
	if err != nil {
		t.Fatalf("read layout: %v", err)
	}
	if strings.Contains(string(b), "app.js") {
		t.Fatal("layout still references app.js")
	}
}
