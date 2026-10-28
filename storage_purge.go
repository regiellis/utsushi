package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/regiellis/hyperchi/hyperchi"
	"github.com/regiellis/hyperchi/hyperchi/database"
)

var errPurgeBusy = errors.New("wait for pending work to finish and collect or cancel unresolved runs before purging")

// storageAccess keeps new uploads and submissions out while a purge runs.
func (w *web) storageAccess(next http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if r.URL.Path == "/settings/storage/purge" {
				w.storageMu.Lock()
				defer w.storageMu.Unlock()
			} else {
				w.storageMu.RLock()
				defer w.storageMu.RUnlock()
			}
		}
		next.ServeHTTP(rw, r)
	})
}

func (w *web) settingsStoragePurge(c *hyperchi.Context) error {
	if err := c.Request().ParseForm(); err != nil {
		return err
	}
	fail := func(message string) error {
		return c.Fragment("notice", hyperchi.H{"message": message})
	}
	if c.Request().PostForm.Get("purge_confirm") != "PURGE" {
		return fail("Type PURGE exactly to delete library storage.")
	}
	if err := w.purgeStorage(c.Ctx()); err != nil {
		if errors.Is(err, errPurgeBusy) {
			return fail(err.Error())
		}
		return fail("Purge did not finish. Some files may already be removed. Retry to finish: " + err.Error())
	}
	// Reload the studio so no deleted cards or selections remain on screen.
	c.Response().Header().Set("HX-Redirect", "/")
	return c.NoContent()
}

// purgeStorage only removes registered library files and previews. Model files,
// exports, preferences and credentials live outside that scope. On a filesystem
// failure the transaction rolls back, retaining the file list so a retry can finish.
func (w *web) purgeStorage(ctx context.Context) error {
	w.uploadMu.Lock()
	defer w.uploadMu.Unlock()
	return w.st.db.TxContext(ctx, func(tx *database.Tx) error {
		// Acquire SQLite's write lock before checking work or touching files.
		if _, err := tx.Exec(`UPDATE studio SET value = value WHERE key = 'clip'`); err != nil {
			return err
		}
		var busy int
		if err := tx.QueryRow(`SELECT
   (SELECT COUNT(*) FROM runs WHERE status IN ('preparing','queued','running','lost','interrupted')) +
   (SELECT COUNT(*) FROM media WHERE status = 'pending')`).Scan(&busy); err != nil {
			return err
		}
		if busy > 0 {
			return errPurgeBusy
		}
		rows, err := tx.Query(`SELECT ` + mediaColumns + ` FROM media`)
		if err != nil {
			return err
		}
		media, err := collect(rows, scanMedia)
		if err != nil {
			return err
		}
		for _, m := range media {
			if err := w.lib.remove(m); err != nil {
				return fmt.Errorf("remove library file %s: %w", m.ID, err)
			}
		}
		if err := w.frames.clear(); err != nil {
			return fmt.Errorf("clear previews: %w", err)
		}
		storyRows, err := tx.Query(`SELECT document FROM story_projects`)
		if err != nil {
			return err
		}
		projects, err := collect(storyRows, func(row interface{ Scan(...any) error }) (storyProject, error) {
			var raw string
			var p storyProject
			if err := row.Scan(&raw); err != nil {
				return p, err
			}
			err := json.Unmarshal([]byte(raw), &p)
			return p, err
		})
		if err != nil {
			return err
		}
		for _, p := range projects {
			for i := range p.Assets {
				p.Assets[i].MediaID = ""
			}
			for i := range p.Episodes {
				for j := range p.Episodes[i].Shots {
					p.Episodes[i].Shots[j].MediaID = ""
					p.Episodes[i].Shots[j].Takes = nil
				}
			}
			raw, err := json.Marshal(p)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(`UPDATE story_projects SET document=? WHERE id=?`, string(raw), p.ID); err != nil {
				return err
			}
		}
		for _, query := range []string{`DELETE FROM runs`, `DELETE FROM media`, `DELETE FROM studio`} {
			if _, err := tx.Exec(query); err != nil {
				return err
			}
		}
		return nil
	})
}
