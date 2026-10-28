package main

import (
	"bytes"
	"context"
	"github.com/go-chi/chi/v5"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
)

func postUpload(t *testing.T, w *web, data []byte, name string) {
	t.Helper()
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	file, err := form.CreateFormFile("file", name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/media?kind=video", &body)
	req.Header.Set("Content-Type", form.FormDataContentType())
	rec := httptest.NewRecorder()
	if err := w.upload(w.app.NewContext(rec, req)); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("upload: %d %s", rec.Code, rec.Body.String())
	}
}

func TestRepeatedUploadReusesFileAndSelection(t *testing.T) {
	w := newToolsWeb(t)
	postUpload(t, w, fakeMP4(2), "clip.mp4")
	first := openedClip(t, w, "")
	postUpload(t, w, fakeMP4(2), "renamed.mp4")
	if got := openedClip(t, w, ""); got != first {
		t.Fatalf("duplicate selected %s, want existing %s", got, first)
	}
	items, err := w.st.listMedia(t.Context(), kindVideo, 100)
	if err != nil {
		t.Fatal(err)
	}
	files, err := os.ReadDir(w.lib.dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || len(files) != 1 {
		t.Fatalf("duplicate left %d rows and %d files", len(items), len(files))
	}
	postUpload(t, w, fakeMP4(3), "clip.mp4")
	if got := openedClip(t, w, ""); got == first {
		t.Fatal("different content reused old clip")
	}
}

func TestUseAsSourceReusesItsIndependentCopy(t *testing.T) {
	w := newToolsWeb(t)
	result, err := w.lib.store(bytes.NewReader(fakeMP4(2)), originResult, "result.mp4", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.st.addMedia(t.Context(), result); err != nil {
		t.Fatal(err)
	}
	run := Run{ID: newID(), Batch: "b", Mode: modeMotion, Engine: "test", RefIDs: []string{},
		ResultID: result.ID, Status: runSucceeded, IdempotencyKey: "key", CreatedAt: now()}
	if err := w.st.addRun(t.Context(), run); err != nil {
		t.Fatal(err)
	}
	if err := w.st.saveRun(t.Context(), run); err != nil {
		t.Fatal(err)
	}
	var location string
	for i := range 2 {
		req := httptest.NewRequest(http.MethodPost, "/runs/"+run.ID+"/source", nil)
		route := chi.NewRouteContext()
		route.URLParams.Add("id", run.ID)
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, route))
		rec := httptest.NewRecorder()
		if err := w.useAsSource(w.app.NewContext(rec, req)); err != nil {
			t.Fatal(err)
		}
		next := rec.Header().Get("Location")
		if next == "" {
			t.Fatal("missing source redirect")
		}
		if i > 0 && next != location {
			t.Fatalf("repeated source changed %q to %q", location, next)
		}
		location = next
	}
	clips, err := w.st.listMedia(t.Context(), kindVideo, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(clips) != 1 || clips[0].File == result.File {
		t.Fatalf("source must be one independent copy: %+v", clips)
	}
}

func TestConcurrentUploadsKeepOneCopy(t *testing.T) {
	w := newToolsWeb(t)
	var wg sync.WaitGroup
	ids := make(chan string, 4)
	for range 4 {
		wg.Go(func() {
			fresh, err := w.lib.store(bytes.NewReader(fakeMP4(2)), originUpload, "clip.mp4", 0)
			if err != nil {
				t.Error(err)
				return
			}
			kept, err := w.keepUpload(t.Context(), fresh)
			if err != nil {
				t.Error(err)
				return
			}
			ids <- kept.ID
		})
	}
	wg.Wait()
	close(ids)
	unique := map[string]bool{}
	for id := range ids {
		unique[id] = true
	}
	files, err := os.ReadDir(w.lib.dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(unique) != 1 || len(files) != 1 {
		t.Fatalf("concurrent upload kept %d IDs and %d files", len(unique), len(files))
	}
}
