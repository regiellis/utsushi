package main

import (
	"context"
	"html/template"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/regiellis/hyperchi/hyperchi"
	"github.com/regiellis/hyperchi/hyperchi/config"
)

func newToolsWeb(t *testing.T) *web {
	t.Helper()
	cfg := config.FromEnv()
	cfg.Database.Name = filepath.Join(t.TempDir(), "t.db")
	app, err := hyperchi.NewWithConfigE(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := app.Close(); err != nil {
			t.Error(err)
		}
	})
	app.RegisterFilters(template.FuncMap{"posters": func() bool { return false }})
	if err := app.LoadTemplatesFS(templateFiles, "templates/**/*.tmpl"); err != nil {
		t.Fatal(err)
	}
	fr, err := newFrames(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	st, lib := newTestStore(t), newTestLibrary(t)
	rn := newRunner(st, lib, &engines{}, nil, &recorder{})
	t.Cleanup(rn.shutdown)
	rn.frames = fr
	rn.tools = &toolKit{models: newModels(t.TempDir())}
	return &web{app: app, st: st, lib: lib, run: rn, frames: fr}
}

// TestToolFormRefusesWhatItCannotMake posts the Tools form the way the
// page does and checks each refusal reaches the form, never a job.
func TestToolFormRefusesWhatItCannotMake(t *testing.T) {
	w := newToolsWeb(t)
	if !w.frames.available() {
		t.Skip("ffmpeg is not installed")
	}
	ctx := context.Background()
	clip := Media{ID: newID(), Kind: kindVideo, Origin: originUpload, File: "c.mp4", Label: "walk.mp4",
		Status: mediaReady, CreatedAt: now(), Width: 480, Height: 854, Seconds: 2}
	unsized := Media{ID: newID(), Kind: kindVideo, Origin: originUpload, File: "u.mp4", Label: "old.mp4",
		Status: mediaReady, CreatedAt: now(), Seconds: 2}
	img := Media{ID: newID(), Kind: kindImage, Origin: originUpload, File: "i.png", Label: "face.png",
		Status: mediaReady, CreatedAt: now(), Width: 512, Height: 512}
	for _, m := range []Media{clip, unsized, img} {
		if err := w.st.addMedia(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	post := func(form url.Values) (int, string) {
		req := httptest.NewRequest(http.MethodPost, "/tools/run", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		if err := w.runToolHandler(w.app.NewContext(rec, req)); err != nil {
			t.Fatal(err)
		}
		return rec.Code, rec.Body.String()
	}
	for _, c := range []struct {
		name string
		form url.Values
		want string
	}{
		{"no tool", url.Values{"tool": {"x"}, "source": {clip.ID}}, "Pick a tool"},
		{"no clip", url.Values{"tool": {"depth"}, "source": {"nope"}, "model": {"frame"}, "look": {"depth"}}, "Pick a clip"},
		{"a clip as the image", url.Values{"tool": {"depth"}, "source_kind": {"image"}, "image": {clip.ID}, "look": {"depth"}}, "Pick an image"},
		{"no look", url.Values{"tool": {"depth"}, "source": {clip.ID}, "model": {"frame"}, "look": {"sepia"}}, "Pick at least one map"},
		{"no size", url.Values{"tool": {"depth"}, "source": {unsized.ID}, "look": {"depth"}}, "size of old.mp4 is unknown"},
		{"no change", url.Values{"tool": {"conform"}, "source": {clip.ID}, "rate": {"keep"}, "size": {"keep"}, "shape": {"keep"}}, "Change the frame rate, size, or shape"},
		{"no time", url.Values{"tool": {"frame"}, "source": {clip.ID}, "at": {"soon"}}, "Set the time to grab"},
		{"an image to conform", url.Values{"tool": {"conform"}, "source_kind": {"image"}, "image": {img.ID}, "rate": {"24"}}, "Conform takes a clip"},
	} {
		code, body := post(c.form)
		if code != http.StatusUnprocessableEntity || !strings.Contains(body, c.want) {
			t.Errorf("%s: %d %q, want 422 with %q", c.name, code, body, c.want)
		}
	}
	made, err := w.st.listOrigin(ctx, originTool, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(made) != 0 {
		t.Fatalf("a refused form made %d items", len(made))
	}
}

func TestToolCardsRenderEachState(t *testing.T) {
	w := newToolsWeb(t)
	for _, c := range []struct {
		name string
		view toolItemView
		want []string
	}{
		{"making", toolItemView{ID: "a", Label: "walk.mp4 · Depth sharp", Pending: true, Note: "Depth · frame 12 of 48 · 25%", Percent: 25, Aspect: "0.5625", SourceURL: "/media/s", SourceKind: "video"},
			[]string{"Making", `aria-valuenow="25"`, "frame 12 of 48", "/media/s#t=0.1"}},
		{"ready clip", toolItemView{ID: "b", URL: "/media/b", Label: "walk.mp4 · Normals sharp", Ready: true, Kind: "video", ToolName: "Depth maps", Aspect: "0.5625", SourceURL: "/media/s", SourceKind: "video"},
			[]string{`title="Depth maps"`, `<video src="/media/b"`, "download", `hx-delete="/media/b"`, `aria-label="Source"`}},
		{"ready image", toolItemView{ID: "c", URL: "/media/c", Label: "face.png · Depth sharp", Ready: true, Kind: "image", Aspect: "1", SourceURL: "/media/i", SourceKind: "image"},
			[]string{`<img src="/media/c"`, `data-lightbox="image"`}},
		{"ready audio", toolItemView{ID: "s", URL: "/media/s", Label: "walk.mp4 · sound", Ready: true, Kind: "audio", Aspect: "1"},
			[]string{`data-play-audio="/media/s"`, "card-audio"}},
		{"failed", toolItemView{ID: "d", Label: "x", Failed: true, Error: "ffmpeg: exit status 1", Aspect: "1"},
			[]string{"Failed", `class="error-mark"`, `data-error="ffmpeg: exit status 1"`, `hx-delete="/media/d"`}},
	} {
		html, err := w.app.RenderToString("tool-card", c.view)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		for _, want := range c.want {
			if !strings.Contains(string(html), want) {
				t.Errorf("%s card lacks %q:\n%s", c.name, want, html)
			}
		}
	}
	// Errors stay behind an amber mark: the message is its tooltip and
	// accessible name, never text over the footage.
	failedRun, err := w.app.RenderToString("run-card-body", runView{ID: "r", Failed: true, Status: runFailed, StatusLabel: "Failed",
		ErrorTitle: "The provider refused the clip", ErrorHint: "It is longer than 10 s.", Aspect: "0.5625", CanRunAgain: true})
	if err != nil {
		t.Fatal(err)
	}
	failedTile, err := w.app.RenderToString("image-tile-body", mediaView{ID: "m", Failed: true, Error: "Out of memory on the GPU"})
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		html       string
		wants, not []string
	}{
		"run card": {string(failedRun), []string{`class="error-mark"`, `data-error="The provider refused the clip"`, `data-error-hint="It is longer than 10 s."`, "Run again"},
			[]string{">The provider refused the clip<", ">It is longer than 10 s.<"}},
		"image tile": {string(failedTile), []string{`class="error-mark"`, `data-error="Out of memory on the GPU"`}, []string{">Out of memory on the GPU<"}},
	} {
		for _, want := range c.wants {
			if !strings.Contains(c.html, want) {
				t.Errorf("failed %s lacks %q:\n%s", name, want, c.html)
			}
		}
		for _, not := range c.not {
			if strings.Contains(c.html, not) {
				t.Errorf("failed %s still prints %q as text", name, not)
			}
		}
	}
	if v := w.viewToolItem(context.Background(), Media{ID: "e", Origin: originTool, Status: mediaPending, Note: "Depth · frame 3 of 9 · 33%"}); v.Percent != 33 {
		t.Errorf("percent from the note = %d, want 33", v.Percent)
	}
}
