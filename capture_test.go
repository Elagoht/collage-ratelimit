package ratelimit_test

import (
	"context"
	"net/http"
	"testing"
	"testing/fstest"

	ratelimit "github.com/Elagoht/collage-ratelimit"
	"github.com/Elagoht/collage/pkg/collage"
)

// buildReader reads a finished build, as elagoht/deploy does; with one
// registered, the build asks the handler for every file to capture its headers.
type buildReader struct{ files []collage.BuiltFile }

func (*buildReader) Name() string                             { return "test/buildreader" }
func (*buildReader) Version() string                          { return "0" }
func (*buildReader) Init(context.Context, collage.Host) error { return nil }
func (*buildReader) Shutdown(context.Context) error           { return nil }
func (b *buildReader) OnBuildFinished(_ context.Context, ev *collage.BuildFinishedEvent) error {
	b.files = ev.Files
	return nil
}

// A static build's header capture is not a client: a rule that limits GET
// requests neither refuses the build's own requests nor puts its RateLimit
// headers in what the build hands a deploy adapter.
func TestBuildCaptureIsNotLimited(t *testing.T) {
	reader := &buildReader{}
	app, err := collage.New(&collage.Config{
		Server: collage.ServerConfig{Host: "localhost", Port: 3000},
		Template: collage.TemplateConfig{FS: fstest.MapFS{
			"t/p.html": {Data: []byte(`<main>home</main>`)},
		}, Root: "t"},
		Plugins: []collage.Plugin{
			ratelimit.New(ratelimit.Options{Rules: []ratelimit.Rule{{Methods: []string{http.MethodGet}, Rate: 0.001, Burst: 1}}}),
			reader,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/", "/a", "/b"} {
		page := collage.NewPage("p"+path).WithContent(collage.NewFragment("home", "p.html").Build()).WithPath("en", path).Build()
		if err := app.RegisterPage(page); err != nil {
			t.Fatal(err)
		}
	}
	builder, err := collage.NewBuilder(app, collage.BuildOptions{OutDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	report, err := builder.Build(context.Background())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	for _, f := range report.Findings {
		t.Errorf("finding: %s %s: %s", f.Rule, f.Path, f.Message)
	}
	captured := 0
	for _, f := range reader.files {
		if !f.Captured {
			continue
		}
		captured++
		if f.Status != http.StatusOK {
			t.Errorf("%s: status %d", f.Path, f.Status)
		}
		if f.Headers.Get("RateLimit-Limit") != "" {
			t.Errorf("%s: the capture carries RateLimit headers: %v", f.Path, f.Headers)
		}
	}
	if captured != 3 {
		t.Fatalf("%d pages captured, want 3", captured)
	}
}
