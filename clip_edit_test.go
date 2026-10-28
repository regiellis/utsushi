package main

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"testing"
)

func TestCleanCutsKeepsCutsInsideTheClipAndApart(t *testing.T) {
	got := cleanCuts([]float64{9.0004, 0.2, 3, 3.3, 11.8, 6}, 12)
	want := []float64{3, 6, 9}
	if len(got) != len(want) {
		t.Fatalf("cleaned %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("cleaned %v, want %v", got, want)
		}
	}
	if got := cleanCuts(nil, 10); got == nil || len(got) != 0 {
		t.Errorf("no cuts should clean to an empty list, which saves as 'no cuts': %v", got)
	}
}

func TestSavedCutsRoundTripAndResetToDetection(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	m := Media{ID: newID(), Kind: kindVideo, Origin: originUpload, File: "a.mp4", Status: mediaReady, CreatedAt: now(), Seconds: 12}
	if err := st.addMedia(ctx, m); err != nil {
		t.Fatal(err)
	}
	if _, saved, err := st.clipCuts(ctx, m.ID); err != nil || saved {
		t.Fatalf("a new clip has no saved cuts: %v %v", saved, err)
	}
	if err := st.setClipCuts(ctx, m.ID, []float64{4.5, 8}); err != nil {
		t.Fatal(err)
	}
	cuts, saved, err := st.clipCuts(ctx, m.ID)
	if err != nil || !saved || len(cuts) != 2 || cuts[0] != 4.5 {
		t.Fatalf("saved cuts read back as %v %v %v", cuts, saved, err)
	}
	if err := st.setClipCuts(ctx, m.ID, []float64{}); err != nil {
		t.Fatal(err)
	}
	if cuts, saved, _ := st.clipCuts(ctx, m.ID); !saved || len(cuts) != 0 {
		t.Errorf("an empty saved list means no cuts, not detection: %v %v", cuts, saved)
	}
	if err := st.setClipCuts(ctx, m.ID, nil); err != nil {
		t.Fatal(err)
	}
	if _, saved, _ := st.clipCuts(ctx, m.ID); saved {
		t.Error("a reset should hand the clip back to detection")
	}
}

func TestSplittingAClipMakesNewClipsAtTheCuts(t *testing.T) {
	ctx := context.Background()
	fr, err := newFrames(filepath.Join(t.TempDir(), "cache"))
	if err != nil {
		t.Fatal(err)
	}
	if !fr.available() {
		t.Skip("ffmpeg is not installed")
	}
	st, lib := newTestStore(t), newTestLibrary(t)
	path := filepath.Join(t.TempDir(), "clip.mp4")
	if err := fr.encode(ctx, "-f", "lavfi", "-i", "testsrc=size=320x568:rate=24:duration=9", "-f", "lavfi", "-i", "sine=duration=9",
		"-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac", "-movflags", "+faststart", "-y", path); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	clip, err := lib.store(f, originUpload, "dance.mp4", 0)
	if cerr := f.Close(); cerr != nil {
		t.Fatal(cerr)
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := st.addMedia(ctx, clip); err != nil {
		t.Fatal(err)
	}
	rn := newRunner(st, lib, &engines{}, nil, &recorder{})
	rn.frames = fr
	t.Cleanup(rn.shutdown)
	w := &web{st: st, lib: lib, frames: fr, run: rn}

	made, err := w.splitClip(ctx, clip, []float64{3, 6})
	if err != nil {
		t.Fatal(err)
	}
	if len(made) != 3 {
		t.Fatalf("made %d clips, want 3", len(made))
	}
	for i, m := range made {
		if math.Abs(m.Seconds-3) > 0.1 || m.Origin != originUpload || m.Label != "dance.mp4 · part "+string(rune('1'+i))+" of 3" {
			t.Errorf("part %d: %.2f s, origin %s, label %q", i+1, m.Seconds, m.Origin, m.Label)
		}
	}
	list, err := st.listMedia(ctx, kindVideo, 10)
	if err != nil || len(list) != 4 {
		t.Errorf("the library should show the clip and its 3 parts, shows %d (%v)", len(list), err)
	}
	trimmed, err := w.trimClip(ctx, clip, 2, 5)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(trimmed.Seconds-3) > 0.1 || trimmed.Label != "dance.mp4 · 2.0 s to 5.0 s" {
		t.Errorf("trim made %.2f s labeled %q, want 3 s", trimmed.Seconds, trimmed.Label)
	}
	for _, r := range [][2]float64{{2, 2.5}, {0, 9}} {
		if _, err := w.trimClip(ctx, clip, r[0], r[1]); err == nil {
			t.Errorf("a trim from %g to %g should be refused", r[0], r[1])
		}
	}
	for _, cuts := range [][]float64{{}, {2, 4, 6}, {0.6}} {
		if _, err := w.splitClip(ctx, clip, cuts); err == nil {
			t.Errorf("cuts %v should be refused: split makes 2 or 3 pieces of at least 1 s", cuts)
		}
	}
}
