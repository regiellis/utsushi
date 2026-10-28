package main

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/regiellis/hyperchi/hyperchi"
)

type episodeCard struct {
	storyEpisode
	Number, Duration, Ready int
	Cover                   string
}
type shotCard struct {
	storyShot
	Number, Start int
	Media         mediaView
	Selected      bool
}
type assetCard struct {
	storyAsset
	Media mediaView
}

func (w *web) storyRoutes() {
	w.app.Get("/", w.storyHome)
	w.app.Post("/projects", w.storyCreate)
	w.app.Get("/projects/{project}", w.storyHome)
	w.app.Post("/projects/{project}/edit", w.storyEdit)
	w.app.Post("/projects/{project}/media", w.storyUpload)
	w.app.Get("/projects/{project}/episodes/{episode}", w.storyWorkspace)
}
func storyURL(id string) string { return "/projects/" + url.PathEscape(id) }
func episodeURL(project, episode string) string {
	return storyURL(project) + "/episodes/" + url.PathEscape(episode)
}
func (w *web) storyHome(c *hyperchi.Context) error {
	ctx := c.Ctx()
	projects, err := w.st.storyProjects(ctx)
	if err != nil {
		return err
	}
	var p storyProject
	if id := c.Param("project"); id != "" {
		p, err = w.st.storyProject(ctx, id)
		if errors.Is(err, sql.ErrNoRows) {
			return hyperchi.NotFound("Project not found.")
		}
		if err != nil {
			return err
		}
	} else if len(projects) > 0 {
		p = projects[0]
	}
	var episodes []episodeCard
	var assets []assetCard
	for i, e := range p.Episodes {
		card := episodeCard{storyEpisode: e, Number: i + 1}
		for _, sh := range e.Shots {
			card.Duration += sh.Seconds
			if m, err := w.st.media(ctx, sh.MediaID); err == nil && m.Status == mediaReady {
				card.Ready++
				if card.Cover == "" && (m.Kind == kindImage || m.Kind == kindVideo) {
					card.Cover = viewMedia(m).URL
					if m.Kind == kindVideo {
						card.Cover = "/media/" + m.ID + "/poster"
					}
				}
			}
		}
		episodes = append(episodes, card)
	}
	for _, a := range p.Assets {
		card := assetCard{storyAsset: a}
		if m, err := w.st.media(ctx, a.MediaID); err == nil {
			card.Media = viewMedia(m)
		}
		assets = append(assets, card)
	}
	media, err := w.storyMedia(c)
	if err != nil {
		return err
	}
	return c.View("story-home", w.page(ctx, hyperchi.H{"title": "Your stories · Utsushi", "nav": "stories", "projects": projects, "project": p, "episodes": episodes, "assets": assets, "media": media, "tab": c.Query("tab")}))
}
func (w *web) storyMedia(c *hyperchi.Context) ([]mediaView, error) {
	rows, err := w.st.db.QueryContext(c.Ctx(), `SELECT `+mediaColumns+` FROM media WHERE status='ready' AND kind IN ('image','video') ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	media, err := collect(rows, scanMedia)
	if err != nil {
		return nil, err
	}
	return w.mediaViews(media, false), nil
}
func (w *web) storyCreate(c *hyperchi.Context) error {
	if err := c.Request().ParseForm(); err != nil {
		return err
	}
	title := strings.TrimSpace(c.Request().PostForm.Get("title"))
	if err := storyRequired(title, "Project title", 120); err != nil {
		return hyperchi.BadRequest(err.Error())
	}
	p := storyProject{ID: newID(), Title: title, Genre: strings.TrimSpace(c.Request().PostForm.Get("genre"))}
	if err := w.st.addStory(c.Ctx(), p); err != nil {
		return err
	}
	return c.Redirect(storyURL(p.ID))
}
func (w *web) storyEdit(c *hyperchi.Context) error {
	req := c.Request()
	req.Body = http.MaxBytesReader(c.Response(), req.Body, 1<<20)
	if err := req.ParseForm(); err != nil {
		return hyperchi.BadRequest("The form is too large or invalid.")
	}
	f := req.PostForm
	get := func(k string) string { return strings.TrimSpace(f.Get(k)) }
	id := c.Param("project")
	dest := storyURL(id)
	if get("episode") != "" {
		dest = episodeURL(id, get("episode"))
	}
	err := w.st.editStory(c.Ctx(), id, func(p *storyProject) error {
		switch get("action") {
		case "project":
			if err := storyRequired(get("title"), "Project title", 120); err != nil {
				return fmt.Errorf("%w: %v", errStoryInput, err)
			}
			p.Title, p.Genre, p.Premise, p.Look = get("title"), get("genre"), get("premise"), get("look")
		case "episode":
			if err := storyRequired(get("title"), "Episode title", 120); err != nil {
				return fmt.Errorf("%w: %v", errStoryInput, err)
			}
			e := storyEpisode{ID: newID(), Title: get("title"), Synopsis: get("synopsis")}
			p.Episodes = append(p.Episodes, e)
			dest = episodeURL(id, e.ID)
		case "asset":
			if !slices.Contains([]string{"character", "prop", "location"}, get("kind")) {
				return errStoryInput
			}
			if err := storyRequired(get("name"), "Asset name", 80); err != nil {
				return fmt.Errorf("%w: %v", errStoryInput, err)
			}
			p.Assets = append(p.Assets, storyAsset{ID: newID(), Kind: get("kind"), Name: get("name"), Description: get("description"), MediaID: get("media")})
			dest = storyURL(id) + "?tab=world"
		case "asset-edit":
			for i := range p.Assets {
				if p.Assets[i].ID == get("asset") {
					if err := storyRequired(get("name"), "Asset name", 80); err != nil {
						return fmt.Errorf("%w: %v", errStoryInput, err)
					}
					p.Assets[i].Name, p.Assets[i].Description, p.Assets[i].MediaID = get("name"), get("description"), get("media")
					dest = storyURL(id) + "?tab=world"
					return nil
				}
			}
			return errNotFound
		case "episode-edit", "shot", "shot-edit", "shot-move", "shot-delete":
			e, err := storyEpisodeAt(p, get("episode"))
			if err != nil {
				return err
			}
			if get("action") == "episode-edit" {
				if err := storyRequired(get("title"), "Episode title", 120); err != nil {
					return fmt.Errorf("%w: %v", errStoryInput, err)
				}
				e.Title, e.Synopsis = get("title"), get("synopsis")
				return nil
			}
			if get("action") == "shot" {
				sh := storyShot{ID: newID(), Scene: get("scene"), Title: fmt.Sprintf("Shot %02d", len(e.Shots)+1), Seconds: 5}
				if sh.Scene == "" {
					sh.Scene = "Scene 1"
				}
				e.Shots = append(e.Shots, sh)
				dest += "?shot=" + sh.ID
				return nil
			}
			sh, err := storyShotAt(e, get("shot"))
			if err != nil {
				return err
			}
			switch get("action") {
			case "shot-edit":
				sec, err := strconv.Atoi(get("seconds"))
				if err != nil || sec < 1 || sec > 120 {
					return fmt.Errorf("%w: shot duration must be 1–120 seconds", errStoryInput)
				}
				if err := storyRequired(get("title"), "Shot title", 120); err != nil {
					return fmt.Errorf("%w: %v", errStoryInput, err)
				}
				sh.Title, sh.Scene, sh.Prompt, sh.Dialogue, sh.MediaID, sh.Seconds = get("title"), get("scene"), get("prompt"), get("dialogue"), get("media"), sec
				dest += "?shot=" + sh.ID
			case "shot-move":
				i := slices.IndexFunc(e.Shots, func(s storyShot) bool { return s.ID == sh.ID })
				step := 1
				if get("direction") == "up" {
					step = -1
				}
				j := i + step
				if j >= 0 && j < len(e.Shots) {
					e.Shots[i], e.Shots[j] = e.Shots[j], e.Shots[i]
				}
				dest += "?shot=" + get("shot")
			case "shot-delete":
				e.Shots = slices.DeleteFunc(e.Shots, func(s storyShot) bool { return s.ID == get("shot") })
			}
		default:
			return errStoryInput
		}
		return nil
	})
	if errors.Is(err, sql.ErrNoRows) || errors.Is(err, errNotFound) {
		return hyperchi.NotFound("Project, episode, or shot not found.")
	}
	if errors.Is(err, errStoryInput) {
		return hyperchi.BadRequest(err.Error())
	}
	if err != nil {
		return err
	}
	return c.Redirect(dest)
}
func (w *web) storyWorkspace(c *hyperchi.Context) error {
	ctx := c.Ctx()
	p, err := w.st.storyProject(ctx, c.Param("project"))
	if errors.Is(err, sql.ErrNoRows) {
		return hyperchi.NotFound("Project not found.")
	}
	if err != nil {
		return err
	}
	e, err := storyEpisodeAt(&p, c.Param("episode"))
	if err != nil {
		return hyperchi.NotFound("Episode not found.")
	}
	var shots []shotCard
	var selected shotCard
	var scenes []string
	duration := 0
	for i, sh := range e.Shots {
		card := shotCard{storyShot: sh, Number: i + 1, Start: duration, Selected: sh.ID == c.Query("shot") || (c.Query("shot") == "" && i == 0)}
		if m, err := w.st.media(ctx, sh.MediaID); err == nil {
			card.Media = viewMedia(m)
		}
		duration += sh.Seconds
		if !slices.Contains(scenes, sh.Scene) {
			scenes = append(scenes, sh.Scene)
		}
		if card.Selected {
			selected = card
		}
		shots = append(shots, card)
	}
	if c.Query("shot") != "" && selected.ID == "" {
		return hyperchi.NotFound("Shot not found.")
	}
	media, err := w.storyMedia(c)
	if err != nil {
		return err
	}
	var takes []runView
	for _, id := range selected.Takes {
		if r, err := w.st.run(ctx, id); err == nil {
			takes = append(takes, w.viewRun(ctx, r))
		}
	}
	return c.View("story-workspace", w.page(ctx, hyperchi.H{"title": e.Title + " · Utsushi", "nav": "stories", "project": p, "episode": e, "shots": shots, "selected": selected, "scenes": scenes, "duration": duration, "media": media, "takes": takes}))
}

// storyUpload reuses the library's validation and duplicate detection.
func (w *web) storyUpload(c *hyperchi.Context) (err error) {
	p, err := w.st.storyProject(c.Ctx(), c.Param("project"))
	if errors.Is(err, sql.ErrNoRows) {
		return hyperchi.NotFound("Project not found.")
	}
	if err != nil {
		return err
	}
	req := c.Request()
	req.Body = http.MaxBytesReader(c.Response(), req.Body, maxUploadBytes)
	if err := req.ParseMultipartForm(8 << 20); err != nil {
		return hyperchi.BadRequest("Choose an image or MP4 under 200 MB.")
	}
	defer func() { err = errors.Join(err, req.MultipartForm.RemoveAll()) }()
	dest := storyURL(p.ID) + "?tab=world"
	if episode := req.PostForm.Get("episode"); episode != "" {
		e, err := storyEpisodeAt(&p, episode)
		if err != nil {
			return hyperchi.NotFound("Episode not found.")
		}
		if shot := req.PostForm.Get("shot"); shot != "" {
			if _, err := storyShotAt(e, shot); err != nil {
				return hyperchi.NotFound("Shot not found.")
			}
		}
		dest = episodeURL(p.ID, episode) + "?shot=" + url.QueryEscape(req.PostForm.Get("shot"))
	}
	file, head, err := req.FormFile("file")
	if err != nil {
		return hyperchi.BadRequest("Choose a file to upload.")
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	fresh, err := w.lib.store(file, originUpload, head.Filename, 0)
	if err != nil {
		return hyperchi.BadRequest(describe(err))
	}
	if fresh.Kind != kindImage && fresh.Kind != kindVideo {
		return errors.Join(hyperchi.BadRequest("Choose an image or MP4 video."), w.lib.remove(fresh))
	}
	if _, err = w.keepUpload(c.Ctx(), fresh); err != nil {
		return err
	}
	return c.Redirect(dest)
}

// storyEmbedded allows the studio's own tool drawer, while keeping third-party
// framing blocked. Other CSP directives remain intact.
func storyEmbedded(c *hyperchi.Context) bool {
	if c.Query("embedded") != "1" {
		return false
	}
	h := c.Response().Header()
	h.Set("X-Frame-Options", "SAMEORIGIN")
	directives := strings.Split(h.Get("Content-Security-Policy"), ";")
	out := make([]string, 0, len(directives)+1)
	for _, d := range directives {
		d = strings.TrimSpace(d)
		if d != "" && !strings.HasPrefix(d, "frame-ancestors ") {
			out = append(out, d)
		}
	}
	out = append(out, "frame-ancestors 'self'")
	h.Set("Content-Security-Policy", strings.Join(out, "; "))
	return true
}
