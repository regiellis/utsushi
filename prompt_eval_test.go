//go:build eval

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/regiellis/hyperchi/hyperchi/database"
)

// The prompt eval sends Suggest and Improve's real requests (promptAsk,
// writePrompt with its one retry) for
// the fixed cases in evals/prompts.json to a language model and grades each
// reply: the tag rules hold, nothing in Avoid appears (the clip's people's
// looks, which a video model copies when the prompt describes them, and a
// character sheet's layout), and
// one word of each Want group does (each reference's look). Each case runs
// UTSUSHI_EVAL_RUNS times (default 3), since replies vary.
//
// The cases name media in the local library (UTSUSHI_DATA, default data);
// the database is copied first, so the eval never writes to it. The model
// is UTSUSHI_EVAL_LLM (default llama.cpp at 127.0.0.1:9903/v1) and
// UTSUSHI_EVAL_MODEL (default ornith-vision). It fails when fewer than
// UTSUSHI_EVAL_MIN of all runs pass (default 0.8).

type evalCase struct {
	Name    string     `json:"name"`
	Feature string     `json:"feature"`
	Mode    string     `json:"mode"`
	Engine  string     `json:"engine"`
	Source  string     `json:"source"`
	Refs    []string   `json:"refs"`
	Draft   string     `json:"draft"`
	Before  string     `json:"before"`
	Style   string     `json:"style"`
	Join    string     `json:"join"`
	Avoid   []string   `json:"avoid"`
	Want    [][]string `json:"want"`
}

// copyFile copies src to dst, missing src allowed (a WAL file may not exist).
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	out, err := os.Create(dst)
	if err != nil {
		return errors.Join(err, in.Close())
	}
	_, err = io.Copy(out, in)
	return errors.Join(err, in.Close(), out.Close())
}

// grade lists what a prompt reply got wrong, empty when it passes.
func grade(c evalCase, sc llmScene, out string) []string {
	var bad []string
	if problem := sc.check(out); problem != "" {
		bad = append(bad, "tags: "+problem)
	}
	return append(bad, gradeWords(c, out)...)
}

// gradeWords checks a reply's words: nothing in Avoid, one of each Want.
func gradeWords(c evalCase, out string) []string {
	var bad []string
	lower := strings.ToLower(out)
	for _, w := range c.Avoid {
		if strings.Contains(lower, w) {
			bad = append(bad, "says what it should not: "+w)
		}
	}
	for _, group := range c.Want {
		found := false
		for _, w := range group {
			if strings.Contains(lower, w) {
				found = true
				break
			}
		}
		if !found {
			bad = append(bad, "misses a reference's look: "+strings.Join(group, " or "))
		}
	}
	return bad
}

func TestPromptEval(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("evals", "prompts.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cases []evalCase
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	runs, err := strconv.Atoi(envOr("UTSUSHI_EVAL_RUNS", "3"))
	if err != nil {
		t.Fatal(err)
	}
	least, err := strconv.ParseFloat(envOr("UTSUSHI_EVAL_MIN", "0.8"), 64)
	if err != nil {
		t.Fatal(err)
	}

	data := envOr("UTSUSHI_DATA", "data")
	tmp := t.TempDir()
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := copyFile(filepath.Join(data, "utsushi.db"+suffix), filepath.Join(tmp, "utsushi.db"+suffix)); err != nil {
			t.Fatal(err)
		}
	}
	cfg := database.DefaultDBConfig()
	cfg.Path = filepath.Join(tmp, "utsushi.db")
	db, err := database.NewDB(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	st, err := newStore(db)
	if err != nil {
		t.Fatal(err)
	}
	lib, err := openLibrary(filepath.Join(data, "media"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := lib.Close(); err != nil {
			t.Error(err)
		}
	})
	fr, err := newFrames(filepath.Join(tmp, "cache"))
	if err != nil {
		t.Fatal(err)
	}
	refs, err := newLocalRefs("http://127.0.0.1:1", "")
	if err != nil {
		t.Fatal(err)
	}
	w := &web{st: st, lib: lib, frames: fr,
		eng: &engines{list: append([]engine{newWan3Engine(refs, lib, func() string { return "" }),
			newLongEngine(refs, lib, nil, nil, fr)}, evalCloudEngines(lib)...)}}
	client, err := newLLM(LLMPrefs{On: true, Provider: "llamacpp",
		URLs:   map[string]string{"llamacpp": envOr("UTSUSHI_EVAL_LLM", "http://127.0.0.1:9903/v1")},
		Models: map[string]string{"llamacpp": envOr("UTSUSHI_EVAL_MODEL", "ornith-vision")}})
	if err != nil {
		t.Fatal(err)
	}

	// UTSUSHI_EVAL_CASES runs only the cases whose names hold one of its
	// comma-separated words, for a quick before-and-after on a few.
	var only []string
	for _, w := range strings.Split(os.Getenv("UTSUSHI_EVAL_CASES"), ",") {
		if w = strings.ToLower(strings.TrimSpace(w)); w != "" {
			only = append(only, w)
		}
	}
	passed, total := 0, 0
	for _, c := range cases {
		if len(only) > 0 && !slices.ContainsFunc(only, func(w string) bool { return strings.Contains(strings.ToLower(c.Name), w) }) {
			continue
		}
		ask := llmAsk{Prompt: c.Draft, Engine: c.Engine, Mode: c.Mode, Source: c.Source, Refs: c.Refs, Style: c.Style,
			Before: c.Before, Join: c.Join}
		if c.Feature == "describe" {
			// Describe is one reply with no tag rules: graded on words alone.
			system, user, img, err := w.describeAsk(context.Background(), ask)
			if err != nil {
				t.Fatalf("%s: %v", c.Name, err)
			}
			good := 0
			for i := range runs {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
				out, err := client.chat(ctx, system, user, []llmImage{img})
				cancel()
				if err != nil {
					t.Fatalf("%s, run %d: %v", c.Name, i+1, err)
				}
				bad := gradeWords(c, out)
				if len(bad) == 0 {
					good++
				}
				t.Logf("%s, run %d: %s\n    %s\n    %s", c.Name, i+1, map[bool]string{true: "PASS", false: "FAIL"}[len(bad) == 0],
					strings.Join(bad, "; "), out)
			}
			t.Logf("%s: %d of %d", c.Name, good, runs)
			passed += good
			total += runs
			continue
		}
		system, user, sc, err := w.promptAsk(context.Background(), c.Feature, ask)
		if err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}
		good := 0
		for i := range runs {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			out, err := writePrompt(ctx, client, system, user, sc)
			cancel()
			var bad []string
			var f failure
			switch {
			case errors.As(err, &f):
				// The app refuses this reply and keeps the visitor's prompt.
				bad = []string{"refused: " + f.msg}
			case err != nil:
				t.Fatalf("%s, run %d: %v", c.Name, i+1, err)
			default:
				bad = grade(c, sc, out)
			}
			if len(bad) == 0 {
				good++
			}
			t.Logf("%s, run %d: %s\n    %s\n    %s", c.Name, i+1, map[bool]string{true: "PASS", false: "FAIL"}[len(bad) == 0],
				strings.Join(bad, "; "), out)
		}
		t.Logf("%s: %d of %d", c.Name, good, runs)
		passed += good
		total += runs
	}
	score := float64(passed) / float64(total)
	t.Logf("score: %d of %d (%.0f%%)", passed, total, score*100)
	if score < least {
		t.Errorf("score %.2f is under %.2f", score, least)
	}
	fmt.Fprintf(os.Stderr, "prompt eval: %d of %d passed\n", passed, total)
}

// evalCloudEngines are the Seedance engines for Object Swap and Style cases:
// only their names, tags, and limits are read, so no Cloud link is needed.
func evalCloudEngines(lib *library) []engine {
	_, edit := newSeedanceEngines(&cloudLink{}, lib)
	tag := func(n int) string { return fmt.Sprintf("@Image %d", n) }
	return []engine{edit, newRestyler(edit, "seedance25-style", 8, tag)}
}
