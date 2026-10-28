package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"mime"
	"net/http"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/regiellis/hyperchi/hyperchi"
	"github.com/regiellis/hyperchi/hyperchi/extensions/sse"
)

// maxUploadBytes bounds one upload request: the largest accepted video plus
// room for the multipart framing.
const maxUploadBytes = 200<<20 + 1<<20

// web holds the handlers and publishes live updates over SSE.
type web struct {
	storageMu sync.RWMutex // gate mutations during a storage purge
	uploadMu  sync.Mutex   // serialize upload reuse and registration
	app       *hyperchi.HyperChi
	st        *store
	lib       *library
	eng       *engines
	run       *runner
	sse       *sse.SSEHub
	gpu       *gpuNames
	// cloud holds the Comfy API key Settings can change; envKey is the
	// COMFY_API_KEY fallback the app started with.
	cloud  *cloudLink
	envKey string
	prefs  *prefsStore
	proxy  *proxyManager // nil when UTSUSHI_PROXY_URL names a proxy run elsewhere
	frames *frames
	crash  *crashes
}

// The story prototype is paused while the product direction is reconsidered.
const storyStudioEnabled = false

// modePaths are the studio pages, one per mode.
var modePaths = map[string]string{modeMotion: "/", modeSwap: "/swap", modeStyle: "/style"}

func (w *web) routes() {
	app := w.app
	app.UseGlobal(w.storageAccess)
	if storyStudioEnabled {
		w.storyRoutes()
	}
	for mode, path := range modePaths {
		if storyStudioEnabled && mode == modeMotion {
			path = "/motion"
		}
		app.Get(path, w.studio(mode))
	}
	app.Get("/long", w.longPage)
	app.Post("/long", w.createLong)
	app.Get("/styles/{key}/tile", w.styleTile)
	app.Get("/engine", w.engineBlock)
	app.Get("/clip", w.clipBlock)
	app.Post("/runs/{id}/source", w.useAsSource)
	app.Get("/runs", w.gallery)
	app.Get("/runs/{id}", w.runPage)
	app.Get("/runs/recent", w.recentRuns)
	app.Get("/runs/{id}/card", w.runCard)
	app.Get("/media/{id}/tile", w.mediaTile)
	app.Get("/status", w.status)
	app.Post("/runs", w.createRuns)
	app.Post("/runs/{id}/cancel", w.cancelRun)
	app.Post("/runs/{id}/collect", w.collectRun)
	app.Post("/runs/{id}/again", w.runAgain)
	app.Delete("/runs/{id}", w.deleteRun)
	app.Post("/media", w.upload)
	app.Get("/media/{id}", w.serveMedia)
	app.Delete("/media/{id}", w.deleteMedia)
	app.Get("/media/{id}/editor", w.clipEditor)
	app.Post("/media/{id}/cuts", w.saveClipCuts)
	app.Post("/media/{id}/split", w.splitClipHandler)
	app.Post("/media/{id}/trim", w.trimClipHandler)
	app.Post("/studio/refs", w.rememberRefsHandler)
	w.settingsRoutes()
	w.previewRoutes()
	w.llmRoutes()
	w.imageRoutes()
	w.toolsRoutes()
}

// studio is the create page for one mode.
func (w *web) studio(mode string) hyperchi.Handler {
	return func(c *hyperchi.Context) error {
		ctx := c.Ctx()
		block := w.engineData(ctx, mode, c.Query("engine"))
		clip, err := w.pageClip(ctx, c.Query("clip"))
		if err != nil {
			return err
		}
		prefill, err := w.reusedPrompt(ctx, mode, c.Query("reuse"))
		if err != nil {
			return err
		}
		if c.Query("shot") != "" {
			project, _, shot, err := w.st.storyContext(ctx, c.Query("project"), c.Query("episode"), c.Query("shot"))
			if err != nil {
				return hyperchi.NotFound("The shot no longer exists.")
			}
			prefill = shot.Prompt
			if shot.MediaID != "" {
				if m, err := w.st.media(ctx, shot.MediaID); err == nil && m.Kind == kindVideo {
					clip, err = w.pageClip(ctx, m.ID)
					if err != nil {
						return err
					}
				}
			}
			if project.Look != "" {
				prefill += "\nVisual direction: " + project.Look
			}
		}
		picked, err := w.rememberedRefs(ctx, mode)
		if err != nil {
			return err
		}
		images, err := w.st.listMedia(ctx, kindImage, -1)
		if err != nil {
			return err
		}
		refVideos, err := w.st.listMedia(ctx, kindVideo, -1)
		if err != nil {
			return err
		}
		audios, err := w.st.listMedia(ctx, kindAudio, -1)
		if err != nil {
			return err
		}
		runs, err := w.st.listRuns(ctx, 12)
		if err != nil {
			return err
		}
		var styles []styleView
		if mode == modeStyle {
			if err := w.run.ensureSwatches(ctx); err != nil {
				log.Printf("utsushi: start swatches: %v", err)
			}
			if styles, err = w.styleViews(ctx); err != nil {
				return err
			}
		}
		return c.View("studio", w.page(ctx, hyperchi.H{
			"embedded": storyEmbedded(c), "storyProject": c.Query("project"), "storyEpisode": c.Query("episode"), "storyShot": c.Query("shot"),
			"styles":         styles,
			"title":          modeLabels[mode] + " · Utsushi",
			"nav":            mode,
			"mode":           mode,
			"modeLabel":      modeLabels[mode],
			"prefill":        prefill,
			"pin":            pinOf(prefill, c.Query("reuse")),
			"engineBlock":    block,
			"clip":           clip,
			"picked":         picked,
			"images":         w.mediaViews(images, false),
			"refVideos":      w.mediaViews(refVideos, false),
			"audios":         w.mediaViews(audios, false),
			"runs":           w.runViews(ctx, runs),
			"versionChoices": []int{1, 2, 3, 4},
		}))
	}
}

// pageFlags are the Settings the page script reads.
type pageFlags struct {
	Notify, Posters            bool
	Improve, Describe, Suggest bool
	Spend                      string // JSON for the Generate guard
}

// page adds what the layout itself reads to a page's data.
func (w *web) page(ctx context.Context, data hyperchi.H) hyperchi.H {
	data["runtime"] = w.runtime(ctx)
	p := w.prefs.get()
	flags := pageFlags{Notify: p.Engines.Notify, Posters: w.frames.available()}
	if p.LLM.On {
		flags.Improve, flags.Describe, flags.Suggest = p.LLM.Improve, p.LLM.Describe, p.LLM.Suggest
	}
	spent, err := w.monthSpend(ctx)
	if err != nil {
		log.Printf("utsushi: month spend: %v", err)
	}
	confirm := -1.0
	if v, err := strconv.ParseFloat(p.Spend.ConfirmOver, 64); err == nil {
		confirm = v
	}
	spend, err := json.Marshal(map[string]any{
		"confirmOver": confirm, "capOn": p.Spend.CapOn, "cap": p.Spend.Cap, "spent": spent,
		"capAction": p.Spend.CapAction, "estimates": p.Spend.Estimates,
	})
	if err == nil {
		flags.Spend = string(spend)
	}
	data["page"] = flags
	return data
}

// engineData is the engine picker for a mode: every engine, and the chosen
// one (the first when chosen names none of them).
func (w *web) engineData(ctx context.Context, mode, chosen string) hyperchi.H {
	p := w.prefs.get()
	list := w.pickerEngines(ctx, mode, p)
	slices.SortStableFunc(list, func(a, b engine) int {
		if (featuredModel(a.info().ID) != "") == (featuredModel(b.info().ID) != "") {
			return 0
		}
		if featuredModel(a.info().ID) != "" {
			return -1
		}
		return 1
	})
	has := func(id string) bool {
		return slices.ContainsFunc(list, func(en engine) bool { return en.info().ID == id })
	}
	chosen = w.eng.preferredID(chosen, openImages(p))
	if !has(chosen) {
		chosen = w.eng.preferredID(p.Engines.Default[mode], openImages(p))
	}
	if !has(chosen) && len(list) > 0 {
		chosen = list[0].info().ID
	}
	opts := viewOpts{Hardware: w.gpu.hardwareSnapshot(ctx), Resolution: p.Engines.Resolution, Estimates: p.Spend.Estimates, LongClips: w.longClips()}
	var views []engineView
	var current engineView
	for _, en := range list {
		v := viewEngine(ctx, en, en.info().ID == chosen, opts)
		if v.Selected {
			current = v
		}
		views = append(views, v)
	}
	return hyperchi.H{"mode": mode, "modeLabel": modeLabels[mode], "engines": views, "engine": current, "hardware": opts.Hardware.summary()}
}

// engineBlock re-renders the picker when an engine is chosen.
func (w *web) engineBlock(c *hyperchi.Context) error {
	mode := c.Query("mode")
	if _, ok := modeLabels[mode]; !ok {
		return hyperchi.BadRequest("Unknown mode.")
	}
	return c.Fragment("engine-block", w.engineData(c.Ctx(), mode, c.Query("engine")))
}

// clipData is the source clip slot: the chosen clip (or, with fallback, the
// newest one) and the other clips for the paged browser.
func (w *web) clipData(ctx context.Context, id string, fallback bool) (hyperchi.H, error) {
	videos, err := w.st.listMedia(ctx, kindVideo, -1)
	if err != nil {
		return nil, err
	}
	var chosen *mediaView
	var others []mediaView
	for _, m := range videos {
		v := viewMedia(m)
		if !v.Ready {
			continue
		}
		if chosen == nil && (m.ID == id || (id == "" && fallback)) {
			v.Selected = true
			chosen = &v
			continue
		}
		others = append(others, v)
	}
	if chosen == nil && id != "" {
		if m, err := w.st.media(ctx, id); err == nil && m.Kind == kindVideo && m.Status == mediaReady {
			v := viewMedia(m)
			v.Selected = true
			chosen = &v
		}
	}
	return hyperchi.H{"clip": chosen, "clips": others}, nil
}

// pinOf is the take whose seed a reused prompt carries, when there was one.
func pinOf(prefill, id string) string {
	if prefill == "" {
		return ""
	}
	return id
}

// reusedPrompt is the prompt of the run a card's Reuse prompt names, for
// the studio of that run's mode to open with. It starts nothing.
func (w *web) reusedPrompt(ctx context.Context, mode, id string) (string, error) {
	if id == "" {
		return "", nil
	}
	r, err := w.st.run(ctx, id)
	if errors.Is(err, errNotFound) {
		return "", nil
	}
	if err != nil || r.Mode != mode {
		return "", err
	}
	return r.Prompt, nil
}

// clipNone is the remembered clip after Remove, so a reload stays empty.
const clipNone = "none"

// rememberClip records the clip the studio shows, or none for an empty id,
// so a reload or another mode opens with the same one.
func (w *web) rememberClip(ctx context.Context, id string) error {
	if id == "" {
		id = clipNone
	}
	return w.st.setStudioValue(ctx, "clip", id)
}

// rememberedRefs is the reference pick a mode's page opens with, as a JSON
// list of ids in pick order: the last one made there, less anything since
// deleted or not ready. The clip is remembered the same way (pageClip).
func (w *web) rememberedRefs(ctx context.Context, mode string) (string, error) {
	raw, err := w.st.studioValue(ctx, "refs:"+mode)
	if err != nil || raw == "" {
		return "[]", err
	}
	var ids []string
	if err := json.Unmarshal([]byte(raw), &ids); err != nil {
		return "[]", nil
	}
	kept := make([]string, 0, len(ids))
	for _, id := range ids {
		m, err := w.st.media(ctx, id)
		switch {
		case errors.Is(err, errNotFound):
			continue
		case err != nil:
			return "[]", err
		}
		if m.Status == mediaReady {
			kept = append(kept, id)
		}
	}
	out, err := json.Marshal(kept)
	return string(out), err
}

// rememberRefsHandler keeps a page's reference pick, in pick order, as the
// studio script sends it whenever the pick changes.
func (w *web) rememberRefsHandler(c *hyperchi.Context) error {
	if err := c.Request().ParseForm(); err != nil {
		return err
	}
	mode := c.Request().Form.Get("mode")
	if _, ok := modeLabels[mode]; !ok {
		return hyperchi.BadRequest("No such mode.")
	}
	refs := c.Request().Form["refs"]
	if len(refs) > 64 {
		refs = refs[:64]
	}
	raw, err := json.Marshal(append([]string{}, refs...))
	if err != nil {
		return err
	}
	if err := w.st.setStudioValue(c.Ctx(), "refs:"+mode, string(raw)); err != nil {
		return err
	}
	return c.NoContent()
}

// pageClip is the clip a studio page opens with: the one the URL asks for,
// else the one last chosen, else the newest. A removed clip stays removed,
// and a remembered clip since deleted falls back to the newest.
func (w *web) pageClip(ctx context.Context, asked string) (hyperchi.H, error) {
	if asked != "" {
		if err := w.rememberClip(ctx, asked); err != nil {
			return nil, err
		}
		return w.clipData(ctx, asked, false)
	}
	saved, err := w.st.studioValue(ctx, "clip")
	if err != nil {
		return nil, err
	}
	if saved == clipNone {
		return w.clipData(ctx, "", false)
	}
	data, err := w.clipData(ctx, saved, saved == "")
	if err != nil || saved == "" {
		return data, err
	}
	if c, ok := data["clip"].(*mediaView); ok && c != nil {
		return data, nil
	}
	return w.clipData(ctx, "", true)
}

// clipBlock switches the source clip, or clears it with no id.
func (w *web) clipBlock(c *hyperchi.Context) error {
	id := c.Query("id")
	if err := w.rememberClip(c.Ctx(), id); err != nil {
		return err
	}
	data, err := w.clipData(c.Ctx(), id, false)
	if err != nil {
		return err
	}
	return c.Fragment("clip-block", data)
}

// useAsSource copies a result into the clip library and opens the studio
// with it chosen. It is a copy, so deleting the run later leaves the clip.
func (w *web) useAsSource(c *hyperchi.Context) error {
	ctx := c.Ctx()
	r, err := w.st.run(ctx, c.Param("id"))
	if errors.Is(err, errNotFound) {
		return hyperchi.NotFound("No such run.")
	}
	if err != nil {
		return err
	}
	if r.ResultID == "" {
		return c.HX().Trigger("toast", hyperchi.H{"message": "This run has no result yet."}).NoContent()
	}
	res, err := w.st.media(ctx, r.ResultID)
	if err != nil {
		return err
	}
	f, err := w.lib.open(res)
	if err != nil {
		return err
	}
	m, err := w.lib.store(f, originUpload, modeLabels[r.Mode]+" result", 0)
	if cerr := f.Close(); cerr != nil && err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	m, err = w.keepUpload(ctx, m)
	if err != nil {
		return err
	}
	return c.Redirect(studioPath(r.Mode) + "?clip=" + m.ID)
}

// status re-checks engine and proxy readiness for the studio's poll and
// answers with out-of-band swaps of just the status notes, so selections
// and focus are left alone.
func (w *web) status(c *hyperchi.Context) error {
	ctx := c.Ctx()
	var views []engineView
	for _, en := range w.eng.forMode(c.Query("mode")) {
		views = append(views, viewEngine(ctx, en, false, viewOpts{}))
	}
	return c.Fragment("studio-status", hyperchi.H{"engines": views, "runtime": w.runtime(ctx)})
}

// runCard is one run's live card, for feeds adding a run started elsewhere.
func (w *web) runCard(c *hyperchi.Context) error {
	r, err := w.st.run(c.Ctx(), c.Param("id"))
	if errors.Is(err, errNotFound) {
		return hyperchi.NotFound("No such run.")
	}
	if err != nil {
		return err
	}
	return c.Fragment("run-card", w.viewRun(c.Ctx(), r))
}

// recentRuns lists the newest run ids, newest first, for a feed catching up
// after its stream was paused.
func (w *web) recentRuns(c *hyperchi.Context) error {
	runs, err := w.st.listRuns(c.Ctx(), 12)
	if err != nil {
		return err
	}
	ids := make([]string, 0, len(runs))
	for _, r := range runs {
		ids = append(ids, r.ID)
	}
	return c.JSON(ids)
}

// mediaTile is one image tile, for a pending tile catching up.
func (w *web) mediaTile(c *hyperchi.Context) error {
	m, err := w.st.media(c.Ctx(), c.Param("id"))
	if errors.Is(err, errNotFound) {
		return hyperchi.NotFound("No such image.")
	}
	if err != nil {
		return err
	}
	return c.Fragment("image-tile", viewMedia(m))
}

// mediaViews converts a library list; the newest video starts selected.
func (w *web) mediaViews(list []Media, selectFirst bool) []mediaView {
	out := make([]mediaView, 0, len(list))
	for i, m := range list {
		v := viewMedia(m)
		v.Selected = selectFirst && i == 0 && v.Ready
		out = append(out, v)
	}
	return out
}

func (w *web) runViews(ctx context.Context, runs []Run) []runView {
	out := make([]runView, 0, len(runs))
	for _, r := range runs {
		out = append(out, w.viewRun(ctx, r))
	}
	return out
}

func (w *web) gallery(c *hyperchi.Context) error {
	runs, err := w.st.listRuns(c.Ctx(), 120)
	if err != nil {
		return err
	}
	return c.View("gallery", w.page(c.Ctx(), hyperchi.H{
		"title": "Library · Utsushi",
		"nav":   "runs",
		"runs":  w.runViews(c.Ctx(), runs),
	}))
}

func (w *web) runPage(c *hyperchi.Context) error {
	r, err := w.st.run(c.Ctx(), c.Param("id"))
	if errors.Is(err, errNotFound) {
		return hyperchi.NotFound("No such run.")
	}
	if err != nil {
		return err
	}
	v := w.viewRun(c.Ctx(), r)
	return c.View("run", w.page(c.Ctx(), hyperchi.H{
		"title":    v.ModeLabel + " · Utsushi",
		"nav":      v.Mode,
		"run":      v,
		"siblings": w.viewSiblings(c.Ctx(), r),
	}))
}

type runForm struct {
	StoryProject string   `form:"story_project"`
	StoryEpisode string   `form:"story_episode"`
	StoryShot    string   `form:"story_shot"`
	Mode         string   `form:"mode" validate:"oneof=motion swap style"`
	Engine       string   `form:"engine" validate:"required"`
	Source       string   `form:"source"`
	Refs         []string `form:"refs"`
	Prompt       string   `form:"prompt"`
	Resolution   string   `form:"resolution"`
	Versions     int      `form:"versions" validate:"min=1,max=4"`
	// Style is a preset key, or "image:<media id>" for the visitor's own
	// style image. Style mode only.
	Style string `form:"style"`
	// Pin is the take Reuse prompt came from; its seed carries over.
	Pin string `form:"pin"`
}

// runStyle is a Style run's choice: a preset, or the visitor's own image.
type runStyle struct {
	Preset, ImageID string
}

// createRuns checks the form against the engine, then starts one run per
// version. Each version is its own billed generation.
func (w *web) createRuns(c *hyperchi.Context) error {
	in, err := hyperchi.Bind[runForm](c)
	if fields := hyperchi.FieldErrors(err); fields != nil {
		return w.formErrors(c, fields)
	}
	if err != nil {
		return err
	}
	ctx := c.Ctx()
	if in.StoryShot != "" {
		if _, _, _, err := w.st.storyContext(ctx, in.StoryProject, in.StoryEpisode, in.StoryShot); err != nil {
			return w.formErrors(c, map[string][]string{"shot": {"The shot no longer exists. Reopen it from the episode."}})
		}
	}
	en, ok := w.eng.get(in.Engine)
	if !ok || en.info().Mode != in.Mode {
		return w.formErrors(c, map[string][]string{"engine": {"Pick an engine for this mode."}})
	}
	if err := en.ready(ctx); err != nil {
		return w.formErrors(c, map[string][]string{"engine": {describe(err)}})
	}
	t := task{Prompt: strings.TrimSpace(in.Prompt), Resolution: in.Resolution, Parts: w.longClips()}
	if in.Source != "" {
		if t.Source, err = w.st.media(ctx, in.Source); err != nil && !errors.Is(err, errNotFound) {
			return err
		}
	}
	if len(in.Refs) > 0 {
		if t.Refs, err = w.st.mediaByIDs(ctx, in.Refs); errors.Is(err, errNotFound) {
			return w.formErrors(c, map[string][]string{"refs": {"A selected reference no longer exists."}})
		} else if err != nil {
			return err
		}
	}
	fields := validate(en.info().Limits, t)
	var style runStyle
	if in.Mode == modeStyle {
		var problem string
		style, problem = w.checkStyle(ctx, in.Style)
		if problem != "" {
			if fields == nil {
				fields = map[string][]string{}
			}
			fields["style"] = append(fields["style"], problem)
		}
	}
	if p := w.prefs.get().Spend; fields == nil && p.CapOn && p.CapAction == "block" {
		if est, ok := estimateUSD(en.info(), t, in.Versions); ok {
			spent, err := w.monthSpend(ctx)
			if err != nil {
				return err
			}
			if spent+est > p.Cap {
				fields = map[string][]string{"engine": {fmt.Sprintf(
					"This run (≈ $%.2f) would pass your $%.0f monthly cap ($%.2f spent). Raise the cap in Settings, Spending.", est, p.Cap, spent)}}
			}
		}
	}
	if fields != nil {
		return w.formErrors(c, fields)
	}
	var pinned int64
	if in.Pin != "" {
		// Reuse prompt opened this from a take: render with its seed.
		if old, err := w.st.run(ctx, in.Pin); err == nil && old.Mode == in.Mode {
			pinned = seedOf(old)
		}
	}
	cards, err := w.startRuns(ctx, in.Mode, in.Engine, t, style, in.Versions, pinned)
	if err != nil {
		return err
	}
	if in.StoryShot != "" {
		if err := w.st.editStory(ctx, in.StoryProject, func(p *storyProject) error {
			e, err := storyEpisodeAt(p, in.StoryEpisode)
			if err != nil {
				return err
			}
			sh, err := storyShotAt(e, in.StoryShot)
			if err != nil {
				return err
			}
			for _, card := range cards {
				sh.Takes = append(sh.Takes, card.ID)
			}
			return nil
		}); err != nil {
			log.Printf("utsushi: attach takes to shot %s: %v; takes remain in library", in.StoryShot, err)
		}
	}
	return c.Fragment("runs-started", hyperchi.H{"runs": cards})
}

// startRuns stores and starts one run per version of t and returns their
// cards, newest first to match the feed.
// checkStyle reads the form's style choice and says what is wrong with it.
// A preset restyles the first frame locally, so it needs the proxy.
func (w *web) checkStyle(ctx context.Context, value string) (runStyle, string) {
	if id, ok := strings.CutPrefix(value, "image:"); ok {
		m, err := w.st.media(ctx, id)
		if err != nil || m.Kind != kindImage || m.Status != mediaReady {
			return runStyle{}, "The style image is gone. Pick a style again."
		}
		return runStyle{ImageID: id}, ""
	}
	if _, ok := presetByKey(value); !ok {
		return runStyle{}, "Pick a style."
	}
	if err := w.run.refs.ready(ctx); err != nil {
		return runStyle{}, "A preset restyles the first frame on your ComfyUI, which is not reachable. Check Settings, ComfyUI server."
	}
	return runStyle{Preset: value}, ""
}

// startRuns stores and starts one run per version. pinned, when set, is the
// seed of the take this one repeats, kept for a single version so the
// difference is only what changed.
func (w *web) startRuns(ctx context.Context, mode, engineID string, t task, style runStyle, versions int, pinned int64) ([]runView, error) {
	refIDs := []string{}
	for _, m := range t.Refs {
		refIDs = append(refIDs, m.ID)
	}
	batch := newID()
	started := now()
	cards := make([]runView, 0, versions)
	var lim limits
	least := 0.0
	if en, ok := w.eng.get(engineID); ok && t.Parts {
		lim, least = en.info().Limits, en.info().MinOutput
	}
	plan := planParts(lim, t.Source.Seconds)
	if len(plan) > 1 {
		// Split where the clip cuts, so each part opens on a fresh shot and
		// the joins fall on cuts: the cuts saved in the clip editor, else
		// the ones ffmpeg finds. Without cuts, the even plan stands.
		cuts, saved, err := w.st.clipCuts(ctx, t.Source.ID)
		if err != nil {
			log.Printf("utsushi: read the saved cuts of %s: %v", t.Source.ID, err)
		}
		if !saved {
			if cuts, err = w.frames.cuts(ctx, w.lib.path(t.Source)); err != nil {
				log.Printf("utsushi: find the cuts in %s: %v", t.Source.ID, err)
			}
		}
		plan = atCuts(lim, least, plan, t.Source.Seconds, cuts)
	}
	for i := range versions {
		r := Run{
			Parts: slices.Clone(plan),
			ID:    newID(), Batch: batch, Mode: mode, Engine: engineID, Prompt: t.Prompt,
			Resolution: t.Resolution, SourceID: t.Source.ID, RefIDs: refIDs,
			Style: style.Preset, StyleImageID: style.ImageID,
			Status: runPreparing, Detail: "Starting",
			// One key per version: retries of a version never bill twice,
			// and versions never collapse into one.
			IdempotencyKey: "utsushi-" + batch + "-" + string(rune('a'+i)),
			CreatedAt:      started.Add(time.Duration(i) * time.Millisecond),
		}
		r.Seed = runSeed(r.IdempotencyKey)
		if pinned > 0 && versions == 1 {
			r.Seed = pinned
		}
		if err := w.st.addRun(ctx, r); err != nil {
			return nil, err
		}
		w.run.start(r)
		cards = append([]runView{w.viewRun(ctx, r)}, cards...)
		w.sse.Broadcast("runs", sse.SSEMessage{Event: "run-added", Data: r.ID})
	}
	return cards, nil
}

// collectRun follows a lost run again and answers with its live card.
func (w *web) collectRun(c *hyperchi.Context) error {
	ctx := c.Ctx()
	err := w.run.collect(ctx, c.Param("id"))
	if errors.Is(err, errNotFound) {
		return hyperchi.NotFound("No such run.")
	}
	if err != nil {
		return c.HX().Trigger("toast", hyperchi.H{"message": sentence(err.Error())}).NoContent()
	}
	if strings.Contains(c.HX().CurrentURL(), "/runs/"+c.Param("id")) {
		return c.Redirect("/runs/" + c.Param("id"))
	}
	// The old card's stream closed when the run was lost, so answer with a
	// fresh card that subscribes again.
	r, err := w.st.run(ctx, c.Param("id"))
	if err != nil {
		return err
	}
	return c.Fragment("run-card", w.viewRun(ctx, r))
}

// runAgain starts a new run with a finished run's engine and inputs. It is a
// new generation with its own idempotency key, so a billed engine bills again.
func (w *web) runAgain(c *hyperchi.Context) error {
	ctx := c.Ctx()
	old, err := w.st.run(ctx, c.Param("id"))
	if errors.Is(err, errNotFound) {
		return hyperchi.NotFound("No such run.")
	}
	if err != nil {
		return err
	}
	toast := func(msg string) error {
		return c.HX().Trigger("toast", hyperchi.H{"message": msg}).NoContent()
	}
	en, ok := w.eng.get(old.Engine)
	if !ok {
		return toast("That engine is no longer available.")
	}
	if err := en.ready(ctx); err != nil {
		return toast(describe(err))
	}
	if old.Mode == modeLong {
		if _, err := w.longAgain(ctx, old); err != nil {
			return toast(sentence(err.Error()))
		}
		return toast("Started again: the same shots, made fresh.")
	}
	t := task{Prompt: old.Prompt, Resolution: old.Resolution, Parts: w.longClips()}
	if t.Source, err = w.st.media(ctx, old.SourceID); errors.Is(err, errNotFound) {
		return toast("The source clip was removed from the library.")
	} else if err != nil {
		return err
	}
	if t.Refs, err = w.st.mediaByIDs(ctx, old.RefIDs); errors.Is(err, errNotFound) {
		return toast("A reference was removed from the library.")
	} else if err != nil {
		return err
	}
	if fields := validate(en.info().Limits, t); fields != nil {
		for _, msgs := range fields {
			return toast(msgs[0])
		}
	}
	// A preset run restyles its first frame again: the old frame belongs to
	// the old run and goes when it is deleted. A visitor's own image is kept.
	style := runStyle{Preset: old.Style}
	if old.Style == "" {
		style.ImageID = old.StyleImageID
	}
	cards, err := w.startRuns(ctx, old.Mode, old.Engine, t, style, 1, seedOf(old))
	if err != nil {
		return err
	}
	if strings.Contains(c.HX().CurrentURL(), "/runs/"+old.ID) {
		return c.Redirect("/runs/" + cards[0].ID)
	}
	return c.Fragment("runs-started", hyperchi.H{"runs": cards})
}

func (w *web) formErrors(c *hyperchi.Context, fields map[string][]string) error {
	var msgs []string
	for _, key := range []string{"source", "style", "refs", "engine", "prompt", "resolution", "versions", "mode"} {
		msgs = append(msgs, fields[key]...)
	}
	return c.Status(http.StatusUnprocessableEntity).Fragment("form-errors", hyperchi.H{"errors": msgs})
}

func (w *web) cancelRun(c *hyperchi.Context) error {
	err := w.run.cancel(c.Ctx(), c.Param("id"))
	if errors.Is(err, errNotFound) {
		return hyperchi.NotFound("No such run.")
	}
	if err != nil {
		return c.HX().Trigger("toast", hyperchi.H{"message": err.Error()}).NoContent()
	}
	return c.NoContent()
}

func (w *web) deleteRun(c *hyperchi.Context) error {
	id := c.Param("id")
	err := w.removeRun(c.Ctx(), id)
	switch {
	case errors.Is(err, errNotFound):
		return hyperchi.NotFound("No such run.")
	case errors.Is(err, errRunActive):
		return c.HX().Trigger("toast", hyperchi.H{"message": "Cancel the run before deleting it."}).NoContent()
	case err != nil:
		return err
	}
	if c.HX().CurrentURL() != "" && strings.Contains(c.HX().CurrentURL(), "/runs/"+id) {
		return c.Redirect("/runs")
	}
	return c.HTML("")
}

// upload stores one dropped file. Bind is not used: it caps multipart
// bodies at 32 MiB, and source clips run to 200 MB.
func (w *web) upload(c *hyperchi.Context) error {
	req := c.Request()
	req.Body = http.MaxBytesReader(c.Response(), req.Body, maxUploadBytes)
	kind := c.Query("kind")
	target := "#upload-" + kind
	switch {
	case c.Query("for") == "style":
		target = "#upload-style"
	case c.Query("for") == "ref":
		target = "#upload-refvideo"
	}
	fail := func(msg string) error {
		return c.Status(http.StatusUnprocessableEntity).HX().Retarget(target+"-notice").Reswap("innerHTML").
			Fragment("notice", hyperchi.H{"message": msg})
	}
	if kind != kindVideo && kind != kindImage && kind != kindAudio {
		return hyperchi.BadRequest("Unknown upload kind.")
	}
	if err := req.ParseMultipartForm(8 << 20); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return fail("That file is over 200 MB.")
		}
		return fail("The upload did not arrive whole. Try again.")
	}
	defer func() {
		if err := req.MultipartForm.RemoveAll(); err != nil {
			log.Printf("utsushi: remove upload temp files: %v", err)
		}
	}()
	file, hdr, err := req.FormFile("file")
	if err != nil {
		return fail("Choose a file first.")
	}
	defer func() {
		if err := file.Close(); err != nil {
			log.Printf("utsushi: close upload: %v", err)
		}
	}()
	m, err := w.lib.store(file, originUpload, hdr.Filename, 0)
	var big errTooLarge
	switch {
	case errors.Is(err, errMediaType):
		return fail("Only JPEG, PNG, and WebP images, MP4 videos, and WAV or MP3 audio are accepted.")
	case errors.As(err, &big):
		return fail(fmt.Sprintf("That file is over the %d MB limit.", big.limit>>20))
	case errors.Is(err, errNoLength):
		return fail("That file has no readable length. Re-export it as MP4, WAV, or MP3 and try again.")
	case err != nil:
		return err
	}
	if m.Kind != kind {
		if rerr := w.lib.remove(m); rerr != nil {
			return rerr
		}
		return fail("That is not " + map[string]string{kindVideo: "a video", kindImage: "an image", kindAudio: "an audio file"}[kind] + ".")
	}
	m, err = w.keepUpload(c.Ctx(), m)
	if err != nil {
		return err
	}
	if kind == kindImage && c.Query("for") == "style" {
		return c.Fragment("style-own-tile", viewMedia(m))
	}
	if kind == kindAudio {
		return c.Fragment("audio-tile", viewMedia(m))
	}
	if kind == kindVideo && c.Query("for") == "ref" {
		return c.Fragment("ref-video-tile", viewMedia(m))
	}
	if kind == kindVideo {
		if err := w.rememberClip(c.Ctx(), m.ID); err != nil {
			return err
		}
		data, err := w.clipData(c.Ctx(), m.ID, false)
		if err != nil {
			return err
		}
		return c.Fragment("clip-block", data)
	}
	v := viewMedia(m)
	return c.Fragment("image-tile", v)
}

// serveMedia streams a stored file with range support, so videos seek.
func (w *web) serveMedia(c *hyperchi.Context) error {
	m, err := w.st.media(c.Ctx(), c.Param("id"))
	if errors.Is(err, errNotFound) || (err == nil && m.Status != mediaReady) {
		return hyperchi.NotFound("No such file.")
	}
	if err != nil {
		return err
	}
	f, err := w.lib.open(m)
	if err != nil {
		return hyperchi.NotFound("The file is missing from the library.")
	}
	defer func() {
		if err := f.Close(); err != nil {
			log.Printf("utsushi: close %s: %v", m.File, err)
		}
	}()
	h := c.Response().Header()
	h.Set("Content-Type", m.ContentType)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "private, max-age=86400, immutable")
	h.Set("Content-Disposition", mime.FormatMediaType("inline", map[string]string{"filename": downloadName(m)}))
	http.ServeContent(c.Response(), c.Request(), "", m.CreatedAt, f)
	return nil
}

// downloadName is the name a saved file gets: the library's own extension,
// which mediaTypes fixes per type. The system MIME list is no guide: on
// Ubuntu it lists .f4v first for video/mp4.
func downloadName(m Media) string { return "utsushi-" + m.ID + filepath.Ext(m.File) }

func (w *web) deleteMedia(c *hyperchi.Context) error {
	m, err := w.forgetMedia(c.Ctx(), c.Param("id"))
	switch {
	case errors.Is(err, errNotFound):
		return hyperchi.NotFound("No such file.")
	case errors.Is(err, errInUse):
		return c.HX().Trigger("toast", hyperchi.H{"message": "A run uses this file, so it stays."}).NoContent()
	case err != nil:
		return err
	}
	if err := w.lib.remove(m); err != nil {
		log.Printf("utsushi: remove %s: %v", m.File, err)
	}
	c.HX().Trigger("media-deleted", hyperchi.H{"id": m.ID})
	if c.Query("for") == "clip" {
		data, err := w.pageClip(c.Ctx(), "")
		if err != nil {
			return err
		}
		return c.Fragment("clip-block", data)
	}
	return c.HTML("")
}

// forgetMedia deletes a library row, and clears the remembered clip when it
// was that one, so the studio opens empty rather than on the next clip.
func (w *web) forgetMedia(ctx context.Context, id string) (Media, error) {
	m, err := w.st.deleteMedia(ctx, id)
	if err != nil {
		return Media{}, err
	}
	saved, err := w.st.studioValue(ctx, "clip")
	if err != nil {
		return Media{}, err
	}
	if saved == id {
		if err := w.rememberClip(ctx, ""); err != nil {
			return Media{}, err
		}
	}
	return m, nil
}

// Live updates. Each run card and pending tile subscribes to its own
// channel; the server pushes the re-rendered body on every change.

func (w *web) publishRun(r Run) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	html, err := w.app.RenderToString("run-card-body", w.viewRun(ctx, r))
	if err != nil {
		log.Printf("utsushi: render run %s: %v", r.ID, err)
		return
	}
	w.sse.BroadcastSwap("run-"+r.ID, string(html))
	if !runActive(r.Status) {
		w.sse.Broadcast("run-"+r.ID, sse.SSEMessage{Event: "done", Data: "done"})
	}
}

func (w *web) publishMedia(m Media) {
	tmpl, view := "image-tile-body", any(viewMedia(m))
	switch m.Origin {
	case originSwatch:
		p, ok := presetByKey(m.Label)
		if !ok {
			return
		}
		tmpl, view = "style-tile-body", viewStyle(p, m, true)
	case originTool:
		tmpl, view = w.publishToolItem(m)
	}
	html, err := w.app.RenderToString(tmpl, view)
	if err != nil {
		log.Printf("utsushi: render media %s: %v", m.ID, err)
		return
	}
	w.sse.BroadcastSwap("media-"+m.ID, string(html))
	if m.Status != mediaPending {
		w.sse.Broadcast("media-"+m.ID, sse.SSEMessage{Event: "done", Data: "done"})
	}
}

// webPublisher adapts web to the runner's publisher. The runner is built
// before web, so the adapter is filled in afterwards.
type webPublisher struct{ w *web }

func (p *webPublisher) run(r Run) {
	if p.w != nil {
		p.w.publishRun(r)
	}
}

func (p *webPublisher) media(m Media) {
	if p.w != nil {
		p.w.publishMedia(m)
	}
}

// styleViews are the preset tiles with their swatches, presets first, then
// the visitor's own style images.
func (w *web) styleViews(ctx context.Context) ([]styleView, error) {
	have, err := w.st.swatches(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]styleView, 0, len(stylePresets))
	for i, p := range stylePresets {
		m, ok := have[p.Key]
		v := viewStyle(p, m, ok)
		v.Selected = i == 0
		out = append(out, v)
	}
	return out, nil
}

// studioPath is the studio page for a mode.
func studioPath(mode string) string {
	switch mode {
	case modeSwap:
		return "/swap"
	case modeStyle:
		return "/style"
	case modeLong:
		return "/long"
	}
	return "/"
}

// styleTile is one preset tile, for a swatch tile catching up after its
// stream was paused.
func (w *web) styleTile(c *hyperchi.Context) error {
	p, ok := presetByKey(c.Param("key"))
	if !ok {
		return hyperchi.NotFound("No such style.")
	}
	have, err := w.st.swatches(c.Ctx())
	if err != nil {
		return err
	}
	m, found := have[p.Key]
	return c.Fragment("style-tile", viewStyle(p, m, found))
}

// pickerEngines are a mode's engines as Settings show them: engines turned
// off are left out. Partner engines need a key to appear; explicit cloud
// workflow choices remain visible with their readiness warning. With
// "Hide engines that aren't ready", engines that cannot run now are left out.
func (w *web) pickerEngines(ctx context.Context, mode string, p Prefs) []engine {
	var out []engine
	for _, en := range w.eng.forMode(mode) {
		in := en.info()
		if (slices.Contains(p.Engines.Off, in.ID) || slices.Contains(p.Engines.Off, engineModelID(in.ID))) || (in.Billed && engineModelID(in.ID) == in.ID && w.cloud.apiKey() == "") {
			continue
		}
		if p.Engines.HideUnready && en.ready(ctx) != nil {
			continue
		}
		out = append(out, en)
	}
	// Best tested first, so the first engine is the one that did best.
	byRating(out, func(en engine) string { return videoRating(mode, en.info().ID).Tier })
	return out
}

// estimateUSD is a run's estimate from a published per-second rate, and
// whether there is one. The output runs as long as the clip, clamped to the
// engine's range, as Wan 3.0 does.
// outputLength is how long an engine's output runs for a clip: the clip's
// length, rounded, between the model's shortest output and the clip limit.
func outputLength(in engineInfo, clip float64) float64 {
	out := math.Max(math.Round(clip), math.Max(in.MinOutput, 2))
	if in.Limits.MaxSeconds > 0 {
		out = math.Min(out, in.Limits.MaxSeconds)
	}
	return out
}

func estimateUSD(in engineInfo, t task, versions int) (float64, bool) {
	if !in.Billed {
		return 0, true
	}
	rate, ok := in.Rates[t.Resolution]
	if !ok {
		return 0, false
	}
	// A clip in parts bills each part: its output and, where the model bills
	// the clip, its segment.
	clips := []float64{t.Source.Seconds}
	if t.Parts {
		if parts := planParts(in.Limits, t.Source.Seconds); parts != nil {
			clips = clips[:0]
			for _, p := range parts {
				clips = append(clips, p.Length)
			}
		}
	}
	var one float64
	for _, c := range clips {
		one += rate*outputLength(in, c) + in.InputRates[t.Resolution]*c
	}
	return one * float64(versions), true
}

// longClips reports whether clips longer than an engine takes can run in
// parts here, which needs ffmpeg.
func (w *web) longClips() bool { return w.frames != nil && w.frames.available() }
