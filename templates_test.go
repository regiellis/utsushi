package main

import (
	"html/template"
	"path/filepath"
	"strings"
	"testing"

	"github.com/regiellis/hyperchi/hyperchi"
	"github.com/regiellis/hyperchi/hyperchi/config"
)

// TestTemplatesParseAndRender loads every template as main does and renders
// the cards and rows whose branches differ by mode, so a template the parser
// refuses fails here rather than at startup.
func TestTemplatesParseAndRender(t *testing.T) {
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
	long := runView{ID: "l", Mode: modeLong, EngineName: "MiniMax H3", Prompt: "p", Shots: 3, Resolution: "480×864",
		Status: runRunning, StatusLabel: "Generating", Active: true, Aspect: "0.5556"}
	html, err := app.RenderToString("run-card-body", long)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(html), `src="#t=0.1"`) || strings.Contains(string(html), "/media/#") {
		t.Error("a long video with no source should render no source video")
	}
	if !strings.Contains(string(html), "Long video · MiniMax H3") {
		t.Error("the card should say it is a long video")
	}
	motion := runView{ID: "m", Mode: modeMotion, EngineName: "Wan", Prompt: "p", SourceURL: "/media/s", StudioURL: "/",
		ResultURL: "/media/r", Status: runSucceeded, StatusLabel: "Done", Succeeded: true, Aspect: "0.5625"}
	html, err = app.RenderToString("run-card-body", motion)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Reuse prompt", "Compare", `/media/s#t=0.1`} {
		if !strings.Contains(string(html), want) {
			t.Errorf("a finished motion card lacks %q", want)
		}
	}
	editor, err := app.RenderToString("clip-editor", clipEditorView{ID: "c", Label: "dance.mp4", URL: "/media/c", Seconds: 12,
		Strip: true, Cuts: "[3.000,8.000]", Detected: "[3.000]"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(editor), `data-cuts="[3.000,8.000]"`) || !strings.Contains(string(editor), `/media/c/split`) {
		t.Error("the editor should carry its cuts and post its split")
	}
	if _, err := app.RenderToString("clips-split", hyperchi.H{"clips": []mediaView{{ID: "p", Label: "part", URL: "/media/p", Ready: true}}}); err != nil {
		t.Fatal(err)
	}
	shot, err := app.RenderToString("long-shot", hyperchi.H{"shot": blankShot, "seconds": longSeconds, "from": "",
		"page": pageFlags{Improve: true, Suggest: true}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(shot), `data-shot-llm="improve"`) || !strings.Contains(string(shot), `data-shot-llm="suggest"`) {
		t.Error("a shot row should offer Improve and Suggest when they are on")
	}
	if !strings.Contains(string(shot), `value="10" checked`) || !strings.Contains(string(shot), `name="shot_prompt"`) {
		t.Errorf("a shot row should default to 10 s and carry its prompt field:\n%s", shot)
	}
}
