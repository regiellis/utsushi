package main

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/regiellis/hyperchi/hyperchi/database"
)

func TestRatioLabelSnapsToCommonShapes(t *testing.T) {
	for _, c := range []struct {
		w, h int
		want string
	}{{480, 854, "9:16"}, {1920, 1080, "16:9"}, {1024, 1024, "1:1"}, {1000, 300, "1000×300"}} {
		if got := ratioLabel(c.w, c.h); got != c.want {
			t.Errorf("%dx%d reads %q, want %q", c.w, c.h, got, c.want)
		}
	}
}

func TestCostLabelNeverInventsAPrice(t *testing.T) {
	for _, c := range []struct {
		in   engineInfo
		want string
		free bool
	}{
		{engineInfo{Billed: false}, "Free · your GPU", true},
		{engineInfo{Billed: true, Via: "Comfy Router"}, "Billed per run", false},
		{engineInfo{Billed: true, Rates: map[string]float64{"480P": 0.0715, "1080P": 0.286}}, "$0.07–0.29/s", false},
		{engineInfo{Billed: true, Rates: map[string]float64{"768P": 0.1144, "2K": 0.1144}, InputRates: map[string]float64{"768P": 0.2}}, "$0.11/s + clip", false},
	} {
		got, free := costLabel(c.in)
		if got != c.want || free != c.free {
			t.Errorf("%+v reads %q (free %v), want %q (free %v)", c.in, got, free, c.want, c.free)
		}
	}
}

func TestRefRuleSaysHowManyImages(t *testing.T) {
	for _, c := range []struct {
		l    limits
		want string
	}{
		{limits{}, "No reference images"},
		{limits{MaxRefs: 1}, "Up to 1 image · optional"},
		{limits{MinRefs: 1, MaxRefs: 1}, "1 image"},
		{limits{MinRefs: 1, MaxRefs: 4}, "1–4 images"},
	} {
		if got := refRule(c.l); got != c.want {
			t.Errorf("%+v reads %q, want %q", c.l, got, c.want)
		}
	}
}

func TestEveryEngineSaysWhatItsReferencesAreFor(t *testing.T) {
	genjutsu := newGenjutsuEngine(nil, nil)
	wan3 := newWan3Engine(nil, nil, nil)
	wanEdit := newWanEditEngine(nil, nil)
	aleph := newAlephEngine(nil, nil)
	seedance, seedanceEdit := newSeedanceEngines(nil, nil)
	all := append([]engine{genjutsu, wan3, wanEdit, aleph, newAnimateEngine("", nil, nil), seedance, seedanceEdit,
		newMiniMaxH3Engine(nil, nil, false), newMiniMaxH3Engine(nil, nil, true), newMiniMaxH3LocalEngine(nil, nil),
		newMiniMaxH3ControlEngine(nil, nil), newLTXControlEngine(nil, nil)},
		styleEngines(genjutsu, wan3, wanEdit, aleph, seedanceEdit, newLTXStyleBase(nil, nil))...)
	for _, en := range all {
		if in := en.info(); in.Limits.RefsFor == "" {
			t.Errorf("%s does not say what its reference images are for", in.ID)
		}
	}
}

func TestLimitHintReadsAsOneChip(t *testing.T) {
	for _, c := range []struct {
		l    limits
		want string
	}{
		{limits{MinRefs: 1, MaxRefs: 8, MinSeconds: 4, MaxSeconds: 30}, "4–30 s · 1–8 images"},
		{limits{MinRefs: 1, MaxRefs: 1, MinSeconds: 1, MaxSeconds: 30}, "1–30 s · 1 image"},
		{limits{MaxRefs: 1, MaxSeconds: 30, PromptRequired: true}, "≤ 30 s · up to 1 image · prompt"},
	} {
		if got := limitHint(c.l); got != c.want {
			t.Errorf("%+v reads %q, want %q", c.l, got, c.want)
		}
	}
}

func TestWan3TagsTranslateStudioTokens(t *testing.T) {
	got := wan3Tags("Recast @1 holding @2 in @clip, like @Image3, moving like @v1 to @a2")
	if got != "Recast @Image1 holding @Image2 in @Video1, like @Image3, moving like @Video2 to @Audio2" {
		t.Errorf("translated to %q", got)
	}
}

func TestGPULabelShortensComfyUIsDeviceName(t *testing.T) {
	if got := gpuLabel("cuda:0 NVIDIA RTX 4000 SFF Ada Generation : cudaMallocAsync", 20989804544); got != "RTX 4000 SFF Ada · 20 GB" {
		t.Errorf("label %q", got)
	}
	if got := gpuLabel("cuda:0 NVIDIA GeForce RTX 4090 : native", 25757220864); got != "RTX 4090 · 24 GB" {
		t.Errorf("label %q", got)
	}
}

func TestSplitErrorSeparatesCauseFromHint(t *testing.T) {
	title, hint := splitError("Could not reach 127.0.0.1:8189 (connection refused). Nothing was billed.")
	if title != "Could not reach 127.0.0.1:8189 (connection refused)." || hint != "Nothing was billed." {
		t.Errorf("split into %q / %q", title, hint)
	}
	if title, hint := splitError("Out of VRAM"); title != "Out of VRAM" || hint != "" {
		t.Errorf("single sentence split into %q / %q", title, hint)
	}
}

func TestLibraryRecordsMediaSize(t *testing.T) {
	lib := newTestLibrary(t)
	clip, err := lib.store(bytes.NewReader(fakeMP4(3)), originUpload, "clip.mp4", 0)
	if err != nil {
		t.Fatal(err)
	}
	img, err := lib.store(bytes.NewReader(fakePNG(t)), originUpload, "ref.png", 0)
	if err != nil {
		t.Fatal(err)
	}
	if clip.Width != 1280 || clip.Height != 720 || img.Width != 300 || img.Height != 300 {
		t.Fatalf("clip %dx%d, image %dx%d", clip.Width, clip.Height, img.Width, img.Height)
	}
}

func TestStoreMigrationAddsSizeColumnsOnce(t *testing.T) {
	cfg := database.DefaultDBConfig()
	cfg.Path = filepath.Join(t.TempDir(), "test.db")
	db, err := database.NewDB(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	for range 2 {
		if _, err := newStore(db); err != nil {
			t.Fatal(err)
		}
	}
	st := &store{db: db}
	m := Media{ID: newID(), Kind: kindImage, Origin: originUpload, File: "x.png", Status: mediaReady, Width: 640, Height: 480, CreatedAt: now()}
	if err := st.addMedia(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	got, err := st.media(context.Background(), m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Width != 640 || got.Height != 480 {
		t.Fatalf("stored %dx%d", got.Width, got.Height)
	}
}

func TestGenjutsuTrimsLongClipsInsteadOfRefusingThem(t *testing.T) {
	en := newGenjutsuEngine(fixedModels(nil), &staticURLs{})
	l := en.info().Limits
	long := task{Source: Media{Kind: kindVideo, Seconds: 45}, Refs: []Media{{Kind: kindImage, Status: mediaReady}}, Resolution: "720p"}
	if fields := validate(l, long); fields != nil {
		t.Fatalf("a 45 s clip is refused: %v", fields)
	}
	short := long
	short.Source.Seconds = 2
	if fields := validate(l, short); fields["source"] == nil {
		t.Fatal("a 2 s clip passes, though the schema says at least 4 s")
	}
	if got := limitHint(l); got != "≥ 4 s · uses first 30 s · 1–8 images" {
		t.Errorf("hint %q", got)
	}
}

func TestStylePromptNamesReferencesTheEngineWay(t *testing.T) {
	plain := stylePrompt("a knitted yarn world", "", 1, nil)
	if !strings.Contains(plain, "as a knitted yarn world, matching the look of the first reference image") || !strings.Contains(plain, "characters in reference 2") {
		t.Errorf("plain prompt %q", plain)
	}
	tagged := stylePrompt("", "Make it rain.", 2, func(n int) string { return fmt.Sprintf("@Image%d", n) })
	if !strings.Contains(tagged, "visual style of the look of @Image1") || !strings.Contains(tagged, "@Image2, @Image3") || !strings.HasSuffix(tagged, "Make it rain.") {
		t.Errorf("tagged prompt %q", tagged)
	}
}

func second[A, B any](_ A, b B) B { return b }

func TestStyleEnginesCountCharactersOnly(t *testing.T) {
	for _, en := range styleEngines(newGenjutsuEngine(fixedModels(nil), &staticURLs{}), newWan3Engine(nil, nil, fixedKey("")),
		newWanEditEngine(fixedModels(nil), &staticURLs{}), newAlephEngine(fixedModels(nil), &staticURLs{}), second(newSeedanceEngines(nil, nil)), newLTXStyleBase(nil, nil)) {
		in := en.info()
		if in.Mode != modeStyle || in.Limits.MinRefs != 0 || in.Limits.PromptRequired {
			t.Errorf("%s: %+v", in.ID, in)
		}
	}
	if _, ok := presetByKey("knitted-yarn"); !ok || len(stylePresets) != 19 {
		t.Errorf("%d presets", len(stylePresets))
	}
}

func TestTileShapeFollowsTheImage(t *testing.T) {
	for _, c := range []struct {
		w, h int
		want string
	}{{1344, 768, "wide"}, {1024, 1024, ""}, {1216, 832, "wide"}, {832, 1216, "tall"}, {768, 1344, "tall"}, {1024, 1536, "tall"}, {1152, 864, ""}} {
		if got := tileShape(c.w, c.h); got != c.want {
			t.Errorf("%dx%d is %q, want %q", c.w, c.h, got, c.want)
		}
	}
}
