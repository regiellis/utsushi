package main

import (
	"context"
	"testing"
	"time"
)

// openedClip is the id of the clip a studio page opens with, empty for none.
func openedClip(t *testing.T, w *web, asked string) string {
	t.Helper()
	data, err := w.pageClip(context.Background(), asked)
	if err != nil {
		t.Fatal(err)
	}
	if c, ok := data["clip"].(*mediaView); ok && c != nil {
		return c.ID
	}
	return ""
}

func TestStudioOpensWithTheLastClipChosenAndARemovedOneStaysRemoved(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	w := &web{st: st}
	base := now().Add(-time.Hour)
	older := Media{ID: newID(), Kind: kindVideo, Origin: originUpload, File: "a.mp4", Status: mediaReady, CreatedAt: base}
	newer := Media{ID: newID(), Kind: kindVideo, Origin: originUpload, File: "b.mp4", Status: mediaReady, CreatedAt: base.Add(time.Minute)}
	for _, m := range []Media{older, newer} {
		if err := st.addMedia(ctx, m); err != nil {
			t.Fatal(err)
		}
	}

	if got := openedClip(t, w, ""); got != newer.ID {
		t.Fatalf("first visit opened %q, want the newest clip", got)
	}
	if err := w.rememberClip(ctx, older.ID); err != nil {
		t.Fatal(err)
	}
	if got := openedClip(t, w, ""); got != older.ID {
		t.Fatalf("after choosing the older clip, a reload opened %q", got)
	}
	if err := w.rememberClip(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if got := openedClip(t, w, ""); got != "" {
		t.Fatalf("after Remove, a reload opened %q, want no clip", got)
	}
	if got := openedClip(t, w, newer.ID); got != newer.ID {
		t.Fatalf("?clip= opened %q", got)
	}
	if got := openedClip(t, w, ""); got != newer.ID {
		t.Fatalf("a clip opened by link was not remembered: got %q", got)
	}
	if err := w.rememberClip(ctx, "gone"); err != nil {
		t.Fatal(err)
	}
	if got := openedClip(t, w, ""); got != newer.ID {
		t.Fatalf("a remembered clip that no longer exists opened %q, want the newest", got)
	}
}

func TestDeletingTheCurrentClipLeavesNoneRatherThanTheNext(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	w := &web{st: st}
	base := now().Add(-time.Hour)
	older := Media{ID: newID(), Kind: kindVideo, Origin: originUpload, File: "a.mp4", Status: mediaReady, CreatedAt: base}
	newer := Media{ID: newID(), Kind: kindVideo, Origin: originUpload, File: "b.mp4", Status: mediaReady, CreatedAt: base.Add(time.Minute)}
	for _, m := range []Media{older, newer} {
		if err := st.addMedia(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.rememberClip(ctx, newer.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := w.forgetMedia(ctx, older.ID); err != nil {
		t.Fatal(err)
	}
	if got := openedClip(t, w, ""); got != newer.ID {
		t.Fatalf("deleting another clip changed the current one to %q", got)
	}
	if _, err := w.forgetMedia(ctx, newer.ID); err != nil {
		t.Fatal(err)
	}
	if got := openedClip(t, w, ""); got != "" {
		t.Fatalf("after deleting the current clip, the page opened %q, want none", got)
	}
}

func TestReusePromptFillsTheStudioOfTheRunsModeAndStartsNothing(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	w := &web{st: st}
	src := Media{ID: newID(), Kind: kindVideo, Origin: originUpload, File: "a.mp4", Status: mediaReady, CreatedAt: now()}
	if err := st.addMedia(ctx, src); err != nil {
		t.Fatal(err)
	}
	r := Run{ID: newID(), Batch: "b", Mode: modeMotion, Engine: "x", Prompt: "Recast them as @1.", SourceID: src.ID,
		RefIDs: []string{}, Status: runSucceeded, IdempotencyKey: "k", CreatedAt: now()}
	if err := st.addRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ mode, id, want string }{
		{modeMotion, r.ID, r.Prompt}, {modeSwap, r.ID, ""}, {modeMotion, "gone", ""}, {modeMotion, "", ""},
	} {
		got, err := w.reusedPrompt(ctx, c.mode, c.id)
		if err != nil {
			t.Fatal(err)
		}
		if got != c.want {
			t.Errorf("%s studio reusing %q: %q, want %q", c.mode, c.id, got, c.want)
		}
	}
	runs, err := st.listRuns(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Errorf("reusing a prompt left %d runs, want the one it read", len(runs))
	}
}

func TestClipBrowserIncludesOlderFiles(t *testing.T) {
	st := newTestStore(t)
	w := &web{st: st}
	var oldest string
	for i := range 75 {
		m := Media{ID: newID(), Kind: kindVideo, Origin: originUpload, File: "clip.mp4",
			Status: mediaReady, CreatedAt: now().Add(time.Duration(i) * time.Second)}
		if i == 0 {
			oldest = m.ID
		}
		if err := st.addMedia(t.Context(), m); err != nil {
			t.Fatal(err)
		}
	}
	data, err := w.clipData(t.Context(), oldest, false)
	if err != nil {
		t.Fatal(err)
	}
	others := data["clips"].([]mediaView)
	if len(others) != 74 {
		t.Fatalf("browser exposes %d other clips, want 74", len(others))
	}
	if data["clip"].(*mediaView).ID != oldest {
		t.Fatal("older selection was lost")
	}
}
