package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPlanPartsCoversTheClipInTheFewestSegmentsTheEngineTakes(t *testing.T) {
	for _, c := range []struct {
		max, clip float64
		want      int
	}{
		{10, 30, 4}, {15, 30, 3}, {15, 16, 2}, {20, 30, 2}, {10, 11, 2},
	} {
		l := limits{MaxSeconds: c.max}
		parts := planParts(l, c.clip)
		if len(parts) != c.want {
			t.Fatalf("max %g, clip %g: %d parts, want %d", c.max, c.clip, len(parts), c.want)
		}
		for i, p := range parts {
			if p.Length > c.max-partMargin+1e-9 {
				t.Errorf("max %g, clip %g: part %d runs %g s, over the engine's limit", c.max, c.clip, i, p.Length)
			}
			if i > 0 && math.Abs(parts[i-1].Start+parts[i-1].Length-p.Start-partOverlap) > 1e-9 {
				t.Errorf("max %g, clip %g: parts %d and %d do not share %g s", c.max, c.clip, i-1, i, partOverlap)
			}
		}
		last := parts[len(parts)-1]
		if math.Abs(last.Start+last.Length-c.clip) > 1e-9 || parts[0].Start != 0 {
			t.Errorf("max %g, clip %g: parts cover %g to %g", c.max, c.clip, parts[0].Start, last.Start+last.Length)
		}
	}
	for _, c := range []struct{ max, clip float64 }{{15, 15}, {15, 31}, {0, 20}} {
		if parts := planParts(limits{MaxSeconds: c.max}, c.clip); parts != nil {
			t.Errorf("max %g, clip %g: want the clip whole or refused, got %d parts", c.max, c.clip, len(parts))
		}
	}
}

func TestJoinGraphFitsEachPartAndCrossfadesEachOverlap(t *testing.T) {
	parts := planParts(limits{MaxSeconds: 10}, 20)
	graph, out := joinGraph(parts, []float64{7, 0, 6.5}, 720, 1280)
	if out != "x2" {
		t.Fatalf("output label %q, want x2", out)
	}
	if n := strings.Count(graph, "xfade="); n != 2 {
		t.Fatalf("%d crossfades, want 2:\n%s", n, graph)
	}
	l := parts[0].Length
	for _, want := range []string{
		fmt.Sprintf("[0:v]setpts=(PTS-STARTPTS)*%.6f", l/7),
		"[1:v]setpts=(PTS-STARTPTS)*1.000000",
		fmt.Sprintf("offset=%.3f[x1]", l-partOverlap),
		fmt.Sprintf("offset=%.3f[x2]", 2*l-2*partOverlap),
		"scale=720:1280:force_original_aspect_ratio=decrease,pad=720:1280",
	} {
		if !strings.Contains(graph, want) {
			t.Errorf("graph lacks %q:\n%s", want, graph)
		}
	}
}

func TestLongClipsPassValidationOnlyWhenTheyCanRunInParts(t *testing.T) {
	l := limits{MaxSeconds: 15}
	clip := func(s float64) Media { return Media{Kind: kindVideo, Seconds: s, Status: mediaReady} }
	if f := validate(l, task{Source: clip(24)}); f["source"] == nil {
		t.Error("a 24 s clip passed a 15 s engine without parts")
	}
	if f := validate(l, task{Source: clip(24), Parts: true}); f["source"] != nil {
		t.Errorf("a 24 s clip in parts was refused: %v", f["source"])
	}
	if f := validate(l, task{Source: clip(31), Parts: true}); f["source"] == nil || !strings.Contains(f["source"][0], "run in parts") {
		t.Errorf("a 31 s clip should be refused naming the parts limit: %v", f["source"])
	}
	wan := limits{MaxSeconds: 15, MaxVideoRefs: 4, RefVideoTotal: 15}
	ref := Media{Kind: kindVideo, Seconds: 2, Status: mediaReady}
	if f := validate(wan, task{Source: clip(24), Refs: []Media{ref}, Parts: true}); f["refs"] != nil {
		t.Errorf("the reference total should count one part of the clip, not all of it: %v", f["refs"])
	}
}

func TestEstimateBillsEachPart(t *testing.T) {
	in := engineInfo{Billed: true, Rates: map[string]float64{"480p": 0.1}, InputRates: map[string]float64{"480p": 0.01},
		Limits: limits{MaxSeconds: 15}}
	whole, ok := estimateUSD(in, task{Resolution: "480p", Source: Media{Seconds: 24}}, 1)
	if !ok {
		t.Fatal("no estimate")
	}
	split, ok := estimateUSD(in, task{Resolution: "480p", Source: Media{Seconds: 24}, Parts: true}, 1)
	if !ok {
		t.Fatal("no estimate")
	}
	// Whole, the output caps at 15 s; in two 12.25 s parts each rounds to 12.
	if want := 0.1*15 + 0.01*24; math.Abs(whole-want) > 1e-9 {
		t.Errorf("whole: %g, want %g", whole, want)
	}
	if want := 2 * (0.1*12 + 0.01*12.25); math.Abs(split-want) > 1e-9 {
		t.Errorf("in parts: %g, want %g", split, want)
	}
}

func TestCancelBetweenPartsAsksNoProvider(t *testing.T) {
	run := Run{RequestID: "req-1", Parts: []part{{RequestID: "req-1", ResultID: "m1"}, {}}}
	if got := heldRequest(run); got != "" {
		t.Errorf("between parts the provider holds %q, want nothing", got)
	}
	run.Parts[1].RequestID = "req-2"
	if got := heldRequest(run); got != "req-2" {
		t.Errorf("held %q, want the part in flight", got)
	}
	if got := heldRequest(Run{RequestID: "req-9"}); got != "req-9" {
		t.Errorf("a whole run holds %q, want its own request", got)
	}
}

// echoEngine hands back the clip it was given, as an engine that changes
// nothing, and records what each generation saw.
type echoEngine struct {
	lib *library
	max float64

	mu      sync.Mutex
	keys    []string
	lengths []float64
	prompts []string
	refs    []int
	seeds   []int64
}

func (e *echoEngine) info() engineInfo {
	return engineInfo{ID: "echo", Mode: modeMotion, Name: "Echo", Via: "Test", Tags: true,
		Limits: limits{MaxSeconds: e.max, MaxRefs: 3}}
}

func (e *echoEngine) ready(context.Context) error { return nil }

func (e *echoEngine) generate(_ context.Context, t task, tr tracker) (outcome, error) {
	e.mu.Lock()
	e.keys = append(e.keys, t.Key)
	e.lengths = append(e.lengths, t.Source.Seconds)
	e.prompts = append(e.prompts, t.Prompt)
	e.refs = append(e.refs, len(t.Refs))
	e.seeds = append(e.seeds, t.Seed)
	e.mu.Unlock()
	tr.queued("req-"+t.Key, nil)
	path := e.lib.path(t.Source)
	return outcome{Write: func(_ context.Context, w io.Writer) error {
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		_, err = io.Copy(w, f)
		return errors.Join(err, f.Close())
	}}, nil
}

func (e *echoEngine) resume(context.Context, string, tracker) (outcome, error) {
	return outcome{}, errors.New("not resumable")
}

func (e *echoEngine) cancel(context.Context, string) error { return nil }

func TestALongClipRunsInPartsAndJoinsBackToItsLengthWithItsSound(t *testing.T) {
	ctx := context.Background()
	fr, err := newFrames(filepath.Join(t.TempDir(), "cache"))
	if err != nil {
		t.Fatal(err)
	}
	if !fr.available() {
		t.Skip("ffmpeg is not installed")
	}
	st, lib := newTestStore(t), newTestLibrary(t)
	clipPath := filepath.Join(t.TempDir(), "clip.mp4")
	if err := fr.encode(ctx, "-f", "lavfi", "-i", "testsrc=size=320x568:rate=24", "-f", "lavfi", "-i", "sine=frequency=440",
		"-t", "12", "-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac", "-movflags", "+faststart", "-y", clipPath); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(clipPath)
	if err != nil {
		t.Fatal(err)
	}
	src, err := lib.store(f, originUpload, "clip.mp4", 0)
	if cerr := f.Close(); cerr != nil {
		t.Fatal(cerr)
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := st.addMedia(ctx, src); err != nil {
		t.Fatal(err)
	}
	echo := &echoEngine{lib: lib, max: 5}
	refs, err := newLocalRefs("http://127.0.0.1:1", "")
	if err != nil {
		t.Fatal(err)
	}
	rn := newRunner(st, lib, &engines{list: []engine{echo}}, refs, &recorder{})
	rn.frames = fr
	t.Cleanup(rn.shutdown)

	run := Run{ID: newID(), Batch: "b", Mode: modeMotion, Engine: "echo", Resolution: "480p", SourceID: src.ID,
		RefIDs: []string{}, Status: runPreparing, IdempotencyKey: "utsushi-b-a", CreatedAt: now(),
		Parts: planParts(echo.info().Limits, src.Seconds)}
	if len(run.Parts) != 3 {
		t.Fatalf("a 12 s clip on a 5 s engine planned %d parts, want 3", len(run.Parts))
	}
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
			t.Fatalf("still %s after two minutes: %s", run.Status, run.Detail)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if run.Status != runSucceeded {
		t.Fatalf("run %s: %s", run.Status, run.Error)
	}
	echo.mu.Lock()
	keys := strings.Join(echo.keys, " ")
	lengths := echo.lengths
	echo.mu.Unlock()
	if keys != "utsushi-b-a-p1 utsushi-b-a-p2 utsushi-b-a-p3" {
		t.Errorf("each part should carry its own idempotency key: %s", keys)
	}
	for i, l := range lengths {
		if l > echo.max || math.Abs(l-run.Parts[i].Length) > 0.1 {
			t.Errorf("part %d sent %.2f s, planned %.2f s within the engine's %g s", i+1, l, run.Parts[i].Length, echo.max)
		}
	}
	result, err := st.media(ctx, run.ResultID)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(result.Seconds-src.Seconds) > 0.1 || result.Width != 320 || result.Height != 568 {
		t.Errorf("joined %.2f s at %dx%d, want %.2f s at 320x568", result.Seconds, result.Width, result.Height, src.Seconds)
	}
	if _, err := fr.run(ctx, "-i", lib.path(result), "-map", "0:a:0", "-t", "0.1", "-f", "null", "-"); err != nil {
		t.Errorf("the joined result has no sound: %v", err)
	}
	list, err := st.listMedia(ctx, kindVideo, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Errorf("the clip library shows %d videos, want only the clip: parts must stay out of it", len(list))
	}
	_, owned, err := st.deleteRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(owned) != 7 {
		t.Errorf("deleting the run removes %d files, want the result and each part's segment and result (7)", len(owned))
	}
}

func TestAThirtySecondClipIsAnythingUnderThirtyOne(t *testing.T) {
	l := limits{MaxSeconds: 15}
	clip := func(s float64) task {
		return task{Source: Media{Kind: kindVideo, Seconds: s, Status: mediaReady}, Parts: true}
	}
	for _, s := range []float64{30, 30.04, 30.9} {
		if f := validate(l, clip(s)); f["source"] != nil {
			t.Errorf("a %g s clip was refused: %v", s, f["source"])
		}
		if parts := planParts(l, s); len(parts) == 0 {
			t.Errorf("a %g s clip planned no parts", s)
		}
	}
	if f := validate(l, clip(31)); f["source"] == nil {
		t.Error("a 31 s clip passed")
	}
	// An engine that takes 30 s trims a 30.04 s clip rather than billing
	// two parts for 0.04 s of it.
	if parts := planParts(limits{MaxSeconds: 30}, 30.04); len(parts) != 1 || parts[0].Length != 30-partMargin {
		t.Errorf("a 30.04 s clip on a 30 s engine planned %v, want one part trimmed to %g s", parts, 30-partMargin)
	}
	if parts := planParts(limits{MaxSeconds: 15}, 16); len(parts) != 2 {
		t.Errorf("a clip a whole second over should run in parts, got %d", len(parts))
	}
}

// theClipsCuts are the shot cuts ffmpeg found in the 30 s two-performer clip
// whose first run in parts lost a performer at a cut.
var theClipsCuts = []float64{1.134462, 8.908906, 16.449774, 19.986632, 20.987639, 27.160469}

func TestPartsSplitAtTheClipsOwnCuts(t *testing.T) {
	l := limits{MaxSeconds: 15, MinSeconds: 2}
	plan := atCuts(l, 0, planParts(l, 30), 30, theClipsCuts)
	want := []float64{0, 8.908906, 19.986632}
	if len(plan) != len(want) {
		t.Fatalf("%d parts, want %d: %v", len(plan), len(want), plan)
	}
	for i, p := range plan {
		if math.Abs(p.Start-want[i]) > 1e-6 || p.Length > l.MaxSeconds-partMargin {
			t.Errorf("part %d: %.3f s for %.3f s, want it to start at the cut at %.3f s within the limit", i+1, p.Start, p.Length, want[i])
		}
		if i > 0 && shared(plan[i-1], p) != 0 {
			t.Errorf("part %d shares %.3f s with the one before, want a straight cut", i+1, shared(plan[i-1], p))
		}
	}
	if last := plan[len(plan)-1]; math.Abs(last.Start+last.Length-30) > 1e-9 {
		t.Errorf("the plan ends at %.3f s, want 30", last.Start+last.Length)
	}
	// With no cut in reach, the even plan's overlaps stand.
	if got := atCuts(l, 0, planParts(l, 30), 30, []float64{0.5, 29.8}); shared(got[0], got[1]) < partOverlap-1e-9 {
		t.Errorf("without usable cuts the parts should overlap, got %v", got)
	}
	// A model whose shortest output is 4 s gets no 2 s part from a cut.
	for _, p := range atCuts(l, 4, planParts(l, 30), 30, []float64{2, 12, 28}) {
		if p.Length < 4 {
			t.Errorf("a %.2f s part for a model whose shortest output is 4 s", p.Length)
		}
	}
}

func TestJoinGraphJoinsStraightAtCuts(t *testing.T) {
	parts := []part{{Start: 0, Length: 8}, {Start: 8, Length: 7.5}, {Start: 15.5, Length: 6}}
	graph, out := joinGraph(parts, []float64{8, 7.5, 6}, 720, 1280)
	if strings.Contains(graph, "xfade") || strings.Count(graph, "concat=n=2:v=1:a=0") != 2 || out != "x2" {
		t.Errorf("parts that meet at cuts should be joined straight:\n%s", graph)
	}
	if graph, out := joinGraph([]part{{Length: 14.75}}, []float64{15}, 720, 1280); out != "p0" || strings.Contains(graph, "xfade") {
		t.Errorf("one trimmed part needs no join: %s -> %s", graph, out)
	}
}

func TestEveryPartOfARunSharesItsSeedAndVersionsDiffer(t *testing.T) {
	a, b := runSeed("utsushi-batch-a"), runSeed("utsushi-batch-b")
	if a <= 0 || a != runSeed("utsushi-batch-a") || a == b {
		t.Errorf("seeds %d and %d: want a positive seed per run, stable, and different per version", a, b)
	}
	if got := (task{Seed: a}).seed(1 << 31); got != a%(1<<31) {
		t.Errorf("a task with seed %d drew %d, want its own seed", a, got)
	}
}

func TestAClipRunsInPartsSplitAtItsCutAndCarriesTheCastForward(t *testing.T) {
	ctx := context.Background()
	fr, err := newFrames(filepath.Join(t.TempDir(), "cache"))
	if err != nil {
		t.Fatal(err)
	}
	if !fr.available() {
		t.Skip("ffmpeg is not installed")
	}
	st, lib := newTestStore(t), newTestLibrary(t)
	clipPath := filepath.Join(t.TempDir(), "clip.mp4")
	// Two shots, cut at 6 s.
	if err := fr.encode(ctx, "-f", "lavfi", "-i", "testsrc=size=320x568:rate=24:duration=6",
		"-f", "lavfi", "-i", "smptebars=size=320x568:rate=24:duration=6", "-f", "lavfi", "-i", "sine=frequency=440:duration=12",
		"-filter_complex", "[0:v][1:v]concat=n=2:v=1:a=0[v]", "-map", "[v]", "-map", "2:a",
		"-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac", "-movflags", "+faststart", "-y", clipPath); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(clipPath)
	if err != nil {
		t.Fatal(err)
	}
	src, err := lib.store(f, originUpload, "clip.mp4", 0)
	if cerr := f.Close(); cerr != nil {
		t.Fatal(cerr)
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := st.addMedia(ctx, src); err != nil {
		t.Fatal(err)
	}
	cuts, err := fr.cuts(ctx, lib.path(src))
	if err != nil {
		t.Fatal(err)
	}
	if len(cuts) != 1 || math.Abs(cuts[0]-6) > 0.1 {
		t.Fatalf("found cuts %v, want one at 6 s", cuts)
	}
	echo := &echoEngine{lib: lib, max: 8}
	plan := atCuts(echo.info().Limits, 0, planParts(echo.info().Limits, src.Seconds), src.Seconds, cuts)
	if len(plan) != 2 || math.Abs(plan[1].Start-cuts[0]) > 1e-9 {
		t.Fatalf("plan %v, want two parts split at the cut", plan)
	}
	refs, err := newLocalRefs("http://127.0.0.1:1", "")
	if err != nil {
		t.Fatal(err)
	}
	rn := newRunner(st, lib, &engines{list: []engine{echo}}, refs, &recorder{})
	rn.frames = fr
	t.Cleanup(rn.shutdown)
	run := Run{ID: newID(), Batch: "c", Mode: modeMotion, Engine: "echo", Resolution: "480p", SourceID: src.ID,
		RefIDs: []string{}, Prompt: "Recast them.", Status: runPreparing, IdempotencyKey: "utsushi-c-a", CreatedAt: now(), Parts: plan}
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
			t.Fatalf("still %s after two minutes: %s", run.Status, run.Detail)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if run.Status != runSucceeded {
		t.Fatalf("run %s: %s", run.Status, run.Error)
	}
	echo.mu.Lock()
	prompts, nrefs, seeds := echo.prompts, echo.refs, echo.seeds
	echo.mu.Unlock()
	if prompts[0] != "Recast them." || nrefs[0] != 0 {
		t.Errorf("part 1 should get the run's own inputs, got %q with %d references", prompts[0], nrefs[0])
	}
	if nrefs[1] != 1 || !strings.Contains(prompts[1], "@1 is a frame from just before this shot") {
		t.Errorf("part 2 should get the last part's frame as @1, got %q with %d references", prompts[1], nrefs[1])
	}
	if seeds[0] == 0 || seeds[0] != seeds[1] {
		t.Errorf("the parts' seeds %v should be one seed", seeds)
	}
	result, err := st.media(ctx, run.ResultID)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(result.Seconds-src.Seconds) > 0.1 {
		t.Errorf("joined %.2f s, want %.2f s", result.Seconds, src.Seconds)
	}
}

func TestATakeRendersWithItsPinnedSeed(t *testing.T) {
	if got := seedOf(Run{Seed: 42, IdempotencyKey: "utsushi-x-a"}); got != 42 {
		t.Errorf("a stored seed should win: %d", got)
	}
	if got := seedOf(Run{IdempotencyKey: "utsushi-x-a"}); got != runSeed("utsushi-x-a") {
		t.Errorf("a run from before seeds were stored keeps its key's seed: %d", got)
	}
	ctx := context.Background()
	st := newTestStore(t)
	src := Media{ID: newID(), Kind: kindVideo, Origin: originUpload, File: "a.mp4", Status: mediaReady, CreatedAt: now()}
	if err := st.addMedia(ctx, src); err != nil {
		t.Fatal(err)
	}
	run := Run{ID: newID(), Batch: "b", Mode: modeMotion, Engine: "x", SourceID: src.ID, RefIDs: []string{},
		Status: runPreparing, IdempotencyKey: "utsushi-new-a", CreatedAt: now(), Seed: 777}
	if err := st.addRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	back, err := st.run(ctx, run.ID)
	if err != nil || back.Seed != 777 {
		t.Fatalf("the seed should be stored: %d, %v", back.Seed, err)
	}
	rn := &runner{st: st}
	tk, err := rn.task(ctx, back)
	if err != nil || tk.Seed != 777 {
		t.Errorf("the run should render with its pinned seed, got %d (%v)", tk.Seed, err)
	}
}
