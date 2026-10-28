package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/regiellis/comfy-api-proxy-go/sdk"
)

func TestLongFramesSitOnTheModelsGridInsideItsRange(t *testing.T) {
	for _, c := range []struct {
		secs      float64
		continues bool
		want      int
	}{
		{5, false, 124}, {8, false, 192}, {10, false, 243}, {15, false, 362}, {5, true, 158}, {15, true, 362}, {1, false, 124},
	} {
		got := longFrames(c.secs, c.continues)
		if got != c.want || (got-5)%17 != 0 || got < longMinFrames || got > longMaxFrames {
			t.Errorf("%g s (continues %v): %d frames, want %d on the 17k+5 grid in %d..%d", c.secs, c.continues, got, c.want, longMinFrames, longMaxFrames)
		}
	}
}

func TestLongPlanJoinsAndTimesTheShots(t *testing.T) {
	in := longForm{Prompts: []string{"one", "two", "three"}, Seconds: []string{"5", "10", "8"},
		Joins: []string{"continue", "continue", "cut"}, Shape: "portrait", Size: "draft", Quality: "turbo"}
	parts, fields := longPlan(in, nil)
	if fields != nil {
		t.Fatal(fields)
	}
	if !parts[0].Cut || parts[1].Cut || !parts[2].Cut {
		t.Errorf("joins %v %v %v: the first shot of a prompt-only video must start fresh, then as asked", parts[0].Cut, parts[1].Cut, parts[2].Cut)
	}
	// A continuing shot's new footage leaves out the lead-in it replays.
	if want := float64(longFrames(10, true)-longTail) / longFPS; math.Abs(parts[1].Length-want) > 1e-9 {
		t.Errorf("shot 2 adds %.3f s, want %.3f", parts[1].Length, want)
	}
	if math.Abs(parts[2].Start-(parts[1].Start+parts[1].Length)) > 1e-9 {
		t.Error("each shot should start where the one before ends")
	}
	start := Media{Kind: kindVideo, Seconds: 12, Width: 1280, Height: 720}
	parts, _ = longPlan(in, &start)
	if parts[0].Cut || parts[0].Start != 12 {
		t.Errorf("extending a video, shot 1 should continue from its end at 12 s: %+v", parts[0])
	}
	for _, bad := range []longForm{
		{Prompts: []string{"x"}, Seconds: []string{"7"}, Joins: []string{"cut"}},
		{Prompts: []string{" "}, Seconds: []string{"5"}, Joins: []string{"cut"}},
		{},
	} {
		if _, fields := longPlan(bad, nil); fields == nil {
			t.Errorf("%+v should be refused", bad)
		}
	}
}

func TestLongSizeFollowsTheStartVideosShape(t *testing.T) {
	if w, h := longSize(longForm{Shape: "portrait", Size: "draft"}, nil); w != 480 || h != 864 {
		t.Errorf("portrait draft is %dx%d, want 480x864", w, h)
	}
	if w, h := longSize(longForm{Shape: "portrait", Size: "hd"}, &Media{Width: 1920, Height: 1080}); w != 1280 || h != 704 && h != 736 {
		t.Errorf("a 16:9 start at 720p is %dx%d", w, h)
	}
}

func TestLongGraphCastsAndGuidesTheShot(t *testing.T) {
	wf, err := sdk.WorkflowFromFile("workflows/minimax-h3-r2v.api.json")
	if err != nil {
		t.Fatal(err)
	}
	longGraph(wf, task{Frames: 141, Width: 480, Height: 864, Turbo: true, Seed: 7, RefSize: "max"}, "a <Picture 1>", []any{"a.png", "b.png"}, "tail.mp4", "end.png")
	inputs := func(id string) map[string]any { return wf.Graph[id].(map[string]any)["inputs"].(map[string]any) }
	r2v := inputs("136")
	if r2v["ref_image_size"] != "max" {
		t.Errorf("reference detail: %v, want max", r2v["ref_image_size"])
	}
	if r2v["width"] != 480 || r2v["height"] != 864 || r2v["length"] != 141 {
		t.Errorf("size and length: %v %v %v", r2v["width"], r2v["height"], r2v["length"])
	}
	for i := range 2 {
		link, ok := r2v[fmt.Sprintf("ref_images.ref_image_%d", i)].([]any)
		if !ok || link[0] != fmt.Sprintf("utsushi_ref_%d", i) {
			t.Errorf("cast %d is not wired: %v", i, r2v[fmt.Sprintf("ref_images.ref_image_%d", i)])
		}
	}
	for _, id := range []string{"115", "131", "132", "137", "139"} {
		if _, ok := wf.Graph[id]; ok {
			t.Errorf("the template's node %s should be gone", id)
		}
	}
	if g := inputs("126")["conditioning"].([]any); g[0] != "utsushi_end_guide" {
		t.Errorf("the guider should read the last guide, reads %v", g)
	}
	if p := inputs("utsushi_end_guide")["positive"].([]any); p[0] != "utsushi_guide" || inputs("utsushi_end_guide")["frame_idx"] != -1 {
		t.Errorf("the end guide should follow the tail guide and anchor the last frame: %v", inputs("utsushi_end_guide"))
	}
	if inputs("utsushi_guide")["frame_idx"] != 0 || inputs("utsushi_tail")["file"] != "tail.mp4" {
		t.Error("the tail should be anchored at frame 0")
	}
	if inputs("146")["value"] != true || inputs("138")["value"] != "a <Picture 1>" {
		t.Error("turbo and the prompt should be set")
	}
	fresh, err := sdk.WorkflowFromFile("workflows/minimax-h3-r2v.api.json")
	if err != nil {
		t.Fatal(err)
	}
	longGraph(fresh, task{Frames: 124, Width: 480, Height: 864}, "x", nil, nil, nil)
	if _, ok := fresh.Graph["utsushi_guide"]; ok {
		t.Error("a shot that starts fresh needs no guide")
	}
	if g := fresh.Graph["126"].(map[string]any)["inputs"].(map[string]any)["conditioning"].([]any); g[0] != "136" {
		t.Errorf("without guides the guider reads the R2V node, reads %v", g)
	}
	if longTags("@1 meets @2") != "<Picture 1> meets <Picture 2>" {
		t.Errorf("tags: %q", longTags("@1 meets @2"))
	}
}

func TestJoinShotsGraphDropsTheReplayedLeadIn(t *testing.T) {
	g := joinShotsGraph([]joinInput{{seconds: 4, sound: true}, {seconds: 6, skip: longTail}, {seconds: 5, sound: true}}, 480, 864)
	for _, want := range []string{"[0:v]fps=24,trim=start_frame=0", "[1:v]fps=24,trim=start_frame=22", "anullsrc", "[2:a]", "concat=n=3:v=1:a=1[v][a]"} {
		if !strings.Contains(g, want) {
			t.Errorf("graph lacks %q:\n%s", want, g)
		}
	}
}

// replayEngine stands in for MiniMax H3: it makes a shot of the frames
// asked for, and a shot given a lead-in replays it first, as the model does.
type replayEngine struct {
	fr  *frames
	lib *library

	mu    sync.Mutex
	tails []string
	sizes []string
}

func (e *replayEngine) info() engineInfo {
	return engineInfo{ID: longEngineID, Mode: modeLong, Name: "Replay", Via: "Test", Tags: true, Limits: limits{MaxRefs: 9}}
}

func (e *replayEngine) ready(context.Context) error { return nil }

func (e *replayEngine) generate(ctx context.Context, t task, tr tracker) (outcome, error) {
	e.mu.Lock()
	e.tails = append(e.tails, t.Source.ID)
	e.sizes = append(e.sizes, fmt.Sprintf("%dx%d/%d", t.Width, t.Height, t.Frames))
	e.mu.Unlock()
	tr.queued("req-"+t.Key, nil)
	dst := filepath.Join(e.fr.cache, "shot-"+newID()+".mp4")
	size := fmt.Sprintf("%dx%d", t.Width, t.Height)
	args := []string{"-f", "lavfi", "-i", "testsrc2=size=" + size + ":rate=24", "-f", "lavfi", "-i", "sine=frequency=660"}
	filter := fmt.Sprintf("[0:v]trim=end_frame=%d[v];[1:a]atrim=end=%.6f[a]", t.Frames, float64(t.Frames)/longFPS)
	if t.Source.ID != "" {
		args = append(args, "-i", e.lib.path(t.Source))
		filter = fmt.Sprintf("[2:v]setpts=PTS-STARTPTS[lead];[0:v]trim=end_frame=%d,setpts=PTS-STARTPTS[rest];[lead][rest]concat=n=2:v=1:a=0[v];"+
			"[1:a]atrim=end=%.6f[a]", t.Frames-longTail, float64(t.Frames)/longFPS)
	}
	args = append(args, "-filter_complex", filter, "-map", "[v]", "-map", "[a]", "-c:v", "libx264", "-pix_fmt", "yuv420p",
		"-c:a", "aac", "-movflags", "+faststart", "-y", dst)
	if err := e.fr.encode(ctx, args...); err != nil {
		return outcome{}, err
	}
	return outcome{Write: func(_ context.Context, w io.Writer) error {
		f, err := os.Open(dst)
		if err != nil {
			return err
		}
		_, err = io.Copy(w, f)
		return errors.Join(err, f.Close(), os.Remove(dst))
	}}, nil
}

func (e *replayEngine) resume(context.Context, string, tracker) (outcome, error) {
	return outcome{}, errors.New("not resumable")
}

func (e *replayEngine) cancel(context.Context, string) error { return nil }

func TestALongVideoExtendsAClipShotByShotToThePlannedLength(t *testing.T) {
	ctx := context.Background()
	fr, err := newFrames(filepath.Join(t.TempDir(), "cache"))
	if err != nil {
		t.Fatal(err)
	}
	if !fr.available() {
		t.Skip("ffmpeg is not installed")
	}
	st, lib := newTestStore(t), newTestLibrary(t)
	clip := filepath.Join(t.TempDir(), "start.mp4")
	// A 3 s start video at another size and rate, with no sound.
	if err := fr.encode(ctx, "-f", "lavfi", "-i", "smptebars=size=640x360:rate=30:duration=3",
		"-c:v", "libx264", "-pix_fmt", "yuv420p", "-movflags", "+faststart", "-y", clip); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(clip)
	if err != nil {
		t.Fatal(err)
	}
	start, err := lib.store(f, originUpload, "start.mp4", 0)
	if cerr := f.Close(); cerr != nil {
		t.Fatal(cerr)
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := st.addMedia(ctx, start); err != nil {
		t.Fatal(err)
	}
	in := longForm{Prompts: []string{"go on", "a new shot"}, Seconds: []string{"5", "5"}, Joins: []string{"continue", "cut"},
		Shape: "portrait", Size: "draft", Quality: "turbo"}
	parts, fields := longPlan(in, &start)
	if fields != nil {
		t.Fatal(fields)
	}
	w, h := longSize(in, &start)
	eng := &replayEngine{fr: fr, lib: lib}
	refs, err := newLocalRefs("http://127.0.0.1:1", "")
	if err != nil {
		t.Fatal(err)
	}
	rn := newRunner(st, lib, &engines{list: []engine{eng}}, refs, &recorder{})
	rn.frames = fr
	t.Cleanup(rn.shutdown)
	run := Run{ID: newID(), Batch: "l", Mode: modeLong, Engine: longEngineID, Prompt: "go on", SourceID: start.ID,
		RefIDs: []string{}, Status: runPreparing, IdempotencyKey: "utsushi-l-a", CreatedAt: now(),
		Parts: parts, Width: w, Height: h, Quality: "turbo"}
	if err := st.addRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	rn.start(run)
	deadline := time.Now().Add(2 * time.Minute)
	for {
		if run, err = st.run(ctx, run.ID); err != nil {
			t.Fatal(err)
		}
		if !runActive(run.Status) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("still %s: %s", run.Status, run.Detail)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if run.Status != runSucceeded {
		t.Fatalf("run %s: %s", run.Status, run.Error)
	}
	eng.mu.Lock()
	tails, sizes := eng.tails, eng.sizes
	eng.mu.Unlock()
	if tails[0] == "" || tails[1] != "" {
		t.Errorf("shot 1 continues from the start video and shot 2 cuts: tails %q", tails)
	}
	if want := fmt.Sprintf("%dx%d/%d", w, h, longFrames(5, true)); sizes[0] != want {
		t.Errorf("shot 1 rendered %s, want %s", sizes[0], want)
	}
	lead, err := st.media(ctx, run.Parts[0].TailID)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(lead.Seconds-float64(longTail)/longFPS) > 0.05 || lead.Width != w || lead.Height != h {
		t.Errorf("the lead-in is %.3f s at %dx%d, want %d frames at %dx%d", lead.Seconds, lead.Width, lead.Height, longTail, w, h)
	}
	result, err := st.media(ctx, run.ResultID)
	if err != nil {
		t.Fatal(err)
	}
	want := start.Seconds + parts[0].Length + parts[1].Length
	if math.Abs(result.Seconds-want) > 0.1 || result.Width != w || result.Height != h {
		t.Errorf("joined %.2f s at %dx%d, want %.2f s at %dx%d", result.Seconds, result.Width, result.Height, want, w, h)
	}
	if !fr.hasSound(ctx, lib.path(result)) {
		t.Error("the joined video has no sound")
	}
	if _, owned, err := st.deleteRun(ctx, run.ID); err != nil || len(owned) != 4 {
		t.Errorf("deleting the run removes %d files (%v), want the result, both shots, and the lead-in", len(owned), err)
	}
}

func TestLongSetupOfAnEarlierTakeReadsBackWhatMadeIt(t *testing.T) {
	r := Run{ID: "r", SourceID: "", RefIDs: []string{"b", "a"}, Width: 1280, Height: 704, Quality: "full", CastView: "front",
		Parts: []part{{Prompt: "one", Frames: longFrames(8, false), Cut: true, ResultID: "m1"},
			{Prompt: "two", Frames: longFrames(12, true), ResultID: ""}}}
	s := longSetupOf(r)
	if s.From != "r" || s.Cast != `["b","a"]` || s.Shape != "landscape" || s.Size != "hd" || s.Quality != "full" || s.CastView != "front" {
		t.Errorf("setup %+v", s)
	}
	if len(s.Shots) != 2 || s.Shots[0].Seconds != 8 || s.Shots[0].Join != "cut" || !s.Shots[0].Done ||
		s.Shots[1].Seconds != 12 || s.Shots[1].Join != "continue" || s.Shots[1].Done {
		t.Errorf("shots %+v", s.Shots)
	}
	if old := longSetupOf(Run{Width: 480, Height: 864}); old.CastView != "sheet" || old.RefSize != "match" || old.Shape != "portrait" || old.Size != "draft" {
		t.Errorf("a take from before the sheet option sent whole sheets, at 9:16 480p: %+v", old)
	}
}

func TestKeptShotsAreCopiedOnlyWhenNothingChanged(t *testing.T) {
	ctx := context.Background()
	st, lib := newTestStore(t), newTestLibrary(t)
	w := &web{st: st, lib: lib}
	shot := func(label string) Media {
		m, err := lib.store(strings.NewReader(string(fakeMP4(5))), originPart, label, 0)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.addMedia(ctx, m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	// The earlier take: shot 1, shot 2 continuing it, shot 3 a new shot.
	base := longForm{Refs: []string{"x", "y"}, Prompts: []string{"one", "two", "three"}, Seconds: []string{"5", "5", "5"},
		Joins: []string{"cut", "continue", "cut"}, Shape: "portrait", Size: "draft", Quality: "turbo", CastView: "front", RefSize: "max"}
	made, fields := longPlan(base, nil)
	if fields != nil {
		t.Fatal(fields)
	}
	old := Run{ID: newID(), Batch: "o", Mode: modeLong, Engine: longEngineID, Prompt: "one", RefIDs: []string{"x", "y"},
		Status: runSucceeded, IdempotencyKey: "k", CreatedAt: now(), Width: 480, Height: 864, Quality: "turbo", CastView: "front",
		RefSize: "max", Parts: slices.Clone(made)}
	for i := range old.Parts {
		old.Parts[i].ResultID = shot(fmt.Sprintf("Shot %d", i+1)).ID
	}
	if err := st.addRun(ctx, old); err != nil {
		t.Fatal(err)
	}
	try := func(f longForm, width, height int) ([]part, string) {
		t.Helper()
		parts, fields := longPlan(f, nil)
		if fields != nil {
			t.Fatal(fields)
		}
		problem, err := w.keepShots(ctx, f, parts, nil, width, height)
		if err != nil {
			t.Fatal(err)
		}
		return parts, problem
	}
	form := base
	form.From = old.ID

	// Shot 2 changes: shot 1 is kept, and shot 3, a new shot, is kept too.
	form.Prompts = []string{"one", "TWO", "three"}
	form.Keeps = []string{"0", "", "2"}
	parts, problem := try(form, 480, 864)
	if problem != "" {
		t.Fatalf("keeping shots 1 and 3 around a changed shot 2: %q", problem)
	}
	for i, wantKept := range []bool{true, false, true} {
		if parts[i].Kept != wantKept {
			t.Errorf("shot %d kept %v, want %v", i+1, parts[i].Kept, wantKept)
		}
		if wantKept && (parts[i].ResultID == "" || parts[i].ResultID == old.Parts[i].ResultID) {
			t.Errorf("shot %d should be a copy of its own", i+1)
		}
		if wantKept {
			if _, err := os.Stat(lib.path(mustMedia(t, st, parts[i].ResultID))); err != nil {
				t.Errorf("shot %d's copy has no file: %v", i+1, err)
			}
		}
	}

	// Shot 1 changes: shot 2 continues it, so it cannot be kept.
	f := form
	f.Prompts, f.Keeps = []string{"ONE", "two", "three"}, []string{"", "1", "2"}
	if _, problem := try(f, 480, 864); !strings.Contains(problem, "continues a shot that is made again") {
		t.Errorf("a continuing shot after a remade one was kept: %q", problem)
	}

	// Everything kept is refused; so is any change to how shots are made.
	f = form
	f.Prompts, f.Keeps = base.Prompts, []string{"0", "1", "2"}
	if _, problem := try(f, 480, 864); problem == "" {
		t.Error("keeping every shot should be refused")
	}
	for name, change := range map[string]func(f *longForm) (int, int){
		"another cast order": func(f *longForm) (int, int) { f.Refs = []string{"y", "x"}; return 480, 864 },
		"another sheet view": func(f *longForm) (int, int) { f.CastView = "sheet"; return 480, 864 },
		"another detail":     func(f *longForm) (int, int) { f.RefSize = "match"; return 480, 864 },
		"another size":       func(f *longForm) (int, int) { return 720, 1280 },
		"a changed kept shot": func(f *longForm) (int, int) {
			f.Prompts = []string{"one!", "TWO", "three"}
			return 480, 864
		},
		"a shot the take never had": func(f *longForm) (int, int) { f.Keeps = []string{"0", "", "7"}; return 480, 864 },
		"a kept shot with another end image": func(f *longForm) (int, int) {
			f.Ends = []string{"some-image", "", ""}
			return 480, 864
		},
	} {
		f := form
		f.Refs, f.Prompts, f.Keeps = slices.Clone(form.Refs), slices.Clone(form.Prompts), slices.Clone(form.Keeps)
		width, height := change(&f)
		if _, problem := try(f, width, height); problem == "" {
			t.Errorf("%s should refuse to keep shots", name)
		}
	}
}

func mustMedia(t *testing.T, st *store, id string) Media {
	t.Helper()
	m, err := st.media(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestASoundtrackPlaysOverTheWholeJoinedVideo(t *testing.T) {
	ctx := context.Background()
	fr, err := newFrames(filepath.Join(t.TempDir(), "cache"))
	if err != nil {
		t.Fatal(err)
	}
	if !fr.available() {
		t.Skip("ffmpeg is not installed")
	}
	st, lib := newTestStore(t), newTestLibrary(t)
	keep := func(name string, args ...string) Media {
		t.Helper()
		path := filepath.Join(t.TempDir(), name)
		if err := fr.encode(ctx, append(args, "-y", path)...); err != nil {
			t.Fatal(err)
		}
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		m, err := lib.store(f, originPart, name, 0)
		if cerr := f.Close(); cerr != nil {
			t.Fatal(cerr)
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := st.addMedia(ctx, m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	shot := func(name string) Media {
		return keep(name, "-f", "lavfi", "-i", "testsrc2=size=480x864:rate=24:duration=3", "-f", "lavfi", "-i", "sine=frequency=330:duration=3",
			"-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac", "-movflags", "+faststart")
	}
	a, b := shot("a.mp4"), shot("b.mp4")
	track := keep("track.wav", "-f", "lavfi", "-i", "sine=frequency=880:duration=2", "-c:a", "pcm_s16le")
	rn := newRunner(st, lib, &engines{}, nil, &recorder{})
	rn.frames = fr
	t.Cleanup(rn.shutdown)
	for _, mode := range []string{"replace", "mix"} {
		run := Run{Mode: modeLong, Width: 480, Height: 864, SoundtrackID: track.ID, SoundtrackMode: mode,
			Parts: []part{{ResultID: a.ID, Cut: true}, {ResultID: b.ID, Cut: true}}}
		m, err := rn.joinShots(ctx, run)
		if err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
		if math.Abs(m.Seconds-6) > 0.1 || !fr.hasSound(ctx, lib.path(m)) {
			t.Errorf("%s: joined %.2f s (sound %v), want 6 s with sound: the 2 s track is padded to the whole", mode, m.Seconds, fr.hasSound(ctx, lib.path(m)))
		}
	}
}

func TestAnImageAShotEndsOnStaysInTheLibrary(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	img := Media{ID: newID(), Kind: kindImage, Origin: originUpload, File: "e.png", Status: mediaReady, CreatedAt: now()}
	if err := st.addMedia(ctx, img); err != nil {
		t.Fatal(err)
	}
	r := Run{ID: newID(), Batch: "b", Mode: modeLong, Engine: longEngineID, RefIDs: []string{}, Status: runSucceeded,
		IdempotencyKey: "k", CreatedAt: now(), Parts: []part{{Prompt: "p", Frames: 124, Cut: true, EndID: img.ID}}}
	if err := st.addRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	if _, err := st.deleteMedia(ctx, img.ID); !errors.Is(err, errInUse) {
		t.Errorf("deleting an image a shot ends on: %v, want errInUse", err)
	}
}
