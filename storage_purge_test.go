package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStoragePurgeRequiresExactConfirmation(t *testing.T) {
	for _, confirmation := range []string{"", "purge", " PURGE", "PURGE ", "DELETE"} {
		t.Run(confirmation, func(t *testing.T) {
			w := newToolsWeb(t)
			w.app.Post("/purge", w.settingsStoragePurge)
			req := httptest.NewRequest(http.MethodPost, "/purge", strings.NewReader(url.Values{"purge_confirm": {confirmation}}.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			rec := httptest.NewRecorder()
			w.app.ServeHTTP(rec, req)
			if !strings.Contains(rec.Body.String(), "Type PURGE exactly") || rec.Header().Get("HX-Redirect") != "" {
				t.Fatalf("confirmation accepted: %d %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestStoragePurgeRefusesPendingWork(t *testing.T) {
	for _, state := range []string{runPreparing, runQueued, runRunning, runLost, runInterrupted, mediaPending} {
		t.Run(state, func(t *testing.T) {
			w := newToolsWeb(t)
			ctx := context.Background()
			m, err := w.lib.store(bytes.NewReader(fakePNG(t)), originUpload, "reference", 0)
			if err != nil {
				t.Fatal(err)
			}
			if state == mediaPending {
				m.Status = mediaPending
			}
			if err := w.st.addMedia(ctx, m); err != nil {
				t.Fatal(err)
			}
			if state != mediaPending {
				if err := w.st.addRun(ctx, Run{ID: newID(), Batch: "b", Mode: modeMotion, Engine: "test", SourceID: m.ID, Status: state, IdempotencyKey: newID(), CreatedAt: now()}); err != nil {
					t.Fatal(err)
				}
			}
			if err := w.purgeStorage(ctx); !errors.Is(err, errPurgeBusy) {
				t.Fatalf("got %v", err)
			}
			if _, err := os.Stat(w.lib.path(m)); err != nil {
				t.Fatal("file removed despite pending work:", err)
			}
			if _, err := w.st.media(ctx, m.ID); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestStoragePurgeRemovesLibraryAndKeepsOtherStorage(t *testing.T) {
	w := newToolsWeb(t)
	ctx := context.Background()
	m, err := w.lib.store(bytes.NewReader(fakePNG(t)), originUpload, "reference", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.st.addMedia(ctx, m); err != nil {
		t.Fatal(err)
	}
	if err := w.st.addRun(ctx, Run{ID: newID(), Batch: "b", Mode: modeMotion, Engine: "test", SourceID: m.ID, Status: runSucceeded, IdempotencyKey: newID(), CreatedAt: now()}); err != nil {
		t.Fatal(err)
	}
	if err := w.st.setStudioValue(ctx, "clip", m.ID); err != nil {
		t.Fatal(err)
	}
	project := storyFixture()
	project.Assets = []storyAsset{{ID: "maya", Name: "Maya", MediaID: m.ID}}
	project.Episodes[0].Shots[0].MediaID = m.ID
	project.Episodes[0].Shots[0].Takes = []string{"run"}
	project.Episodes[0].Shots[0].Prompt = "Keep the courier in frame."
	if err := w.st.addStory(ctx, project); err != nil {
		t.Fatal(err)
	}
	preview := filepath.Join(w.frames.cache, m.ID+"-poster.jpg")
	if err := os.WriteFile(preview, []byte("preview"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Unregistered files are outside the purge's explicit file list.
	keep := filepath.Join(w.lib.dir, "keep.txt")
	if err := os.WriteFile(keep, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	w.app.Post("/purge", w.settingsStoragePurge)
	req := httptest.NewRequest(http.MethodPost, "/purge", strings.NewReader("purge_confirm=PURGE"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	w.app.ServeHTTP(rec, req)
	if rec.Header().Get("HX-Redirect") != "/" {
		t.Fatalf("purge failed: %d %s", rec.Code, rec.Body.String())
	}
	for _, path := range []string{w.lib.path(m), preview} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("file remains: %s (%v)", path, err)
		}
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"media", "runs", "studio"} {
		var count int
		if err := w.st.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("%s still contains %d rows", table, count)
		}
	}
	saved, err := w.st.storyProject(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	shot := saved.Episodes[0].Shots[0]
	if saved.Title != project.Title || shot.Prompt != project.Episodes[0].Shots[0].Prompt {
		t.Fatal("purge removed written planning")
	}
	if saved.Assets[0].MediaID != "" || shot.MediaID != "" || len(shot.Takes) != 0 {
		t.Fatal("purge left deleted media or run references")
	}
	if err := w.purgeStorage(ctx); err != nil {
		t.Fatal("repeat purge:", err)
	}
}
