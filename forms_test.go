package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/regiellis/hyperchi/hyperchi"
	"github.com/regiellis/hyperchi/hyperchi/config"
)

// TestFormsBindAsThePagesSendThem binds each form struct from a request
// shaped like the page's, so a validate tag the binder does not know (it
// has no omitempty) fails here rather than as a 500 in the browser.
func TestFormsBindAsThePagesSendThem(t *testing.T) {
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
	ctx := func(form url.Values) *hyperchi.Context {
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		return app.NewContext(httptest.NewRecorder(), req)
	}
	long := url.Values{"start": {""}, "refs": {"a", "b"}, "shot_prompt": {"one", "two"}, "shot_seconds": {"5", "8"},
		"shot_join": {"cut", "continue"}, "shot_keep": {"", ""}, "shot_end": {"", "img"}, "shape": {"portrait"},
		"size": {"draft"}, "quality": {"turbo"}, "cast_view": {"front"}, "ref_size": {"match"}, "soundtrack": {""},
		"soundtrack_mode": {"replace"}, "from": {""}}
	in, err := hyperchi.Bind[longForm](ctx(long))
	if err != nil {
		t.Fatalf("the long video form does not bind: %v", err)
	}
	if len(in.Prompts) != 2 || in.Ends[1] != "img" || in.SoundtrackMode != "replace" {
		t.Errorf("bound %+v", in)
	}
	run := url.Values{"mode": {"motion"}, "engine": {"x"}, "source": {"s"}, "refs": {"a"}, "prompt": {"p"},
		"resolution": {"480p"}, "versions": {"1"}, "style": {""}, "pin": {""}}
	if _, err := hyperchi.Bind[runForm](ctx(run)); err != nil {
		t.Errorf("the studio form does not bind: %v", err)
	}
	ask := url.Values{"prompt": {"p"}, "engine": {"x"}, "mode": {"long"}, "refs": {"a"}, "before": {"b"}, "join": {"cut"}}
	if _, err := hyperchi.Bind[llmAsk](ctx(ask)); err != nil {
		t.Errorf("the language model form does not bind: %v", err)
	}
}
