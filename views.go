package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
)

// View models: only what a template reads, so database rows never reach the
// page whole.

type mediaView struct {
	ID, Kind, Origin, URL, Label, Note, Error, Length string
	// Aspect is the CSS aspect-ratio as one number ("0.5625"), empty when
	// unknown. A "w / h" pair would be dropped by html/template's style
	// sanitizer, which refuses the slash.
	// Meta is the clip's facts line ("720×1280 · 6.4 s").
	Aspect, Meta           string
	Seconds                float64
	Pending, Failed, Ready bool
	Local, Selected        bool
	Badge                  string // "local" or "cloud" for an image made here
	// Shape is the tile's place in a bento: "wide", "tall", or empty for
	// square-ish. The script's packer (bentoLayout) sizes it from that.
	Shape string
	Tag   string // "@1" on a run's references
}

// tileShape is a bento tile's shape for media w by h: wide from 3:2, tall
// from 2:3, so sheets and portraits get cells near their own shape.
func tileShape(w, h int) string { return ratioShape(float64(w) / float64(h)) }

func ratioShape(r float64) string {
	switch {
	case r >= 1.45:
		return "wide"
	case r <= 0.7:
		return "tall"
	}
	return ""
}

// shapeOf is the bento shape of a CSS ratio ("0.5625"), empty when it does
// not read.
func shapeOf(aspect string) string {
	r, err := strconv.ParseFloat(aspect, 64)
	if err != nil || r <= 0 {
		return ""
	}
	return ratioShape(r)
}

func viewMedia(m Media) mediaView {
	v := mediaView{
		ID: m.ID, Kind: m.Kind, Origin: m.Origin, Label: m.Label, Note: m.Note, Error: m.Error,
		Pending: m.Status == mediaPending, Failed: m.Status == mediaFailed, Ready: m.Status == mediaReady,
		Local: m.Origin == originLocal, Seconds: m.Seconds,
	}
	switch m.Origin {
	case originLocal:
		v.Badge = "local"
	case originCloud:
		v.Badge = "cloud"
	}
	if v.Ready {
		v.URL = "/media/" + m.ID
	}
	var facts []string
	if m.Width > 0 && m.Height > 0 {
		v.Aspect = ratio(m.Width, m.Height)
		v.Shape = tileShape(m.Width, m.Height)
		facts = append(facts, fmt.Sprintf("%d×%d", m.Width, m.Height))
	}
	if m.Seconds > 0 {
		v.Length = seconds(m.Seconds)
		facts = append(facts, v.Length)
	}
	v.Meta = strings.Join(facts, " · ")
	if v.Pending && v.Note == "" {
		v.Note = "Waiting for the GPU"
	}
	return v
}

type runView struct {
	ID, Mode, ModeLabel, EngineName, Via, Code, Prompt, Resolution string
	// StudioURL is the page of the run's mode; Reuse opens it with the
	// run's prompt, or for a long video, all of its setup.
	StudioURL string
	// Shots is a long video's shot count.
	Shots                                                int
	Status, StatusLabel, Detail, Error, Credits, Created string
	SourceURL, ResultURL                                 string
	// Tone colors the status pill: queued, generating, done, failed, lost,
	// or muted. Group is the feed filter it falls under: running, done, or
	// failed.
	Tone, Group string
	// Aspect is the card's CSS aspect-ratio, from the result once there is
	// one and the source until then, so footage is never letterboxed.
	// Shape is its bento shape (see mediaView.Shape).
	Aspect, AspectLabel, Shape string
	// Version is "v1/2" for runs started together, empty for a single run.
	Version string
	// Length is the source clip's length; ResultLength the output's.
	Length, ResultLength string
	// ErrorTitle and ErrorHint split a failure into its cause and the rest.
	ErrorTitle, ErrorHint string
	RunTime, Cost         string
	Back                  string // the studio this run came from
	// StyleName and StyleURL are a Style run's preset (or "Your image")
	// and its style frame once made.
	StyleName, StyleURL   string
	Refs                  []mediaView
	Percent               int
	Active, Succeeded     bool
	Failed, Indeterminate bool
	// Lost runs can be collected again; finished runs can run again.
	// Billed says whether running again charges.
	Lost, CanRunAgain, Billed bool
}

var statusLabels = map[string]string{
	runPreparing:   "Preparing",
	runQueued:      "Queued",
	runRunning:     "Generating",
	runSucceeded:   "Done",
	runFailed:      "Failed",
	runCanceled:    "Canceled",
	runInterrupted: "Interrupted",
	runLost:        "Connection lost",
}

var statusTones = map[string]string{
	runPreparing:   "queued",
	runQueued:      "queued",
	runRunning:     "generating",
	runSucceeded:   "done",
	runFailed:      "failed",
	runCanceled:    "muted",
	runInterrupted: "failed",
	runLost:        "lost",
}

func (w *web) viewRun(ctx context.Context, r Run) runView {
	v := runView{
		ID: r.ID, Mode: r.Mode, ModeLabel: modeLabels[r.Mode], Prompt: r.Prompt, Resolution: r.Resolution,
		StudioURL: studioPath(r.Mode),
		Status:    r.Status, StatusLabel: statusLabels[r.Status], Detail: r.Detail, Error: r.Error,
		Credits: r.Credits, Created: ago(r.CreatedAt), Tone: statusTones[r.Status],
		Active: runActive(r.Status), Succeeded: r.Status == runSucceeded,
		Failed:  r.Status == runFailed || r.Status == runInterrupted,
		Percent: int(r.Progress * 100),
		Back:    studioPath(r.Mode),
	}
	if r.SourceID != "" {
		v.SourceURL = "/media/" + r.SourceID
	}
	if r.Mode == modeLong {
		// A long video's size is its own, and its length the shots'.
		v.Resolution = fmt.Sprintf("%d×%d", r.Width, r.Height)
		v.Aspect, v.AspectLabel = ratio(r.Width, r.Height), fmt.Sprintf("%d×%d", r.Width, r.Height)
		total := 0.0
		for _, p := range r.Parts {
			total += p.Length
		}
		v.Shots = len(r.Parts)
		v.Length = seconds(total)
	}
	if r.Mode == modeStyle {
		v.StyleName = "Your image"
		if p, ok := presetByKey(r.Style); ok {
			v.StyleName = p.Name
		}
		if r.StyleImageID != "" {
			v.StyleURL = "/media/" + r.StyleImageID
		}
	}
	switch {
	case v.Active:
		v.Group = "running"
	case v.Succeeded:
		v.Group = "done"
	case r.Status != runCanceled:
		v.Group = "failed"
	}
	// A Router queue reports no fraction, only a state.
	v.Indeterminate = v.Active && r.Progress == 0
	v.Lost = r.Status == runLost
	v.CanRunAgain = !v.Active && !v.Lost
	v.ErrorTitle, v.ErrorHint = splitError(r.Error)
	if en, ok := w.eng.get(r.Engine); ok {
		in := en.info()
		v.EngineName, v.Via, v.Code, v.Billed = in.Name, in.Via, in.Code, in.Billed
	} else {
		v.EngineName = r.Engine
	}
	var output float64
	if !r.FinishedAt.IsZero() {
		v.RunTime = span(r.FinishedAt.Sub(r.CreatedAt))
	}
	if src, err := w.st.media(ctx, r.SourceID); err == nil && r.Mode != modeLong {
		v.Aspect, v.AspectLabel = aspectOf(src)
		if src.Seconds > 0 {
			v.Length = seconds(src.Seconds)
		}
	}
	if r.ResultID != "" {
		v.ResultURL = "/media/" + r.ResultID
		if res, err := w.st.media(ctx, r.ResultID); err == nil {
			if a, label := aspectOf(res); a != "" {
				v.Aspect, v.AspectLabel = a, label
			}
			if res.Seconds > 0 {
				v.ResultLength = seconds(res.Seconds)
				output = res.Seconds
			}
		}
	}
	if v.Aspect == "" {
		v.Aspect, v.AspectLabel = ratio(9, 16), "9:16"
	}
	v.Shape = shapeOf(v.Aspect)
	if en, ok := w.eng.get(r.Engine); ok {
		v.Cost = runCost(en.info(), r, output)
	}
	if batch, err := w.st.batchRuns(ctx, r.Batch); err == nil && len(batch) > 1 {
		i := slices.IndexFunc(batch, func(b Run) bool { return b.ID == r.ID })
		v.Version = fmt.Sprintf("v%d/%d", i+1, len(batch))
	}
	refs, err := w.st.mediaByIDs(ctx, r.RefIDs)
	if err != nil {
		// The card still renders; only the reference strip is missing.
		log.Printf("utsushi: run %s references: %v", r.ID, err)
	}
	tags := refTags(refs)
	for i, m := range refs {
		mv := viewMedia(m)
		mv.Tag = tags[i]
		v.Refs = append(v.Refs, mv)
	}
	return v
}

// siblingView is one version in the viewer's switcher.
type siblingView struct {
	ID, Label, ThumbURL string
	Current, Ready      bool
}

// viewSiblings lists the versions started with r, for the result viewer.
func (w *web) viewSiblings(ctx context.Context, r Run) []siblingView {
	batch, err := w.st.batchRuns(ctx, r.Batch)
	if err != nil || len(batch) < 2 {
		return nil
	}
	out := make([]siblingView, 0, len(batch))
	for i, b := range batch {
		s := siblingView{ID: b.ID, Label: fmt.Sprintf("v%d", i+1), Current: b.ID == r.ID, Ready: b.Status == runSucceeded}
		if b.ResultID != "" {
			s.ThumbURL = "/media/" + b.ResultID
		} else {
			s.ThumbURL = "/media/" + b.SourceID
		}
		out = append(out, s)
	}
	return out
}

// aspectOf is a media item's CSS aspect-ratio and its label ("9:16"), both
// empty when its size is unknown.
func aspectOf(m Media) (string, string) {
	if m.Width <= 0 || m.Height <= 0 {
		return "", ""
	}
	return ratio(m.Width, m.Height), ratioLabel(m.Width, m.Height)
}

// ratio is w/h as a CSS aspect-ratio number.
func ratio(w, h int) string { return fmt.Sprintf("%.4f", float64(w)/float64(h)) }

// ratioLabel names a frame's shape by the nearest common ratio, since
// encoders round sizes (480×854 is 9:16, not 240:427). An uncommon shape
// reads as its size.
func ratioLabel(w, h int) string {
	r := float64(w) / float64(h)
	for _, c := range [][2]int{{9, 16}, {16, 9}, {1, 1}, {4, 3}, {3, 4}, {4, 5}, {5, 4}, {2, 3}, {3, 2}, {21, 9}} {
		if math.Abs(r-float64(c[0])/float64(c[1]))/r < 0.02 {
			return fmt.Sprintf("%d:%d", c[0], c[1])
		}
	}
	return fmt.Sprintf("%d×%d", w, h)
}

// splitError separates a failure's first sentence, the cause, from the
// rest, which reads as the hint below it.
func splitError(msg string) (string, string) {
	if i := strings.Index(msg, ". "); i > 0 && i < len(msg)-2 {
		return msg[:i+1], strings.TrimSpace(msg[i+2:])
	}
	return msg, ""
}

// runCost is what a run cost, as far as anything published says: the
// credits a provider reported, free on the local GPU, an estimate from a
// published per-second rate, or who bills it.
func runCost(in engineInfo, r Run, output float64) string {
	rate, published := in.Rates[r.Resolution]
	switch {
	case r.Credits != "":
		return r.Credits + " credits"
	case !in.Billed:
		return "Free"
	case published && output > 0:
		return fmt.Sprintf("≈ $%.2f (estimate)", rate*output)
	}
	return "Billed by " + in.Via
}

// span is a run time such as "3 m 12 s".
func span(d time.Duration) string {
	d = d.Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%d s", int(d.Seconds()))
	}
	return fmt.Sprintf("%d m %d s", int(d.Minutes()), int(d.Seconds())%60)
}

// resOpt is one resolution in the segmented control, with its price.
type resOpt struct {
	Value, Label, Price string
	Selected            bool
}

type engineView struct {
	Featured                                  string
	Advice                                    modelAdvice
	ID, Name, Via, Code, Blurb, Offline, Hint string
	RefRule, RefsFor                          string
	// Tier, Meter, and Note are the engine's tested rating for its mode.
	Tier, Meter, RatingNote      string
	CostLabel                    string
	Free                         bool
	Resolutions                  []resOpt
	MinRefs, MaxRefs             int
	MinSeconds, MaxSeconds       float64
	PromptRequired, Billed, Tags bool
	PromptMax                    int
	Selected                     bool
	// Facts is the engine's numbers for the studio script: rates, clip
	// limits, and reference limits, as JSON.
	Facts string
}

// viewOpts are the Settings that change how an engine is shown.
type viewOpts struct {
	Hardware   hardwareInfo
	Resolution string // the default resolution's number, such as "720"
	Estimates  bool   // show prices
	LongClips  bool   // clips past the engine's limit run in parts (ffmpeg is here)
}

func viewEngine(ctx context.Context, en engine, selected bool, opts viewOpts) engineView {
	in := en.info()
	l := in.Limits
	v := engineView{
		Featured: featuredModel(in.ID), ID: in.ID, Name: in.Name, Via: in.Via, Code: in.Code, Blurb: in.Blurb, Selected: selected,
		MinRefs: l.MinRefs, MaxRefs: l.MaxRefs, MinSeconds: l.MinSeconds, MaxSeconds: l.MaxSeconds,
		PromptRequired: l.PromptRequired, PromptMax: l.PromptMax,
		Billed: in.Billed, Tags: in.Tags,
		Hint: limitHint(l), RefRule: refRule(l), RefsFor: l.RefsFor,
	}
	r := videoRating(in.Mode, in.ID)
	v.Tier, v.Meter, v.RatingNote = r.Tier, tierMeter(r.Tier), r.Note
	v.CostLabel, v.Free = costLabel(in)
	if !in.Billed && in.Via == "Your ComfyUI" {
		v.Advice = localAdvice(in.ID, opts.Hardware)
	} else {
		price := in.Rates[l.DefaultResolution]
		for res, rate := range in.Rates {
			if strings.HasPrefix(res, opts.Resolution) && opts.Resolution != "" {
				price = rate
				break
			}
		}
		v.Advice = cloudAdvice(price, r.Tier, "output second")
	}

	if !opts.Estimates && in.Billed {
		v.CostLabel = "Billed"
	}
	preferred := l.DefaultResolution
	for _, r := range l.Resolutions {
		if opts.Resolution != "" && strings.HasPrefix(r, opts.Resolution) {
			preferred = r
		}
	}
	for _, r := range l.Resolutions {
		o := resOpt{Value: r, Label: strings.ToLower(r), Selected: r == preferred}
		if rate, ok := in.Rates[r]; ok && (opts.Estimates || !in.Billed) {
			o.Price = "free"
			if rate > 0 {
				o.Price = fmt.Sprintf("$%.2f", rate)
			}
		}
		v.Resolutions = append(v.Resolutions, o)
	}
	facts, err := json.Marshal(map[string]any{
		"rates": in.Rates, "billed": in.Billed, "via": in.Via,
		"minSeconds": l.MinSeconds, "maxSeconds": l.MaxSeconds, "minRefs": l.MinRefs, "maxRefs": l.MaxRefs,
		"promptRequired": l.PromptRequired, "trimSeconds": l.TrimSeconds, "estimates": opts.Estimates,
		"promptMax": l.PromptMax, "refRule": refRule(l), "refsFor": l.RefsFor,
		"inputRates": in.InputRates, "minOutput": in.MinOutput,
		"maxVideoRefs": l.MaxVideoRefs, "maxAudioRefs": l.MaxAudioRefs,
		"refVideoTotal": l.RefVideoTotal, "refAudioTotal": l.RefAudioTotal,
		"longMax": longMax(opts.LongClips), "overlap": partOverlap, "margin": partMargin, "trimOver": trimOver,
	})
	if err == nil {
		v.Facts = string(facts)
	}
	if err := en.ready(ctx); err != nil {
		v.Offline = describe(err)
	}
	return v
}

// costLabel is the engine's cost chip: "Free · your GPU", the published
// per-second range from lowest to highest, or who bills it when no price is
// published.
func costLabel(in engineInfo) (string, bool) {
	if !in.Billed {
		return "Free · your GPU", true
	}
	if len(in.Rates) == 0 {
		return "Billed per run", false
	}
	lo, hi := math.Inf(1), 0.0
	for _, r := range in.Rates {
		lo, hi = math.Min(lo, r), math.Max(hi, r)
	}
	label := fmt.Sprintf("$%.2f–%.2f/s", lo, hi)
	if fmt.Sprintf("%.2f", lo) == fmt.Sprintf("%.2f", hi) {
		label = fmt.Sprintf("$%.2f/s", lo)
	}
	// Some models bill the clip they are given as well as their output.
	if len(in.InputRates) > 0 {
		label += " + clip"
	}
	return label, false
}

// limitHint is an engine's limits as one chip: clip length, then how many
// references.
func limitHint(l limits) string {
	var parts []string
	switch {
	case l.MinSeconds > 0 && l.MaxSeconds > 0:
		parts = append(parts, fmt.Sprintf("%g–%g s", l.MinSeconds, l.MaxSeconds))
	case l.MaxSeconds > 0:
		parts = append(parts, fmt.Sprintf("≤ %g s", l.MaxSeconds))
	case l.MinSeconds > 0:
		parts = append(parts, fmt.Sprintf("≥ %g s", l.MinSeconds))
	}
	if l.TrimSeconds > 0 {
		parts = append(parts, fmt.Sprintf("uses first %g s", l.TrimSeconds))
	}
	switch {
	case l.MaxRefs == 0:
		parts = append(parts, "no images")
	case l.MinRefs == l.MaxRefs:
		parts = append(parts, fmt.Sprintf("%d image%s", l.MaxRefs, plural(l.MaxRefs)))
	case l.MinRefs == 0:
		parts = append(parts, fmt.Sprintf("up to %d image%s", l.MaxRefs, plural(l.MaxRefs)))
	default:
		parts = append(parts, fmt.Sprintf("%d–%d images", l.MinRefs, l.MaxRefs))
	}
	if l.MaxVideoRefs > 0 || l.MaxAudioRefs > 0 {
		parts = append(parts, "video · audio")
	}
	if l.PromptRequired {
		parts = append(parts, "prompt")
	}
	return strings.Join(parts, " · ")
}

// refRule is how many references of each kind an engine takes, in words.
func refRule(l limits) string {
	var r string
	switch {
	case l.MaxRefs == 0:
		r = "No reference images"
	case l.MinRefs == 0:
		r = fmt.Sprintf("Up to %d image%s · optional", l.MaxRefs, plural(l.MaxRefs))
	case l.MinRefs == l.MaxRefs:
		r = fmt.Sprintf("%d image%s", l.MaxRefs, plural(l.MaxRefs))
	default:
		r = fmt.Sprintf("%d–%d images", l.MinRefs, l.MaxRefs)
	}
	if l.MaxVideoRefs > 0 {
		r += fmt.Sprintf(" · up to %d video%s", l.MaxVideoRefs, plural(l.MaxVideoRefs))
	}
	if l.MaxAudioRefs > 0 {
		r += fmt.Sprintf(" · up to %d audio", l.MaxAudioRefs)
	}
	return r
}

// ago is a short relative time for a card.
func ago(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d h ago", int(d.Hours()))
	}
	return t.Local().Format("Jan 2, 15:04")
}

// styleView is one preset tile: its name, and its swatch once made.
type styleView struct {
	Key, Name, URL, MediaID, Note string
	Pending, Selected             bool
}

func viewStyle(p stylePreset, m Media, have bool) styleView {
	v := styleView{Key: p.Key, Name: p.Name}
	if !have {
		return v
	}
	v.MediaID = m.ID
	switch m.Status {
	case mediaReady:
		v.URL = "/media/" + m.ID
	case mediaPending:
		v.Pending = true
		v.Note = m.Note
	}
	return v
}

// longMax is the longest clip the page lets run in parts, 0 without ffmpeg.
func longMax(on bool) float64 {
	if on {
		return longClipMax
	}
	return 0
}
