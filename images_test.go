package main

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
)

// links are every [node, slot] reference in a graph, as from -> to.
func graphLinks(g map[string]any) [][2]string {
	var out [][2]string
	for id, raw := range g {
		n, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		in, _ := n["inputs"].(map[string]any)
		for name, v := range in {
			if l, ok := v.([]any); ok && len(l) == 2 {
				if from, ok := l[0].(string); ok {
					out = append(out, [2]string{from, id + "." + name})
				}
			}
		}
	}
	return out
}

func TestImageGraphsAreWhole(t *testing.T) {
	for _, e := range imageEngines {
		for _, refs := range [][]any{nil, {"a.png", "b.png"}} {
			if len(refs) > 0 && e.MaxRefs == 0 {
				continue
			}
			if len(refs) == 0 && e.MinRefs > 0 {
				continue
			}
			g := e.graph(imageJob{Prompt: "p", Aspect: "wide", Resolution: "1K", Refs: refs, Seed: 7})
			for _, l := range graphLinks(g) {
				if _, ok := g[l[0]]; !ok {
					t.Errorf("%s: %s links to missing node %s", e.ID, l[1], l[0])
				}
			}
			saves := 0
			for _, raw := range g {
				if raw.(map[string]any)["class_type"] == "SaveImage" {
					saves++
				}
			}
			if saves != 1 {
				t.Errorf("%s: %d SaveImage nodes, want 1", e.ID, saves)
			}
			loads := 0
			for _, raw := range g {
				if raw.(map[string]any)["class_type"] == "LoadImage" {
					loads++
				}
			}
			if loads != len(refs) {
				t.Errorf("%s: %d LoadImage nodes for %d references", e.ID, loads, len(refs))
			}
		}
	}
}

func TestImageGraphsWireReferencesTheNodesWay(t *testing.T) {
	refs := []any{"a.png", "b.png"}
	for id, want := range map[string][]string{
		"nanobanana21": {"model.images.image_1", "model.images.image_2"},
		"krea3":        {"model.images.image_0", "model.images.image_1"},
		"seedream5":    {"model.images.image_1", "model.images.image_2"},
	} {
		e, _ := imageEngineByID(id)
		in := e.graph(imageJob{Prompt: "p", Aspect: "square", Resolution: "1K", Refs: refs})["1"].(map[string]any)["inputs"].(map[string]any)
		for _, k := range want {
			if _, ok := in[k].([]any); !ok {
				t.Errorf("%s: no link at %s (inputs %v)", id, k, in)
			}
		}
	}
	e, _ := imageEngineByID("qwen21-edit")
	g := e.graph(imageJob{Prompt: "p", Refs: refs})
	enc := g["5"].(map[string]any)["inputs"].(map[string]any)
	if _, ok := enc["images.image_2"].([]any); !ok {
		t.Errorf("Qwen edit encoder lacks images.image_2: %v", enc)
	}
	if latent := g["8"].(map[string]any)["inputs"].(map[string]any)["latent_image"].([]any); latent[0] != "5" || latent[1] != 2 {
		t.Errorf("Qwen edit samples from %v, want the encoder's latent", latent)
	}
}

func TestImageGraphsUseTheChosenShape(t *testing.T) {
	z, _ := imageEngineByID("zimage")
	lat := z.graph(imageJob{Aspect: "tall"})["7"].(map[string]any)["inputs"].(map[string]any)
	if lat["width"] != 768 || lat["height"] != 1344 {
		t.Errorf("tall latent %v", lat)
	}
	s, _ := imageEngineByID("seedream5")
	in := s.graph(imageJob{Aspect: "wide", Resolution: "2K"})["1"].(map[string]any)["inputs"].(map[string]any)
	if in["model.size_preset"] != "(2K) 2848x1600 (16:9)" {
		t.Errorf("seedream preset %v", in["model.size_preset"])
	}
	n, _ := imageEngineByID("nanobanana21")
	in = n.graph(imageJob{Aspect: "portrait", Resolution: "1K"})["1"].(map[string]any)["inputs"].(map[string]any)
	if in["model.aspect_ratio"] != "2:3" || in["model"] != "Gemini Nano Banana 2.1" {
		t.Errorf("nano banana inputs %v", in)
	}
}

func TestImagePromptsFollowEachModelsGuide(t *testing.T) {
	byFamily := map[string]imageEngine{}
	for _, e := range imageEngines {
		if e.Family == "" {
			t.Errorf("%s has no prompt family", e.ID)
		}
		byFamily[e.Family] = e
	}
	for _, c := range []struct {
		family, purpose, subject string
		refs                     int
		want                     []string
	}{
		{"qwen", "character", "a knight.", 0, []string{"The image is a wide", "character reference sheet of a knight,", "The lighting is", "The overall composition"}},
		{"qwen", "environment", "", 0, []string{"environment sheet of an original location", "reverse angle"}},
		{"qwen-edit", "character", "", 2, []string{"the character in <image1> and <image2>"}},
		{"zimage", "character", "a courier", 0, []string{"Character turnaround sheet of a courier:", "free of any lettering"}},
		{"krea2", "environment", "a night market", 0, []string{"environment sheet of a night market", krea2NoText}},
		{"gemini", "character", "", 0, []string{"Create a character turnaround sheet", "for a video production", "no labels or captions"}},
		{"gemini", "character", "", 1, []string{"the character in the first image"}},
		{"seedream", "environment", "a ruined chapel", 0, []string{"Design an environment concept sheet in one image for a ruined chapel."}},
		{"seedream", "object", "", 3, []string{"from Image 1, Image 2, and Image 3"}},
		{"krea3", "place", "", 1, []string{"setting from @Image1"}},
		{"gpt", "character", "a knight", 0, []string{"Intended use: a character reference sheet", "Subject: a knight,", "Constraints:"}},
		{"gpt", "character", "", 2, []string{"Image 1: the character to draw.", "Image 2: more of the same character.", "Do not redesign the character."}},
		{"gpt", "object", "", 1, []string{"Image 1: the object.", "Change only the setting", "Do not restyle the object."}},
		{"gemini", "free", "Exactly as typed.", 2, []string{"Exactly as typed."}},
	} {
		got := imagePrompt(byFamily[c.family], c.purpose, c.subject, c.refs)
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s %s %q (%d refs): %q lacks %q", c.family, c.purpose, c.subject, c.refs, got, w)
			}
		}
	}
	// Seedream turns "a set" or "a series" into several images; a sheet is one.
	for _, purpose := range []string{"character", "environment", "object", "place"} {
		for _, refs := range []int{0, 2} {
			p := strings.ToLower(imagePrompt(byFamily["seedream"], purpose, "", refs))
			if strings.Contains(p, "a set") || strings.Contains(p, "series") {
				t.Errorf("seedream %s prompt asks for several images: %q", purpose, p)
			}
		}
	}
	// No family words a request the same as another.
	seen := map[string]string{}
	for fam, e := range byFamily {
		p := imagePrompt(e, "character", "a knight", 0)
		if other, dup := seen[p]; dup {
			t.Errorf("%s and %s write the same character prompt", fam, other)
		}
		seen[p] = fam
		if strings.Contains(p, "—") {
			t.Errorf("%s prompt has a long dash", fam)
		}
	}
	if got := imagePrompt(byFamily["gemini"], "free", "Exactly as typed.", 0); got != "Exactly as typed." {
		t.Errorf("free prompt rewritten to %q", got)
	}
}

func TestImagePricesAreThePublishedOnes(t *testing.T) {
	for _, c := range []struct {
		id, res string
		refs    int
		lo, hi  float64
	}{
		{"nanobanana21", "1K", 0, 0.058773, 0.058773},
		{"nanobanana2", "2K", 3, 0.1217, 0.1217},
		{"gpt-image-25-flare", "", 2, 0.0753 + 2*0.0117, 0.0753 + 2*0.0176},
		{"krea3", "1K", 2, 0.1001, 0.1001},
		{"krea3", "2K", 1, 0.1144, 0.1859},
		{"seedream5", "1K", 3, 0.051, 0.051},
		{"zimage", "1K", 0, 0, 0},
	} {
		e, _ := imageEngineByID(c.id)
		lo, hi := e.cost("square", c.res, c.refs)
		if math.Abs(lo-c.lo) > 1e-6 || math.Abs(hi-c.hi) > 1e-6 {
			t.Errorf("%s %s %d refs: %.6f–%.6f, want %.6f–%.6f", c.id, c.res, c.refs, lo, hi, c.lo, c.hi)
		}
	}
	if got := usd(0.045); got != "$0.045" {
		t.Errorf("usd(0.045) = %s", got)
	}
}

func TestImageEnginesRunWhereTheyCan(t *testing.T) {
	qwen, _ := imageEngineByID("qwen21")
	if w := qwen.where(onCloud); w != onCloud {
		t.Fatalf("open model with cloud chosen runs on %s", w)
	}
	if err := qwen.readiness(context.Background(), onCloud, "key", nil); err != nil {
		t.Errorf("Qwen on Cloud with a key: %v", err)
	}
	var off errOffline
	nano, _ := imageEngineByID("nanobanana21")
	if nano.where(onComfyUI) != onCloud {
		t.Error("a partner model left Comfy Cloud")
	}
	if err := nano.readiness(context.Background(), onCloud, "", nil); !errors.As(err, &off) {
		t.Errorf("partner model without a key: %v", err)
	}
	if err := nano.readiness(context.Background(), onCloud, "key", nil); err != nil {
		t.Errorf("partner model with a key: %v", err)
	}
}

func TestRatingsOnlyNameRealEnginesAndRankBestFirst(t *testing.T) {
	images := map[string]bool{}
	for _, e := range imageEngines {
		images[e.ID] = true
	}
	for purpose, list := range imageRatings {
		for id, r := range list {
			if !images[id] {
				t.Errorf("image rating %s names unknown engine %s", purpose, id)
			}
			if r.Note == "" || r.Tier == tierUntested {
				t.Errorf("image rating %s/%s needs a tier and what the run showed", purpose, id)
			}
		}
	}
	seedance, edit := newSeedanceEngines(nil, nil)
	video := map[string]bool{}
	for _, en := range append([]engine{seedance, edit, newMiniMaxH3Engine(nil, nil, false), newMiniMaxH3LocalEngine(nil, nil),
		newAnimateEngine("", nil, nil), newWan3Engine(nil, nil, nil)},
		styleEngines(newGenjutsuEngine(nil, nil), newWan3Engine(nil, nil, nil), newWanEditEngine(nil, nil), newAlephEngine(nil, nil), edit, newLTXStyleBase(nil, nil))...) {
		video[en.info().ID] = true
	}
	for mode, list := range videoRatings {
		for id, r := range list {
			if !video[id] {
				t.Errorf("video rating %s names unknown engine %s", mode, id)
			}
			if r.Note == "" {
				t.Errorf("video rating %s/%s has no note", mode, id)
			}
		}
	}
	ids := []string{"zimage", "seedream5", "krea3", "qwen21-turbo", "nanobanana2"}
	byRating(ids, func(id string) string { return imageRating("environment", false, id).Tier })
	if got := strings.Join(ids, " "); got != "seedream5 krea3 qwen21-turbo nanobanana2 zimage" {
		t.Errorf("environment order %q", got)
	}
}
