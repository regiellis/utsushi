package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/regiellis/hyperchi/hyperchi/database"
)

// Story documents keep episode planning separate from generation history.
// References are media IDs; missing/deleted media is shown as an empty take.
type storyProject struct {
	ID, Title, Genre, Premise, Look string
	Episodes                        []storyEpisode
	Assets                          []storyAsset
}
type storyEpisode struct {
	ID, Title, Synopsis string
	Shots               []storyShot
}
type storyShot struct {
	ID, Scene, Title, Prompt, Dialogue, MediaID string
	Seconds                                     int
	Takes                                       []string
}
type storyAsset struct {
	ID, Kind, Name, Description, MediaID string
}

func (s *store) storyProjects(ctx context.Context) ([]storyProject, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT document FROM story_projects ORDER BY rowid DESC`)
	if err != nil {
		return nil, err
	}
	return collect(rows, func(row interface{ Scan(...any) error }) (storyProject, error) {
		var raw string
		var p storyProject
		if err := row.Scan(&raw); err != nil {
			return p, err
		}
		err := json.Unmarshal([]byte(raw), &p)
		return p, err
	})
}
func (s *store) storyProject(ctx context.Context, id string) (storyProject, error) {
	var raw string
	var p storyProject
	err := s.db.QueryRowContext(ctx, `SELECT document FROM story_projects WHERE id=?`, id).Scan(&raw)
	if err != nil {
		return p, err
	}
	err = json.Unmarshal([]byte(raw), &p)
	return p, err
}
func (s *store) addStory(ctx context.Context, p storyProject) error {
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO story_projects(id,document) VALUES(?,?)`, p.ID, string(raw))
	return err
}
func (s *store) editStory(ctx context.Context, id string, edit func(*storyProject) error) error {
	return s.db.TxContext(ctx, func(tx *database.Tx) error {
		// Obtain the write lock before reading, so concurrent forms cannot overwrite one another.
		if _, err := tx.Exec(`UPDATE story_projects SET document=document WHERE id=?`, id); err != nil {
			return err
		}
		var raw string
		if err := tx.QueryRow(`SELECT document FROM story_projects WHERE id=?`, id).Scan(&raw); err != nil {
			return err
		}
		var p storyProject
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			return err
		}
		if err := edit(&p); err != nil {
			return err
		}
		data, err := json.Marshal(p)
		if err != nil {
			return err
		}
		_, err = tx.Exec(`UPDATE story_projects SET document=? WHERE id=?`, string(data), id)
		return err
	})
}
func storyEpisodeAt(p *storyProject, id string) (*storyEpisode, error) {
	for i := range p.Episodes {
		if p.Episodes[i].ID == id {
			return &p.Episodes[i], nil
		}
	}
	return nil, errNotFound
}
func storyShotAt(e *storyEpisode, id string) (*storyShot, error) {
	for i := range e.Shots {
		if e.Shots[i].ID == id {
			return &e.Shots[i], nil
		}
	}
	return nil, errNotFound
}
func (s *store) storyContext(ctx context.Context, project, episode, shot string) (storyProject, storyEpisode, storyShot, error) {
	p, err := s.storyProject(ctx, project)
	if err != nil {
		return p, storyEpisode{}, storyShot{}, err
	}
	e, err := storyEpisodeAt(&p, episode)
	if err != nil {
		return p, storyEpisode{}, storyShot{}, err
	}
	sh, err := storyShotAt(e, shot)
	if err != nil {
		return p, *e, storyShot{}, err
	}
	return p, *e, *sh, nil
}
func storyRequired(value, name string, max int) error {
	if value == "" || len([]rune(value)) > max {
		return fmt.Errorf("%s must contain 1–%d characters", name, max)
	}
	return nil
}

var errStoryInput = errors.New("invalid story input")
