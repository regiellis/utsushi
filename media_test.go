package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"
)

func TestMP4SecondsReadsHeaderAfterMediaData(t *testing.T) {
	got, err := mp4Seconds(bytes.NewReader(fakeMP4(7)))
	if err != nil {
		t.Fatal(err)
	}
	if got != 7 {
		t.Fatalf("seconds = %v, want 7", got)
	}
}

func TestMP4SizeReadsTheVideoTrack(t *testing.T) {
	w, h, err := mp4Size(bytes.NewReader(fakeMP4(3)))
	if err != nil {
		t.Fatal(err)
	}
	if w != 1280 || h != 720 {
		t.Fatalf("size %dx%d, want 1280x720", w, h)
	}
	audioOnly := box("moov", mvhd0(1000, 1000), trak("soun", 0, 0))
	if _, _, err := mp4Size(bytes.NewReader(audioOnly)); err == nil {
		t.Fatal("a file with no video track has a size")
	}
}

func TestMP4SecondsRejectsTruncatedFile(t *testing.T) {
	data := fakeMP4(7)
	if _, err := mp4Seconds(bytes.NewReader(data[:len(data)-40])); err == nil {
		t.Fatal("a truncated movie header read as valid")
	}
}

func TestLibraryStoreSniffsAndMeasures(t *testing.T) {
	lib := newTestLibrary(t)
	m, err := lib.store(bytes.NewReader(fakeMP4(12)), originUpload, "clip.mp4", 0)
	if err != nil {
		t.Fatal(err)
	}
	if m.Kind != kindVideo || m.ContentType != "video/mp4" || m.Seconds != 12 {
		t.Fatalf("stored %+v", m)
	}
	if _, err := os.Stat(lib.path(m)); err != nil {
		t.Fatal(err)
	}

	img, err := lib.store(bytes.NewReader(fakePNG(t)), originUpload, "ref.png", 0)
	if err != nil {
		t.Fatal(err)
	}
	if img.Kind != kindImage || img.File != img.ID+".png" {
		t.Fatalf("stored %+v", img)
	}
}

func TestLibraryStoreRefusesOtherTypesAndLeavesNoFile(t *testing.T) {
	lib := newTestLibrary(t)
	if _, err := lib.store(bytes.NewReader([]byte("<html><script>x</script></html>")), originUpload, "x.mp4", 0); !errors.Is(err, errMediaType) {
		t.Fatalf("err = %v, want errMediaType", err)
	}
	if _, err := lib.store(bytes.NewReader(fakePNG(t)), originUpload, "big.png", 100); !errors.As(err, new(errTooLarge)) {
		t.Fatalf("err = %v, want errTooLarge", err)
	}
	entries, err := os.ReadDir(lib.dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("refused uploads left %d file(s)", len(entries))
	}
}

func TestDeleteMediaRefusesWhileARunUsesIt(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	src := Media{ID: newID(), Kind: kindVideo, Origin: originUpload, File: "a.mp4", Status: mediaReady, CreatedAt: now()}
	ref := Media{ID: newID(), Kind: kindImage, Origin: originUpload, File: "b.png", Status: mediaReady, CreatedAt: now()}
	spare := Media{ID: newID(), Kind: kindImage, Origin: originUpload, File: "c.png", Status: mediaReady, CreatedAt: now()}
	for _, m := range []Media{src, ref, spare} {
		if err := st.addMedia(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	run := Run{ID: newID(), Batch: "b", Mode: modeMotion, Engine: "genjutsu", SourceID: src.ID,
		RefIDs: []string{ref.ID}, Status: runFailed, IdempotencyKey: "k", CreatedAt: now()}
	if err := st.addRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{src.ID, ref.ID} {
		if _, err := st.deleteMedia(ctx, id); !errors.Is(err, errInUse) {
			t.Fatalf("delete %s: err = %v, want errInUse", id, err)
		}
	}
	if _, err := st.deleteMedia(ctx, spare.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.deleteRun(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.deleteMedia(ctx, ref.ID); err != nil {
		t.Fatalf("after the run is gone: %v", err)
	}
}

func TestValidateAgainstEngineLimits(t *testing.T) {
	clip := Media{Kind: kindVideo, Seconds: 12, Status: mediaReady}
	img := Media{Kind: kindImage, Status: mediaReady}
	genjutsu := newGenjutsuEngine(nil, nil).info().Limits
	wan := newWanEditEngine(nil, nil).info().Limits

	if f := validate(genjutsu, task{Source: clip, Refs: []Media{img}, Resolution: "720p"}); f != nil {
		t.Fatalf("valid Genjutsu task refused: %v", f)
	}
	f := validate(genjutsu, task{Source: clip, Resolution: "4k"})
	if f["refs"] == nil || f["resolution"] == nil {
		t.Fatalf("missing refs and bad resolution passed: %v", f)
	}
	f = validate(wan, task{Source: clip, Resolution: "720P"})
	if f["source"] == nil || f["prompt"] == nil {
		t.Fatalf("a 12 s clip and no prompt passed Wan's 10 s limit: %v", f)
	}
	if f := validate(wan, task{Source: Media{Kind: kindImage}, Prompt: "x", Resolution: "720P"}); f["source"] == nil {
		t.Fatalf("an image passed as the source clip: %v", f)
	}
}

func TestDownloadsAreNamedByTheLibrarysOwnExtension(t *testing.T) {
	for file, want := range map[string]string{"abc.mp4": "utsushi-m1.mp4", "abc.png": "utsushi-m1.png", "abc.mp3": "utsushi-m1.mp3"} {
		if got := downloadName(Media{ID: "m1", File: file, ContentType: "video/mp4"}); got != want {
			t.Errorf("%s downloads as %q, want %q (the system MIME list put .f4v first for video/mp4)", file, got, want)
		}
	}
}
