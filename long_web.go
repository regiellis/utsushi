package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/regiellis/hyperchi/hyperchi"
	"github.com/regiellis/hyperchi/hyperchi/extensions/sse"
)

// The Long video tab: a shot list made into one video. Each shot has its
// own prompt and length and either continues from the shot before or cuts
// to a new one; the cast is shared; the video can start from a prompt or
// extend a library video.

const (
	longEngineID = "minimax-h3-long"
	// longMaxShots bounds one long video; at 15 s a shot that is 5 minutes.
	longMaxShots = 20
)

// longSeconds are the shot lengths offered, in seconds of new footage.
var longSeconds = []float64{5, 8, 10, 12, 15}

// longShapes are the frame shapes, as aspect width and height.
var longShapes = map[string][2]int{"portrait": {9, 16}, "landscape": {16, 9}, "square": {1, 1}}

// longSizes are the frame sizes by their short side.
var longSizes = map[string]int{"draft": 480, "hd": 720}

type longForm struct {
	Start   string   `form:"start"`
	Refs    []string `form:"refs"`
	Prompts []string `form:"shot_prompt"`
	Seconds []string `form:"shot_seconds"`
	Joins   []string `form:"shot_join"`
	// Ends are the library images shots end on, "" for none.
	Ends    []string `form:"shot_end"`
	Shape   string   `form:"shape" validate:"oneof=portrait landscape square"`
	Size    string   `form:"size" validate:"oneof=draft hd"`
	Quality string   `form:"quality" validate:"oneof=turbo full"`
	// CastView is "front" to send each character sheet as its first figure.
	CastView string `form:"cast_view" validate:"oneof=front sheet"`
	// RefSize is the cast's detail: "match" (scaled to the video) or "max".
	RefSize string `form:"ref_size" validate:"oneof=match max"`
	// Soundtrack is a library audio file for the whole video, or empty, and
	// SoundtrackMode "replace" or "mix".
	Soundtrack     string `form:"soundtrack"`
	SoundtrackMode string `form:"soundtrack_mode" validate:"oneof=replace mix"`
	// From is the earlier take this one was set up from, and Keeps, per
	// shot, the index of the earlier shot it keeps, or "" to make it.
	From  string   `form:"from"`
	Keeps []string `form:"shot_keep"`
}

// longSetup is what the tab opens with: defaults, or an earlier take's
// setup when Reuse opened it.
type longSetup struct {
	From                           string
	Start                          string
	Cast                           string // JSON list of image ids in pick order
	Shape, Size, Quality, CastView string
	RefSize                        string
	Soundtrack, SoundtrackMode     string
	Shots                          []longShotView
}

// longShotView is one shot row as the tab shows it; Done marks a shot an
// earlier take already made, which can be kept.
type longShotView struct {
	Prompt  string
	Seconds float64
	Join    string
	End     string // the image it ends on
	Done    bool
}

// blankShot is the row a new shot starts as.
var blankShot = longShotView{Seconds: 10, Join: "continue"}

// longSetupOf is an earlier take's setup, for the tab to open with.
func longSetupOf(r Run) longSetup {
	cast, err := json.Marshal(r.RefIDs)
	if err != nil {
		cast = []byte("[]")
	}
	s := longSetup{From: r.ID, Start: r.SourceID, Cast: string(cast), Size: "draft", Quality: cmp.Or(r.Quality, "turbo"),
		CastView: cmp.Or(r.CastView, "sheet"), RefSize: cmp.Or(r.RefSize, "match"), Shape: "portrait",
		Soundtrack: r.SoundtrackID, SoundtrackMode: cmp.Or(r.SoundtrackMode, "replace")}
	switch {
	case r.Width > r.Height*6/5:
		s.Shape = "landscape"
	case r.Height <= r.Width*6/5:
		s.Shape = "square"
	}
	if min(r.Width, r.Height) >= 640 {
		s.Size = "hd"
	}
	for _, p := range r.Parts {
		frames := p.Frames
		join := "cut"
		if !p.Cut {
			frames -= longTail
			join = "continue"
		}
		s.Shots = append(s.Shots, longShotView{Prompt: p.Prompt, Seconds: nearestSeconds(float64(frames) / longFPS),
			Join: join, End: p.EndID, Done: p.ResultID != ""})
	}
	return s
}

// nearestSeconds is the offered shot length nearest to secs.
func nearestSeconds(secs float64) float64 {
	best := longSeconds[0]
	for _, s := range longSeconds {
		if math.Abs(s-secs) < math.Abs(best-secs) {
			best = s
		}
	}
	return best
}

// soundtrackMode is the run's soundtrack mode: empty without a soundtrack.
func soundtrackMode(in longForm) string {
	if in.Soundtrack == "" {
		return ""
	}
	return cmp.Or(in.SoundtrackMode, "replace")
}

// longPage is the Long video tab.
func (w *web) longPage(c *hyperchi.Context) error {
	ctx := c.Ctx()
	images, err := w.st.listMedia(ctx, kindImage, 120)
	if err != nil {
		return err
	}
	videos, err := w.st.listMedia(ctx, kindVideo, 60)
	if err != nil {
		return err
	}
	runs, err := w.st.listRuns(ctx, 12)
	if err != nil {
		return err
	}
	audios, err := w.st.listMedia(ctx, kindAudio, 60)
	if err != nil {
		return err
	}
	var starts []mediaView
	for _, m := range videos {
		if v := viewMedia(m); v.Ready {
			starts = append(starts, v)
		}
	}
	setup := longSetup{Cast: "[]", Shape: "portrait", Size: "draft", Quality: "turbo", CastView: "front", RefSize: "match",
		SoundtrackMode: "replace",
		Shots:          []longShotView{blankShot}}
	// Without a take to set up from, the cast is the one last picked here.
	cast, err := w.rememberedRefs(ctx, modeLong)
	if err != nil {
		return err
	}
	setup.Cast = cast
	if id := c.Query("reuse"); id != "" {
		if r, err := w.st.run(ctx, id); err == nil && r.Mode == modeLong && len(r.Parts) > 0 {
			setup = longSetupOf(r)
		} else if err != nil && !errors.Is(err, errNotFound) {
			return err
		}
	}
	data := hyperchi.H{
		"embedded":  storyEmbedded(c),
		"title":     "Long video · Utsushi",
		"nav":       modeLong,
		"mode":      modeLong,
		"images":    w.mediaViews(images, false),
		"starts":    starts,
		"runs":      w.runViews(ctx, runs),
		"seconds":   longSeconds,
		"setup":     setup,
		"audios":    w.mediaViews(audios, false),
		"blankShot": blankShot,
	}
	if en, ok := w.eng.get(longEngineID); ok {
		in := en.info()
		data["engine"] = in
		if err := en.ready(ctx); err != nil {
			data["offline"] = describe(err)
		}
	}
	return c.View("long", w.page(ctx, data))
}

// longPlan turns the form into the run's shots: each shot's frames on the
// model's grid and the seconds of new footage it adds after the lead-in it
// replays. It returns problems by field, nil when the form is sound.
func longPlan(in longForm, start *Media) ([]part, map[string][]string) {
	fields := map[string][]string{}
	add := func(field, msg string) { fields[field] = append(fields[field], msg) }
	n := len(in.Prompts)
	switch {
	case n == 0:
		add("shots", "Add a shot.")
	case n > longMaxShots:
		add("shots", fmt.Sprintf("A long video takes at most %d shots.", longMaxShots))
	case len(in.Seconds) != n || len(in.Joins) != n:
		add("shots", "Each shot needs a length and a join.")
	}
	if len(fields) > 0 {
		return nil, fields
	}
	at := 0.0
	if start != nil {
		at = start.Seconds
	}
	parts := make([]part, n)
	for i := range n {
		prompt := strings.TrimSpace(in.Prompts[i])
		secs, err := strconv.ParseFloat(in.Seconds[i], 64)
		if err != nil || !containsFloat(longSeconds, secs) {
			add("shots", fmt.Sprintf("Shot %d: pick one of the listed lengths.", i+1))
			continue
		}
		if prompt == "" {
			add("shots", fmt.Sprintf("Shot %d: say what happens.", i+1))
		}
		if len([]rune(prompt)) > 4000 {
			add("shots", fmt.Sprintf("Shot %d: keep the prompt under 4000 characters.", i+1))
		}
		// The first shot can only continue from a start video.
		cut := in.Joins[i] != "continue" || (i == 0 && start == nil)
		frames := longFrames(secs, !cut)
		skip := 0
		if !cut {
			skip = longTail
		}
		length := float64(frames-skip) / longFPS
		parts[i] = part{Start: at, Length: length, Prompt: prompt, Frames: frames, Cut: cut}
		if len(in.Ends) == n {
			parts[i].EndID = in.Ends[i]
		}
		at += length
	}
	if len(fields) > 0 {
		return nil, fields
	}
	return parts, nil
}

func containsFloat(list []float64, v float64) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// longSize is the frame size: the start video's shape when extending one,
// else the chosen shape, at the chosen short side on the model's 32 grid.
func longSize(in longForm, start *Media) (int, int) {
	short := longSizes[in.Size]
	if start != nil && start.Width > 0 && start.Height > 0 {
		return frameSizeBy(start.Width, start.Height, short, 32)
	}
	s := longShapes[in.Shape]
	return frameSizeBy(s[0], s[1], short, 32)
}

// createLong checks the form and starts the long video.
func (w *web) createLong(c *hyperchi.Context) error {
	in, err := hyperchi.Bind[longForm](c)
	if fields := hyperchi.FieldErrors(err); fields != nil {
		return w.formErrors(c, fields)
	}
	if err != nil {
		return err
	}
	ctx := c.Ctx()
	en, ok := w.eng.get(longEngineID)
	if !ok {
		return w.formErrors(c, map[string][]string{"engine": {"MiniMax H3 is not set up."}})
	}
	if err := en.ready(ctx); err != nil {
		return w.formErrors(c, map[string][]string{"engine": {describe(err)}})
	}
	if !w.longClips() {
		return w.formErrors(c, map[string][]string{"engine": {"A long video needs ffmpeg on the server to join its shots."}})
	}
	var start *Media
	if in.Start != "" {
		m, err := w.st.media(ctx, in.Start)
		if errors.Is(err, errNotFound) || (err == nil && (m.Kind != kindVideo || m.Status != mediaReady)) {
			return w.formErrors(c, map[string][]string{"start": {"The start video is gone. Pick another."}})
		}
		if err != nil {
			return err
		}
		start = &m
	}
	cast, err := w.st.mediaByIDs(ctx, in.Refs)
	if errors.Is(err, errNotFound) {
		return w.formErrors(c, map[string][]string{"refs": {"A cast image no longer exists."}})
	}
	if err != nil {
		return err
	}
	fields := map[string][]string{}
	for _, m := range cast {
		if m.Kind != kindImage || m.Status != mediaReady {
			fields["refs"] = []string{"The cast takes finished images only."}
		}
	}
	if max := en.info().Limits.MaxRefs; len(cast) > max {
		fields["refs"] = append(fields["refs"], fmt.Sprintf("The cast takes at most %d images.", max))
	}
	if in.Soundtrack != "" {
		if m, err := w.st.media(ctx, in.Soundtrack); err != nil || m.Kind != kindAudio || m.Status != mediaReady {
			fields["soundtrack"] = []string{"The soundtrack is gone. Pick another, or none."}
		}
	}
	for i, id := range in.Ends {
		if id == "" {
			continue
		}
		if m, err := w.st.media(ctx, id); err != nil || m.Kind != kindImage || m.Status != mediaReady {
			fields["shots"] = append(fields["shots"], fmt.Sprintf("Shot %d: the image it ends on is gone.", i+1))
		}
	}
	parts, problems := longPlan(in, start)
	for k, v := range problems {
		fields[k] = append(fields[k], v...)
	}
	if len(fields) > 0 {
		return w.formErrors(c, fields)
	}
	width, height := longSize(in, start)
	problem, err := w.keepShots(ctx, in, parts, start, width, height)
	if err != nil {
		return err
	}
	if problem != "" {
		return w.formErrors(c, map[string][]string{"shots": {problem}})
	}
	r := Run{
		ID: newID(), Batch: newID(), Mode: modeLong, Engine: longEngineID, Prompt: parts[0].Prompt,
		RefIDs: in.Refs, Status: runPreparing, Detail: "Starting", CreatedAt: now(),
		Parts: parts, Width: width, Height: height, Quality: in.Quality, CastView: in.CastView, RefSize: in.RefSize,
		SoundtrackID: in.Soundtrack, SoundtrackMode: soundtrackMode(in),
	}
	if r.RefIDs == nil {
		r.RefIDs = []string{}
	}
	if start != nil {
		r.SourceID = start.ID
	}
	r.IdempotencyKey = "utsushi-" + r.Batch + "-a"
	r.Seed = runSeed(r.IdempotencyKey)
	if in.From != "" {
		// A take set up from an earlier one renders with its seed, so what
		// changed is the only difference.
		if old, err := w.st.run(ctx, in.From); err == nil && old.Mode == modeLong {
			r.Seed = seedOf(old)
		}
	}
	view, err := w.launchLong(ctx, r)
	if err != nil {
		return err
	}
	return c.Fragment("runs-started", hyperchi.H{"runs": []runView{view}})
}

// keepShots copies the shots of the earlier take the form keeps, marking
// them Kept in parts. Keeps holds, per shot, the index of the earlier take's
// shot it keeps, or "" to make it. A shot is kept only when it is that
// shot unchanged, made the same way (cast, start, size, quality, sheet
// view, detail), and either starts a new shot or follows the shot that came
// just before it in the earlier take, also kept: a continuing shot carries
// on from the very frames before it. The copies are this take's own files,
// so deleting either take leaves the other whole.
func (w *web) keepShots(ctx context.Context, in longForm, parts []part, start *Media, width, height int) (string, error) {
	var keeps []int
	for i, k := range in.Keeps {
		if k == "" || i >= len(parts) {
			continue
		}
		n, err := strconv.Atoi(k)
		if err != nil {
			return fmt.Sprintf("Shot %d names no earlier shot to keep.", i+1), nil
		}
		for len(keeps) < i {
			keeps = append(keeps, -1)
		}
		keeps = append(keeps, n)
	}
	for len(keeps) < len(parts) {
		keeps = append(keeps, -1)
	}
	count := 0
	for _, k := range keeps {
		if k >= 0 {
			count++
		}
	}
	if count == 0 || in.From == "" {
		return "", nil
	}
	if count == len(parts) {
		return "Nothing changed since the earlier take. Edit a shot, or Remake one to make it again.", nil
	}
	old, err := w.st.run(ctx, in.From)
	if errors.Is(err, errNotFound) {
		return "The earlier take is gone, so its shots cannot be kept. Make them again.", nil
	}
	if err != nil {
		return "", err
	}
	startID := ""
	if start != nil {
		startID = start.ID
	}
	same := old.Mode == modeLong && old.SourceID == startID && slices.Equal(old.RefIDs, in.Refs) &&
		old.Width == width && old.Height == height && old.Quality == in.Quality && old.CastView == in.CastView &&
		cmp.Or(old.RefSize, "match") == in.RefSize
	if !same {
		return "The kept shots no longer match this setup. Make them again.", nil
	}
	for i, k := range keeps {
		if k < 0 {
			continue
		}
		p := parts[i]
		if k >= len(old.Parts) {
			return fmt.Sprintf("Shot %d names a shot the earlier take does not have.", i+1), nil
		}
		o := old.Parts[k]
		if o.ResultID == "" || o.Prompt != p.Prompt || o.Frames != p.Frames || o.Cut != p.Cut || o.EndID != p.EndID {
			return fmt.Sprintf("Shot %d changed since the earlier take, so it cannot be kept.", i+1), nil
		}
		// A continuing shot is kept only after the shot it continued in the
		// earlier take; the first shot continues the start video itself.
		follows := i > 0 && keeps[i-1] == k-1
		firstOfStart := i == 0 && k == 0
		if !p.Cut && !follows && !firstOfStart {
			return fmt.Sprintf("Shot %d continues a shot that is made again, so it must be made again too.", i+1), nil
		}
		m, err := w.st.media(ctx, o.ResultID)
		if err != nil {
			return fmt.Sprintf("Shot %d's file is gone. Make it again.", i+1), nil
		}
		cp, err := w.copyMedia(ctx, m, fmt.Sprintf("Shot %d of %d", i+1, len(parts)))
		if err != nil {
			return "", err
		}
		parts[i].ResultID, parts[i].Kept = cp.ID, true
	}
	return "", nil
}

// copyMedia stores a copy of a library file as a part of this take.
func (w *web) copyMedia(ctx context.Context, m Media, label string) (Media, error) {
	f, err := w.lib.open(m)
	if err != nil {
		return Media{}, err
	}
	cp, err := w.lib.store(f, originPart, label, 0)
	if cerr := f.Close(); cerr != nil && err == nil {
		err = cerr
	}
	if err != nil {
		return Media{}, err
	}
	if err := w.st.addMedia(ctx, cp); err != nil {
		return Media{}, errors.Join(err, w.lib.remove(cp))
	}
	return cp, nil
}

// launchLong stores a long video's run, starts it, and announces its card.
func (w *web) launchLong(ctx context.Context, r Run) (runView, error) {
	if err := w.st.addRun(ctx, r); err != nil {
		return runView{}, err
	}
	w.run.start(r)
	w.sse.Broadcast("runs", sse.SSEMessage{Event: "run-added", Data: r.ID})
	return w.viewRun(ctx, r), nil
}

// longAgain starts a long video again from the same shots, cast, and start:
// new generations, so the shots are made fresh.
func (w *web) longAgain(ctx context.Context, old Run) (Run, error) {
	r := old
	r.ID, r.Batch = newID(), newID()
	r.IdempotencyKey = "utsushi-" + r.Batch + "-a"
	r.Seed = seedOf(old)
	r.Status, r.Detail, r.Progress, r.RequestID, r.ResultID = runPreparing, "Starting", 0, "", ""
	r.Error, r.Credits, r.CostUSD, r.CreatedAt, r.FinishedAt = "", "", 0, now(), time.Time{}
	r.Parts = make([]part, len(old.Parts))
	for i, p := range old.Parts {
		r.Parts[i] = part{Start: p.Start, Length: p.Length, Prompt: p.Prompt, Frames: p.Frames, Cut: p.Cut, EndID: p.EndID}
	}
	if r.SourceID != "" {
		if _, err := w.st.media(ctx, r.SourceID); err != nil {
			return Run{}, fmt.Errorf("the start video: %w", err)
		}
	}
	if _, err := w.launchLong(ctx, r); err != nil {
		return Run{}, err
	}
	return r, nil
}
