package main

import (
	"math"
	"strings"
	"testing"
)

func TestCloudVideoGraphPutsTheClipFirst(t *testing.T) {
	seedance, _ := newSeedanceEngines(nil, nil)
	tk := task{Prompt: "@1 dances like @v1 to @a1 in @clip", Resolution: "720p", Source: Media{Seconds: 6.4},
		Refs: []Media{{Kind: kindImage}, {Kind: kindVideo}, {Kind: kindAudio}, {Kind: kindImage}}}
	g := seedance.graph(tk, "clip.mp4", []any{"a.png", "walk.mp4", "voice.wav", "b.png"}, 7)
	in := g["1"].(map[string]any)["inputs"].(map[string]any)
	if g["1"].(map[string]any)["class_type"] != "ByteDance2ReferenceNodeV2" || in["model.task_type"] != "reference" {
		t.Fatalf("node %v", g["1"])
	}
	for input, from := range map[string]string{
		"model.reference_videos.video_1": "10", "model.reference_images.image_1": "20",
		"model.reference_videos.video_2": "21", "model.reference_audios.audio_1": "22",
		"model.reference_images.image_2": "23",
	} {
		if l, ok := in[input].([]any); !ok || l[0] != from {
			t.Errorf("%s links %v, want node %s", input, in[input], from)
		}
	}
	if in["model.duration"] != 6 || in["model.ratio"] != "adaptive" {
		t.Errorf("duration %v ratio %v", in["model.duration"], in["model.ratio"])
	}
	if in["model.prompt"] != "@Image 1 dances like @Video 2 to @Audio 1 in @Video 1" {
		t.Errorf("prompt %q", in["model.prompt"])
	}
	for _, l := range graphLinks(g) {
		if _, ok := g[l[0]]; !ok {
			t.Errorf("%s links to missing node %s", l[1], l[0])
		}
	}
}

func TestMiniMaxNamesReferencesItsWay(t *testing.T) {
	h3 := newMiniMaxH3Engine(nil, nil, false)
	if got := h3.tags("@1 in @clip with @a2"); got != "Image 1 in Video 1 with Audio 2" {
		t.Errorf("tags %q", got)
	}
	in := h3.graph(task{Resolution: "768P", Source: Media{Seconds: 22}, Refs: []Media{{Kind: kindImage}}}, "c", []any{"a"}, 1)["1"].(map[string]any)["inputs"].(map[string]any)
	if in["model"] != "MiniMax H3" || in["model.duration"] != 15 {
		t.Errorf("inputs %v", in)
	}
	if p, _ := in["model.prompt"].(string); !strings.HasPrefix(p, "Recast the people in Video 1 as Image 1") {
		t.Errorf("default prompt %q", p)
	}
	hm := newMiniMaxH3Engine(nil, nil, true)
	if hm.graph(task{Resolution: "2K", Source: Media{Seconds: 2}}, "c", nil, 1)["1"].(map[string]any)["inputs"].(map[string]any)["model.duration"] != 4 {
		t.Error("a 2 s clip should ask for the 4 s minimum")
	}
}

func TestEstimateCountsTheClipWhereItIsBilled(t *testing.T) {
	seedance, _ := newSeedanceEngines(nil, nil)
	in := seedance.info()
	got, ok := estimateUSD(in, task{Resolution: "720p", Source: Media{Seconds: 10}}, 1)
	want := seedanceRates["720p"] * 20 // 10 s out, 10 s clip in
	if !ok || math.Abs(got-want) > 1e-9 {
		t.Errorf("Seedance 10 s at 720p: %.3f, want %.3f", got, want)
	}
	if math.Abs(want-3.954) > 0.01 {
		t.Errorf("Seedance rate drifted from the published table: %.3f", want)
	}
	got, _ = estimateUSD(in, task{Resolution: "480p", Source: Media{Seconds: 2}}, 1)
	if want := seedanceRates["480p"] * (4 + 2); math.Abs(got-want) > 1e-9 {
		t.Errorf("a 2 s clip bills the 4 s minimum output: %.3f, want %.3f", got, want)
	}
	wan3 := newWan3Engine(nil, nil, nil).info()
	if got, _ := estimateUSD(wan3, task{Resolution: "480P", Source: Media{Seconds: 5}}, 2); math.Abs(got-0.0715*5*2) > 1e-9 {
		t.Errorf("Wan 3.0 changed: %.4f", got)
	}
}
