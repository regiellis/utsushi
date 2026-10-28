package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/regiellis/hyperchi/hyperchi/database"
)

// Media kinds and origins.
const (
	kindVideo = "video"
	kindImage = "image"
	kindAudio = "audio"

	originUpload = "upload" // the visitor's own file
	originLocal  = "local"  // generated on this machine through the proxy
	originResult = "result" // a finished generation, downloaded and kept
	originStyle  = "style"  // a clip's first frame restyled for a Style run
	originSwatch = "swatch" // a style preset's sample tile; Label is the preset key
	originCloud  = "cloud"  // an image made on Comfy Cloud
	originPart   = "part"   // a segment of a long clip, or what an engine made of one
	originTool   = "tool"   // made on the Tools page from a library item; media.tool names the tool
)

// Media states. Only a locally generated reference is ever pending.
const (
	mediaReady   = "ready"
	mediaPending = "pending"
	mediaFailed  = "failed"
)

// Run states. A run that holds a request id when the server stops is
// collected on the next start, so queued and running survive a restart.
const (
	runPreparing   = "preparing"
	runQueued      = "queued"
	runRunning     = "running"
	runSucceeded   = "succeeded"
	runFailed      = "failed"
	runCanceled    = "canceled"
	runInterrupted = "interrupted"
	// runLost is a run the provider holds that the app lost contact with
	// after retrying. Collect, or the next start, follows it again.
	runLost = "lost"
)

func runActive(status string) bool {
	switch status {
	case runPreparing, runQueued, runRunning:
		return true
	}
	return false
}

var migrations = []string{
	`CREATE TABLE IF NOT EXISTS story_projects (id TEXT PRIMARY KEY, document TEXT NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS media (
		id           TEXT PRIMARY KEY,
		kind         TEXT NOT NULL,
		origin       TEXT NOT NULL,
		file         TEXT NOT NULL DEFAULT '',
		content_type TEXT NOT NULL DEFAULT '',
		size         INTEGER NOT NULL DEFAULT 0,
		label        TEXT NOT NULL DEFAULT '',
		seconds      REAL NOT NULL DEFAULT 0,
		status       TEXT NOT NULL,
		error        TEXT NOT NULL DEFAULT '',
		created_at   TEXT NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS media_kind_created ON media (kind, created_at DESC)`,
	`CREATE TABLE IF NOT EXISTS runs (
		id              TEXT PRIMARY KEY,
		batch           TEXT NOT NULL,
		mode            TEXT NOT NULL,
		engine          TEXT NOT NULL,
		prompt          TEXT NOT NULL,
		resolution      TEXT NOT NULL,
		source_id       TEXT REFERENCES media (id),
		refs            TEXT NOT NULL,
		status          TEXT NOT NULL,
		detail          TEXT NOT NULL DEFAULT '',
		progress        REAL NOT NULL DEFAULT 0,
		request_id      TEXT NOT NULL DEFAULT '',
		idempotency_key TEXT NOT NULL,
		result_id       TEXT REFERENCES media (id),
		credits         TEXT NOT NULL DEFAULT '',
		error           TEXT NOT NULL DEFAULT '',
		created_at      TEXT NOT NULL,
		finished_at     TEXT NOT NULL DEFAULT ''
	)`,
	`CREATE INDEX IF NOT EXISTS runs_created ON runs (created_at DESC)`,
	`CREATE TABLE IF NOT EXISTS studio (
		key   TEXT PRIMARY KEY,
		value TEXT NOT NULL
	)`,
}

// Media is one stored file, or a reference still being generated.
type Media struct {
	ID          string
	Kind        string
	Origin      string
	File        string
	ContentType string
	Size        int64
	Label       string
	Seconds     float64
	// Width and Height are the frame or image size in pixels, 0 when it
	// could not be read. Cards use them to show footage at its own shape.
	Width, Height int
	Status        string
	Error         string
	CreatedAt     time.Time
	// Note is a pending reference's live progress line. It is pushed to the
	// page and never stored.
	Note string
	// CostUSD is a made image's published price, counted toward the
	// monthly cap; zero for free and unpriced images.
	CostUSD float64
	// SourceID, Tool, and ToolForm are, for an item made on the Tools page,
	// what it was made from, which tool made it, and the order's options
	// (URL-encoded); empty otherwise.
	SourceID, Tool, ToolForm string
}

// Run is one generation: a source video, references, and an engine.
type Run struct {
	ID             string
	Batch          string
	Mode           string
	Engine         string
	Prompt         string
	Resolution     string
	SourceID       string
	RefIDs         []string
	Status         string
	Detail         string
	Progress       float64
	RequestID      string
	IdempotencyKey string
	// Style is a Style run's preset key, empty for other modes and for a
	// run styled from the visitor's own image. StyleImageID is the image
	// the engine is given as the style: the restyled first frame, made
	// before the provider is called, or the visitor's own image.
	Style        string
	StyleImageID string
	// CostUSD is the run's estimated cost, from a published per-second rate
	// and the output's length, recorded when it finishes. Zero for free
	// engines and for providers that publish no price.
	CostUSD float64
	// Parts are the segments a clip longer than the engine takes is run in,
	// empty when it runs whole. A long video's parts are its shots.
	Parts []part
	// Width, Height, and Quality are a long video's frame size and
	// "turbo" or "full"; zero and empty for other modes. CastView is
	// "front" when a long video's character sheets go to the model as
	// their first figure only.
	Width, Height     int
	Quality, CastView string
	// RefSize is how MiniMax H3 sizes a long video's cast images: "match"
	// scales them down to the video's area, "max" keeps up to 2048 px on the
	// short side, which its node says holds identity best.
	RefSize string
	// SoundtrackID is a library audio file laid over a long video's whole
	// length, and SoundtrackMode "replace" (it is the sound) or "mix" (under
	// the shots' own); empty for none.
	SoundtrackID, SoundtrackMode string
	// Seed is the run's seed, set when it is made and inherited by every
	// take after it (Run again, Reuse), so a change shows its own effect
	// rather than a new roll of the dice. Zero, for runs made before seeds
	// were stored, falls back to one from the idempotency key.
	Seed       int64
	ResultID   string
	Credits    string
	Error      string
	CreatedAt  time.Time
	FinishedAt time.Time
}

var errNotFound = errors.New("not found")

// errInUse is a media file a run still points at.
var errInUse = errors.New("in use by a run")

type store struct{ db *database.DB }

// newStore applies the schema. Every statement is IF NOT EXISTS, so it runs
// on each start. (AutoMigrate would log each statement in full.)
func newStore(db *database.DB) (*store, error) {
	for _, m := range migrations {
		if _, err := db.Exec(m); err != nil {
			return nil, fmt.Errorf("migrate: %w", err)
		}
	}
	// Columns added after the first release. SQLite has no ADD COLUMN IF
	// NOT EXISTS, so each is checked first.
	for _, col := range []string{"width", "height"} {
		if err := addColumn(db, "media", col, "INTEGER NOT NULL DEFAULT 0"); err != nil {
			return nil, fmt.Errorf("migrate: %w", err)
		}
	}
	for _, col := range []string{"style", "style_image"} {
		if err := addColumn(db, "runs", col, "TEXT NOT NULL DEFAULT ''"); err != nil {
			return nil, fmt.Errorf("migrate: %w", err)
		}
	}
	if err := addColumn(db, "media", "cost_usd", "REAL NOT NULL DEFAULT 0"); err != nil {
		return nil, err
	}
	if err := addColumn(db, "runs", "cost_usd", "REAL NOT NULL DEFAULT 0"); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	if err := addColumn(db, "runs", "parts", "TEXT NOT NULL DEFAULT '[]'"); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	for _, col := range []string{"width", "height"} {
		if err := addColumn(db, "runs", col, "INTEGER NOT NULL DEFAULT 0"); err != nil {
			return nil, fmt.Errorf("migrate: %w", err)
		}
	}
	for _, col := range []string{"quality", "cast_view", "ref_size", "soundtrack", "soundtrack_mode"} {
		if err := addColumn(db, "runs", col, "TEXT NOT NULL DEFAULT ''"); err != nil {
			return nil, fmt.Errorf("migrate: %w", err)
		}
	}
	// A clip's own cuts, as its editor saved them: '' until saved, so runs
	// in parts detect cuts instead.
	if err := addColumn(db, "media", "cuts", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	if err := addColumn(db, "runs", "seed", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	// What a Tools item was made from, and by which tool: '' otherwise. No
	// foreign key, so deleting the source leaves what was made from it.
	// tool_form is the order's options, URL-encoded, so a failed item can
	// be made again as it was asked for.
	for _, col := range []string{"source_id", "tool", "tool_form"} {
		if err := addColumn(db, "media", col, "TEXT NOT NULL DEFAULT ''"); err != nil {
			return nil, fmt.Errorf("migrate: %w", err)
		}
	}
	// Depth maps came first, with an origin of their own.
	if _, err := db.Exec(`UPDATE media SET origin = 'tool', tool = 'depth' WHERE origin = 'depth'`); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	if err := sourceOptional(db); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &store{db: db}, nil
}

// notNullSource matches the source column as the first release declared it.
var notNullSource = regexp.MustCompile(`source_id\s+TEXT NOT NULL REFERENCES`)

// sourceOptional lets runs.source_id be NULL, so a long video started from a
// prompt has no source. SQLite cannot drop NOT NULL in place, so this is its
// documented rebuild (sqlite.org/lang_altertable.html, "other kinds of table
// schema changes"): on one connection with foreign keys off, copy the rows
// into a table made from the old one's own CREATE text, drop, rename, and
// check the keys before committing. It runs once.
func sourceOptional(db *database.DB) error {
	ctx := context.Background()
	var create string
	if err := db.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'runs'`).Scan(&create); err != nil {
		return err
	}
	if !notNullSource.MatchString(create) {
		return nil
	}
	head := regexp.MustCompile(`(?i)^CREATE TABLE\s+(IF NOT EXISTS\s+)?"?runs"?`)
	create = head.ReplaceAllString(notNullSource.ReplaceAllString(create, "source_id TEXT REFERENCES"), "CREATE TABLE runs_new")
	conn, err := db.Raw().Conn(ctx)
	if err != nil {
		return err
	}
	exec := func(q string) error {
		_, err := conn.ExecContext(ctx, q)
		return err
	}
	err = func() error {
		if err := exec(`PRAGMA foreign_keys = OFF`); err != nil {
			return err
		}
		if err := exec(`BEGIN`); err != nil {
			return err
		}
		for _, q := range []string{create, `INSERT INTO runs_new SELECT * FROM runs`, `DROP TABLE runs`,
			`ALTER TABLE runs_new RENAME TO runs`, `CREATE INDEX IF NOT EXISTS runs_created ON runs (created_at DESC)`} {
			if err := exec(q); err != nil {
				return errors.Join(err, exec(`ROLLBACK`))
			}
		}
		var bad int
		if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_foreign_key_check`).Scan(&bad); err != nil || bad > 0 {
			return errors.Join(err, fmt.Errorf("%d broken references after rebuilding runs", bad), exec(`ROLLBACK`))
		}
		return exec(`COMMIT`)
	}()
	return errors.Join(err, exec(`PRAGMA foreign_keys = ON`), conn.Close())
}

func addColumn(db *database.DB, table, column, decl string) error {
	var n int
	if err := db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, table, column).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	_, err := db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + column + ` ` + decl)
	return err
}

func newID() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand only fails when the platform has no entropy source,
		// and nothing here can run safely without one.
		panic(fmt.Sprintf("crypto/rand: %v", err))
	}
	return hex.EncodeToString(b)
}

func now() time.Time { return time.Now().UTC().Truncate(time.Millisecond) }

func stamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func parseStamp(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339Nano, s)
}

const mediaColumns = `id, kind, origin, file, content_type, size, label, seconds, status, error, created_at, width, height, cost_usd, source_id, tool, tool_form`

func scanMedia(row interface{ Scan(...any) error }) (Media, error) {
	var m Media
	var created string
	if err := row.Scan(&m.ID, &m.Kind, &m.Origin, &m.File, &m.ContentType, &m.Size,
		&m.Label, &m.Seconds, &m.Status, &m.Error, &created, &m.Width, &m.Height, &m.CostUSD, &m.SourceID, &m.Tool, &m.ToolForm); err != nil {
		return Media{}, err
	}
	t, err := parseStamp(created)
	if err != nil {
		return Media{}, err
	}
	m.CreatedAt = t
	return m, nil
}

func (s *store) addMedia(ctx context.Context, m Media) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO media (`+mediaColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.ID, m.Kind, m.Origin, m.File, m.ContentType, m.Size, m.Label, m.Seconds,
		m.Status, m.Error, stamp(m.CreatedAt), m.Width, m.Height, m.CostUSD, m.SourceID, m.Tool, m.ToolForm)
	return err
}

// finishMedia records a pending reference as ready with its file, or as
// failed with the reason.
func (s *store) finishMedia(ctx context.Context, m Media) error {
	_, err := s.db.ExecContext(ctx, `UPDATE media
		SET file = ?, content_type = ?, size = ?, seconds = ?, status = ?, error = ?, width = ?, height = ?, cost_usd = ?
		WHERE id = ?`,
		m.File, m.ContentType, m.Size, m.Seconds, m.Status, m.Error, m.Width, m.Height, m.CostUSD, m.ID)
	return err
}

// unsized is ready media stored before sizes were recorded.
func (s *store) unsized(ctx context.Context) ([]Media, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+mediaColumns+` FROM media
		WHERE status = ? AND file != '' AND width = 0`, mediaReady)
	if err != nil {
		return nil, err
	}
	return collect(rows, scanMedia)
}

func (s *store) setSize(ctx context.Context, id string, w, h int) error {
	_, err := s.db.ExecContext(ctx, `UPDATE media SET width = ?, height = ? WHERE id = ?`, w, h, id)
	return err
}

// batchRuns are the versions started together with a run, oldest first.
func (s *store) batchRuns(ctx context.Context, batch string) ([]Run, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+runColumns+` FROM runs WHERE batch = ? ORDER BY created_at`, batch)
	if err != nil {
		return nil, err
	}
	return collect(rows, scanRun)
}

func (s *store) media(ctx context.Context, id string) (Media, error) {
	m, err := scanMedia(s.db.QueryRowContext(ctx, `SELECT `+mediaColumns+` FROM media WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Media{}, errNotFound
	}
	return m, err
}

// listMedia is the newest media of one kind that the visitor picks from.
// Results, restyled frames, and preset swatches show with their run or
// style, not in the library.
func (s *store) listMedia(ctx context.Context, kind string, limit int) ([]Media, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+mediaColumns+` FROM media
		WHERE kind = ? AND origin NOT IN (?, ?, ?, ?) ORDER BY created_at DESC LIMIT ?`,
		kind, originResult, originStyle, originSwatch, originPart, limit)
	if err != nil {
		return nil, err
	}
	return collect(rows, scanMedia)
}

// listOrigin is one origin's media of every kind, newest first.
func (s *store) listOrigin(ctx context.Context, origin string, limit int) ([]Media, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+mediaColumns+` FROM media
		WHERE origin = ? ORDER BY created_at DESC LIMIT ?`, origin, limit)
	if err != nil {
		return nil, err
	}
	return collect(rows, scanMedia)
}

// mediaByIDs returns the media for ids in the order given.
func (s *store) mediaByIDs(ctx context.Context, ids []string) ([]Media, error) {
	out := make([]Media, 0, len(ids))
	for _, id := range ids {
		m, err := s.media(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("media %s: %w", id, err)
		}
		out = append(out, m)
	}
	return out, nil
}

// deleteMedia removes a library item no run uses and returns it, so the
// caller can remove its file.
func (s *store) deleteMedia(ctx context.Context, id string) (Media, error) {
	m, err := s.media(ctx, id)
	if err != nil {
		return Media{}, err
	}
	var uses int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM runs
		WHERE source_id = ? OR result_id = ? OR style_image = ? OR soundtrack = ?
		OR EXISTS (SELECT 1 FROM json_each(runs.refs) WHERE value = ?)
		OR EXISTS (SELECT 1 FROM json_each(runs.parts) WHERE json_extract(value, '$.end') = ?)`,
		id, id, id, id, id, id).Scan(&uses); err != nil {
		return Media{}, err
	}
	if uses > 0 {
		return Media{}, errInUse
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM media WHERE id = ?`, id); err != nil {
		return Media{}, err
	}
	return m, nil
}

const runColumns = `id, batch, mode, engine, prompt, resolution, COALESCE(source_id, ''), refs, status, detail,
	progress, request_id, idempotency_key, COALESCE(result_id, ''), credits, error, created_at, finished_at,
	style, style_image, cost_usd, parts, width, height, quality, cast_view, ref_size, seed, soundtrack, soundtrack_mode`

func scanRun(row interface{ Scan(...any) error }) (Run, error) {
	var r Run
	var refs, created, finished, parts string
	if err := row.Scan(&r.ID, &r.Batch, &r.Mode, &r.Engine, &r.Prompt, &r.Resolution, &r.SourceID,
		&refs, &r.Status, &r.Detail, &r.Progress, &r.RequestID, &r.IdempotencyKey, &r.ResultID,
		&r.Credits, &r.Error, &created, &finished, &r.Style, &r.StyleImageID, &r.CostUSD, &parts,
		&r.Width, &r.Height, &r.Quality, &r.CastView, &r.RefSize, &r.Seed, &r.SoundtrackID, &r.SoundtrackMode); err != nil {
		return Run{}, err
	}
	if err := json.Unmarshal([]byte(refs), &r.RefIDs); err != nil {
		return Run{}, fmt.Errorf("run %s refs: %w", r.ID, err)
	}
	if err := json.Unmarshal([]byte(parts), &r.Parts); err != nil {
		return Run{}, fmt.Errorf("run %s parts: %w", r.ID, err)
	}
	var err error
	if r.CreatedAt, err = parseStamp(created); err != nil {
		return Run{}, err
	}
	if r.FinishedAt, err = parseStamp(finished); err != nil {
		return Run{}, err
	}
	return r, nil
}

func (s *store) addRun(ctx context.Context, r Run) error {
	refs, err := json.Marshal(r.RefIDs)
	if err != nil {
		return err
	}
	parts, err := partsJSON(r.Parts)
	if err != nil {
		return err
	}
	var source any
	if r.SourceID != "" {
		source = r.SourceID
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO runs (id, batch, mode, engine, prompt, resolution,
		source_id, refs, status, idempotency_key, created_at, style, style_image, parts, width, height, quality, cast_view, ref_size, seed,
		soundtrack, soundtrack_mode)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.Batch, r.Mode, r.Engine, r.Prompt, r.Resolution, source, string(refs),
		r.Status, r.IdempotencyKey, stamp(r.CreatedAt), r.Style, r.StyleImageID, parts, r.Width, r.Height, r.Quality, r.CastView, r.RefSize, r.Seed,
		r.SoundtrackID, r.SoundtrackMode)
	return err
}

// partsJSON is a run's parts as stored, never null.
func partsJSON(parts []part) (string, error) {
	if parts == nil {
		parts = []part{}
	}
	b, err := json.Marshal(parts)
	return string(b), err
}

// saveRun writes a run's progress fields, the style image once a Style run
// has made it, and its parts as they progress. Identity and the other inputs
// never change after addRun.
func (s *store) saveRun(ctx context.Context, r Run) error {
	var result any
	if r.ResultID != "" {
		result = r.ResultID
	}
	parts, err := partsJSON(r.Parts)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `UPDATE runs SET status = ?, detail = ?, progress = ?,
		request_id = ?, result_id = ?, credits = ?, error = ?, finished_at = ?, style_image = ?, cost_usd = ?, parts = ? WHERE id = ?`,
		r.Status, r.Detail, r.Progress, r.RequestID, result, r.Credits, r.Error, stamp(r.FinishedAt), r.StyleImageID, r.CostUSD,
		parts, r.ID)
	return err
}

func (s *store) run(ctx context.Context, id string) (Run, error) {
	r, err := scanRun(s.db.QueryRowContext(ctx, `SELECT `+runColumns+` FROM runs WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, errNotFound
	}
	return r, err
}

func (s *store) listRuns(ctx context.Context, limit int) ([]Run, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+runColumns+` FROM runs ORDER BY created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	return collect(rows, scanRun)
}

// activeRuns are the runs a restart must settle: those in flight, and lost
// ones, which it collects again.
func (s *store) activeRuns(ctx context.Context) ([]Run, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+runColumns+` FROM runs
		WHERE status IN (?, ?, ?, ?) ORDER BY created_at`, runPreparing, runQueued, runRunning, runLost)
	if err != nil {
		return nil, err
	}
	return collect(rows, scanRun)
}

// pendingMedia are references a stopped server left generating.
func (s *store) pendingMedia(ctx context.Context) ([]Media, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+mediaColumns+` FROM media WHERE status = ?`, mediaPending)
	if err != nil {
		return nil, err
	}
	return collect(rows, scanMedia)
}

// errRunActive is a delete of a run still in progress.
var errRunActive = errors.New("the run is still in progress")

// deleteRun removes a finished run with its result and returns both, so the
// deleteRun removes a finished run with the media only it owns, its result
// and its restyled frame, and returns those so the caller can remove their
// files. A visitor's own style image is theirs and stays.
func (s *store) deleteRun(ctx context.Context, id string) (Run, []Media, error) {
	r, err := s.run(ctx, id)
	if err != nil {
		return Run{}, nil, err
	}
	if runActive(r.Status) {
		return Run{}, nil, errRunActive
	}
	var owned []Media
	ids := []string{r.ResultID, r.StyleImageID}
	for _, p := range r.Parts {
		ids = append(ids, p.SourceID, p.ResultID, p.FrameID, p.TailID)
	}
	for _, mid := range ids {
		if mid == "" {
			continue
		}
		m, err := s.media(ctx, mid)
		if errors.Is(err, errNotFound) {
			continue
		}
		if err != nil {
			return Run{}, nil, err
		}
		if m.Origin == originResult || m.Origin == originStyle || m.Origin == originPart {
			owned = append(owned, m)
		}
	}
	err = s.db.TxContext(ctx, func(tx *database.Tx) error {
		if _, err := tx.Exec(`DELETE FROM runs WHERE id = ?`, id); err != nil {
			return err
		}
		for _, m := range owned {
			if _, err := tx.Exec(`DELETE FROM media WHERE id = ?`, m.ID); err != nil {
				return err
			}
		}
		return nil
	})
	return r, owned, err
}

func collect[T any](rows *sql.Rows, scan func(interface{ Scan(...any) error }) (T, error)) (_ []T, err error) {
	defer func() {
		if cerr := rows.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()
	var out []T
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// clip shortens s for a status line.
func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimSpace(string(r[:n])) + "…"
}

// swatches are the preset sample tiles by preset key: the newest one per
// preset, ready, pending, or failed.
func (s *store) swatches(ctx context.Context) (map[string]Media, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+mediaColumns+` FROM media WHERE origin = ? ORDER BY created_at`, originSwatch)
	if err != nil {
		return nil, err
	}
	list, err := collect(rows, scanMedia)
	if err != nil {
		return nil, err
	}
	out := make(map[string]Media, len(list))
	for _, m := range list {
		out[m.Label] = m
	}
	return out, nil
}

// clipCuts are the cuts a clip's editor saved, in seconds, and whether any
// were saved (an empty saved list means the clip has no cuts).
func (s *store) clipCuts(ctx context.Context, id string) ([]float64, bool, error) {
	var raw string
	if err := s.db.QueryRowContext(ctx, `SELECT cuts FROM media WHERE id = ?`, id).Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, errNotFound
		}
		return nil, false, err
	}
	if raw == "" {
		return nil, false, nil
	}
	var cuts []float64
	if err := json.Unmarshal([]byte(raw), &cuts); err != nil {
		return nil, false, fmt.Errorf("clip %s cuts: %w", id, err)
	}
	return cuts, true, nil
}

// setClipCuts saves a clip's cuts; nil forgets them, so they are detected.
func (s *store) setClipCuts(ctx context.Context, id string, cuts []float64) error {
	raw := ""
	if cuts != nil {
		b, err := json.Marshal(cuts)
		if err != nil {
			return err
		}
		raw = string(b)
	}
	_, err := s.db.ExecContext(ctx, `UPDATE media SET cuts = ? WHERE id = ?`, raw, id)
	return err
}

// studioValue is a remembered studio choice, empty when never set.
func (s *store) studioValue(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM studio WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// setStudioValue remembers a studio choice across reloads and restarts.
func (s *store) setStudioValue(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO studio (key, value) VALUES (?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// spentSince is the estimated spend of runs and made images created since t.
func (s *store) spentSince(ctx context.Context, t time.Time) (float64, error) {
	var sum float64
	err := s.db.QueryRowContext(ctx, `SELECT
		(SELECT COALESCE(SUM(cost_usd), 0) FROM runs WHERE created_at >= ?) +
		(SELECT COALESCE(SUM(cost_usd), 0) FROM media WHERE created_at >= ?)`, stamp(t), stamp(t)).Scan(&sum)
	return sum, err
}

// finishedBefore are finished runs created before t, oldest first, for the
// storage cleanup.
func (s *store) finishedBefore(ctx context.Context, t time.Time) ([]Run, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+runColumns+` FROM runs
		WHERE created_at < ? AND status NOT IN (?, ?, ?, ?) ORDER BY created_at`,
		stamp(t), runPreparing, runQueued, runRunning, runLost)
	if err != nil {
		return nil, err
	}
	return collect(rows, scanRun)
}

// sourceInUse reports whether any run still uses a media item as its source.
func (s *store) sourceInUse(ctx context.Context, id string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM runs WHERE source_id = ?`, id).Scan(&n)
	return n > 0, err
}
