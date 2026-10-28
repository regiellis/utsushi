package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/regiellis/hyperchi/hyperchi/database"
)

func TestAnOldDatabaseLetsARunHaveNoSource(t *testing.T) {
	ctx := context.Background()
	cfg := database.DefaultDBConfig()
	cfg.Path = filepath.Join(t.TempDir(), "old.db")
	db, err := database.NewDB(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	// The first release's schema, with a run on it.
	for _, m := range migrations {
		m = strings.Replace(m, "source_id       TEXT REFERENCES media (id)", "source_id       TEXT NOT NULL REFERENCES media (id)", 1)
		if _, err := db.Exec(m); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO media (id, kind, origin, status, created_at) VALUES ('clip', 'video', 'upload', 'ready', '2026-10-01T00:00:00.000Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO runs (id, batch, mode, engine, prompt, resolution, source_id, refs, status, idempotency_key, created_at)
		VALUES ('old', 'b', 'motion', 'x', 'p', '480p', 'clip', '[]', 'succeeded', 'k', '2026-10-01T00:00:00.000Z')`); err != nil {
		t.Fatal(err)
	}

	st, err := newStore(db)
	if err != nil {
		t.Fatal(err)
	}
	old, err := st.run(ctx, "old")
	if err != nil || old.SourceID != "clip" || old.Prompt != "p" {
		t.Fatalf("the old run after the rebuild: %+v, %v", old, err)
	}
	long := Run{ID: newID(), Batch: "b", Mode: "long", Engine: "x", Prompt: "p", RefIDs: []string{}, Status: runPreparing,
		IdempotencyKey: "k2", CreatedAt: now(), Width: 480, Height: 864, Quality: "turbo"}
	if err := st.addRun(ctx, long); err != nil {
		t.Fatalf("a run with no source was refused: %v", err)
	}
	got, err := st.run(ctx, long.ID)
	if err != nil || got.SourceID != "" || got.Width != 480 || got.Quality != "turbo" {
		t.Fatalf("the sourceless run read back as %+v, %v", got, err)
	}
	bad := long
	bad.ID, bad.SourceID, bad.IdempotencyKey = newID(), "missing", "k3"
	if err := st.addRun(ctx, bad); err == nil {
		t.Error("a run naming a missing source was stored: foreign keys must still hold")
	}
	// Opening again leaves the rebuilt table alone.
	if _, err := newStore(db); err != nil {
		t.Fatal(err)
	}
}
