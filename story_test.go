package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/regiellis/hyperchi/hyperchi"
)

func storyFixture() storyProject {
	return storyProject{ID: "project", Title: "Midnight delivery", Genre: "Action mystery", Episodes: []storyEpisode{{ID: "episode", Title: "The package", Shots: []storyShot{{ID: "shot-a", Scene: "Rooftop", Title: "The arrival", Seconds: 5}, {ID: "shot-b", Scene: "Rooftop", Title: "The handoff", Seconds: 7}}}}}
}
func TestStoryEditsPreserveConcurrentShots(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	if err := st.addStory(ctx, storyFixture()); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for _, id := range []string{"shot-a", "shot-b"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := st.editStory(ctx, "project", func(p *storyProject) error {
				e, err := storyEpisodeAt(p, "episode")
				if err != nil {
					return err
				}
				sh, err := storyShotAt(e, id)
				if err != nil {
					return err
				}
				sh.Prompt = "saved " + id
				return nil
			})
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	p, err := st.storyProject(ctx, "project")
	if err != nil {
		t.Fatal(err)
	}
	for _, sh := range p.Episodes[0].Shots {
		if sh.Prompt != "saved "+sh.ID {
			t.Fatalf("lost shot edit: %+v", sh)
		}
	}
	if _, _, _, err := st.storyContext(ctx, "project", "other", "shot-a"); err == nil {
		t.Fatal("accepted shot from wrong episode")
	}
}
func TestStoryShotActions(t *testing.T) {
	w := newToolsWeb(t)
	ctx := context.Background()
	if err := w.st.addStory(ctx, storyFixture()); err != nil {
		t.Fatal(err)
	}
	w.app.Post("/projects/{project}/edit", w.storyEdit)
	post := func(values url.Values) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/projects/project/edit", strings.NewReader(values.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		w.app.ServeHTTP(rec, req)
		return rec
	}
	bad := post(url.Values{"action": {"shot-edit"}, "episode": {"episode"}, "shot": {"shot-a"}, "title": {"Bad"}, "seconds": {"0"}})
	if bad.Code != 400 {
		t.Fatalf("accepted invalid duration: %d", bad.Code)
	}
	good := post(url.Values{"action": {"shot-edit"}, "episode": {"episode"}, "shot": {"shot-a"}, "title": {"Arrival"}, "seconds": {"8"}, "prompt": {"Wide shot, rainy rooftop"}, "scene": {"Rooftop"}})
	if good.Code < 300 || good.Code >= 400 {
		t.Fatalf("save: %d %s", good.Code, good.Body.String())
	}
	moved := post(url.Values{"action": {"shot-move"}, "episode": {"episode"}, "shot": {"shot-b"}, "direction": {"up"}})
	if moved.Code < 300 || moved.Code >= 400 {
		t.Fatal(moved.Code)
	}
	p, err := w.st.storyProject(ctx, "project")
	if err != nil {
		t.Fatal(err)
	}
	if p.Episodes[0].Shots[0].ID != "shot-b" || p.Episodes[0].Shots[1].Seconds != 8 {
		t.Fatalf("%+v", p)
	}
	deleted := post(url.Values{"action": {"shot-delete"}, "episode": {"episode"}, "shot": {"shot-b"}})
	if deleted.Code < 300 || deleted.Code >= 400 {
		t.Fatal(deleted.Code)
	}
	p, err = w.st.storyProject(ctx, "project")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Episodes[0].Shots) != 1 || p.Episodes[0].Shots[0].ID != "shot-a" {
		t.Fatalf("%+v", p)
	}
}
func TestStoryTemplatesRenderEmptyAndPopulated(t *testing.T) {
	w := newToolsWeb(t)
	p := storyFixture()
	for _, data := range []hyperchi.H{
		{"project": storyProject{}, "runtime": runtimeView{}},
		{"project": p, "runtime": runtimeView{}, "episodes": []episodeCard{{storyEpisode: p.Episodes[0], Number: 1}}, "assets": []assetCard{{storyAsset: storyAsset{ID: "maya", Name: "Maya", Kind: "character"}}}, "tab": "world"},
	} {
		if _, err := w.app.RenderToString("story-home", data); err != nil {
			t.Fatal(err)
		}
	}
	sh := shotCard{storyShot: p.Episodes[0].Shots[0], Number: 1, Selected: true}
	html, err := w.app.RenderToString("story-workspace", hyperchi.H{"project": p, "episode": p.Episodes[0], "shots": []shotCard{sh}, "selected": sh, "scenes": []string{"Rooftop"}, "duration": 5})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(html), "episode-timeline") || !strings.Contains(string(html), "shot=shot-a") {
		t.Fatal("workspace lost timeline or shot context")
	}
}

func TestStoryToolsOnlyAllowSameOriginFrames(t *testing.T) {
	w := newToolsWeb(t)
	w.app.Get("/frame", func(c *hyperchi.Context) error {
		c.Response().Header().Set("X-Frame-Options", "DENY")
		c.Response().Header().Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'; form-action 'self'")
		storyEmbedded(c)
		return c.HTML("tool")
	})
	for _, embedded := range []bool{false, true} {
		target := "/frame"
		if embedded {
			target += "?embedded=1"
		}
		rec := httptest.NewRecorder()
		w.app.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
		frame, ancestor := "DENY", "frame-ancestors 'none'"
		if embedded {
			frame, ancestor = "SAMEORIGIN", "frame-ancestors 'self'"
		}
		if rec.Header().Get("X-Frame-Options") != frame {
			t.Fatal(rec.Header())
		}
		policy := rec.Header().Get("Content-Security-Policy")
		for _, want := range []string{"default-src 'self'", "form-action 'self'", ancestor} {
			if !strings.Contains(policy, want) {
				t.Fatal(policy)
			}
		}
	}
}
