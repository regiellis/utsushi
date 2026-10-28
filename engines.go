package main

import (
	"context"
	"fmt"
	"io"
	"math/rand/v2"
	"slices"
	"strings"

	"github.com/regiellis/comfy-api-proxy-go/sdk"
)

// The two Genjutsu-style modes.
const (
	modeMotion = "motion" // recast the people in a clip, keeping its motion and camera
	modeSwap   = "swap"   // replace one object in a clip, keeping everything else
	modeStyle  = "style"  // restyle a whole clip, keeping its motion, camera, and framing
	modeLong   = "long"   // a video of shots from prompts and a cast, past any one model's length
)

var modeLabels = map[string]string{
	modeMotion: "Motion Transfer",
	modeSwap:   "Object Swap",
	modeStyle:  "Style",
	modeLong:   "Long video",
}

// limits are what an engine accepts. The form is checked against them
// before anything is uploaded or billed.
type limits struct {
	// MinRefs and MaxRefs count reference images. MaxVideoRefs and
	// MaxAudioRefs count reference videos and audio clips, zero where the
	// engine takes none.
	MinRefs, MaxRefs           int
	MaxVideoRefs, MaxAudioRefs int
	// RefVideoTotal caps the seconds of the source clip plus every reference
	// video, and RefAudioTotal the seconds of every reference audio clip.
	RefVideoTotal, RefAudioTotal float64
	MinSeconds, MaxSeconds       float64
	PromptRequired               bool
	PromptMax                    int
	// TrimSeconds is where a provider cuts a longer clip instead of
	// refusing it (Genjutsu: "a longer video is trimmed to 30 seconds").
	TrimSeconds       float64
	Resolutions       []string // empty: the engine takes no resolution
	DefaultResolution string
	// RefsFor says what the engine does with its references, in the
	// studio's words, so the picker can say what to pick.
	RefsFor string
	// ControlClip engines follow the clip as a control video: it must be a
	// depth, pose, or edge clip made on the Tools page.
	ControlClip bool
}

// engineInfo describes an engine to the page.
type engineInfo struct {
	ID   string
	Mode string
	Name string
	Via  string // "Comfy Router", "Your ComfyUI", "Comfy Cloud", or "Partner node"
	// Code is the engine's two-letter mark in the picker.
	Code string
	// Billed engines charge per generation; the page says so where they
	// are chosen.
	Billed bool
	// Rates is the published price per second of output for each
	// resolution, in USD. Nil when the provider publishes none (Comfy
	// Router's catalog carries billing facts but never prices), so the
	// page never shows an invented number.
	Rates map[string]float64
	// InputRates is the published USD per second of source clip, for
	// models that bill the clip they are given as well as their output
	// (Seedance, MiniMax H3). Nil when only output is billed.
	InputRates map[string]float64
	// MinOutput is the shortest output the model makes, in seconds; a
	// shorter clip still bills this much. Zero means 2.
	MinOutput float64
	// Tags is true when the prompt can name references as @1 and the
	// clip as @clip.
	Tags   bool
	Blurb  string
	Limits limits
}

// task is one generation's inputs, already stored in the library.
type task struct {
	Prompt     string
	Resolution string
	Source     Media
	// Refs are the references in pick order, of any kind; each kind is
	// numbered on its own (images @1, videos @v1, audio @a1).
	Refs []Media
	// Look is a Style run's preset description, empty for the visitor's own
	// style image. StyleImage is the style frame, also at Refs[0].
	Look       string
	StyleImage Media
	// Key makes the submission idempotent, so a retried submit cannot bill
	// twice.
	Key string
	// Parts is true when a clip longer than the engine takes, up to
	// longClipMax, may run in parts (the server has ffmpeg).
	Parts bool
	// Seed is the run's seed, the same for every part of a run in parts so
	// they render alike; zero draws a fresh one.
	Seed int64
	// A long video's shot: its frame count, frame size, and whether to use
	// the turbo LoRA. Source is then what the shot continues from (its last
	// moment, already cut), or zero for a shot that starts fresh.
	Frames        int
	Width, Height int
	Turbo         bool
	// FrontViews sends a character sheet (a wide image of one person shown
	// several times in a row) as its first figure only.
	FrontViews bool
	// RefSize is the R2V node's ref_image_size: "match" or "max".
	RefSize string
	// EndImage is the image a long video's shot ends on, or zero.
	EndImage Media
}

// seed is the task's seed below n, or a fresh one when it has none.
func (t task) seed(n int64) int64 {
	if t.Seed > 0 {
		return t.Seed % n
	}
	return rand.Int64N(n)
}

// tracker receives a run's progress. queued is called once the provider has
// accepted the work, with the id that collects it after a restart.
type tracker interface {
	queued(requestID string, position *int)
	running(detail string, progress float64)
}

// outcome is a finished generation: a public URL to download, or a writer
// that streams a v2 job output.
type outcome struct {
	URL     string
	Write   func(ctx context.Context, w io.Writer) error
	Credits string
}

// engine runs one kind of generation.
type engine interface {
	info() engineInfo
	// ready reports why the engine cannot run now, nil when it can.
	ready(ctx context.Context) error
	generate(ctx context.Context, t task, tr tracker) (outcome, error)
	// resume collects a generation submitted before a restart.
	resume(ctx context.Context, requestID string, tr tracker) (outcome, error)
	cancel(ctx context.Context, requestID string) error
}

// errOffline is an engine missing what it needs to run.
type errOffline struct{ reason string }

func (e errOffline) Error() string { return e.reason }

// engines is the registry, in display order.
type engines struct{ list []engine }

func (e *engines) get(id string) (engine, bool) {
	if base, target, ok := strings.Cut(id, "@"); ok {
		for _, en := range e.list {
			if en.info().ID == base {
				return engineAt(en, target)
			}
		}
		return nil, false
	}
	for _, en := range e.list {
		if en.info().ID == id {
			return en, true
		}
	}
	return nil, false
}

func (e *engines) forMode(mode string) []engine {
	var out []engine
	for _, en := range e.choices() {
		if en.info().Mode == mode {
			out = append(out, en)
		}
	}
	return out
}

// validate checks a task against an engine's limits and returns messages by
// form field, nil when it passes.
func validate(l limits, t task) map[string][]string {
	fields := map[string][]string{}
	add := func(field, msg string) { fields[field] = append(fields[field], msg) }
	if t.Source.Kind != kindVideo {
		add("source", "Choose a source video.")
	} else if cc, ok := controlOf(t.Source); l.ControlClip && (!ok || !cc.drives()) {
		add("source", "This engine follows a depth, pose, or edge clip: make one of your clip on the Tools page and pick it as the clip.")
	} else if t.Source.Seconds > 0 {
		if l.MinSeconds > 0 && t.Source.Seconds < l.MinSeconds {
			add("source", fmt.Sprintf("This engine needs a clip of at least %s; this one is %s.",
				seconds(l.MinSeconds), seconds(t.Source.Seconds)))
		}
		if l.MaxSeconds > 0 && t.Source.Seconds > l.MaxSeconds && !(t.Parts && splits(l, t.Source.Seconds)) {
			msg := fmt.Sprintf("This engine takes at most %s; this clip is %s.", seconds(l.MaxSeconds), seconds(t.Source.Seconds))
			if t.Parts {
				msg = fmt.Sprintf("Clips under %s run in parts; this clip is %s.", seconds(longClipStop), seconds(t.Source.Seconds))
			}
			add("source", msg)
		}
	}
	images, videos, audios := t.ofKind(kindImage), t.ofKind(kindVideo), t.ofKind(kindAudio)
	switch n := len(images); {
	case n < l.MinRefs:
		add("refs", fmt.Sprintf("Pick at least %d reference image%s.", l.MinRefs, plural(l.MinRefs)))
	case n > l.MaxRefs && l.MaxRefs == 0:
		add("refs", "This engine takes no reference images; clear the selection.")
	case n > l.MaxRefs:
		add("refs", fmt.Sprintf("This engine takes at most %d reference image%s.", l.MaxRefs, plural(l.MaxRefs)))
	}
	for _, c := range []struct {
		list       []Media
		max        int
		total      float64
		base       float64
		noun, with string
	}{
		{videos, l.MaxVideoRefs, l.RefVideoTotal, clipSeconds(l, t), "reference video", "the clip and reference videos"},
		{audios, l.MaxAudioRefs, l.RefAudioTotal, 0, "audio clip", "the audio clips"},
	} {
		switch {
		case len(c.list) > 0 && c.max == 0:
			add("refs", fmt.Sprintf("This engine takes no %ss; clear them.", c.noun))
		case len(c.list) > c.max:
			add("refs", fmt.Sprintf("This engine takes at most %d %s%s.", c.max, c.noun, plural(c.max)))
		}
		if len(c.list) > 0 && c.total > 0 {
			sum := c.base
			for _, m := range c.list {
				sum += m.Seconds
			}
			if sum > c.total {
				add("refs", fmt.Sprintf("Together %s may run %s; these run %s.", c.with, seconds(c.total), seconds(sum)))
			}
		}
	}
	for _, r := range t.Refs {
		if (r.Kind != kindImage && r.Kind != kindVideo && r.Kind != kindAudio) || r.Status != mediaReady {
			add("refs", "A selected reference is not a finished file.")
			break
		}
	}
	if l.PromptRequired && t.Prompt == "" {
		add("prompt", "Describe the change.")
	}
	if l.PromptMax > 0 && len([]rune(t.Prompt)) > l.PromptMax {
		add("prompt", fmt.Sprintf("Keep the prompt under %d characters.", l.PromptMax))
	}
	if len(l.Resolutions) > 0 && !slices.Contains(l.Resolutions, t.Resolution) {
		add("resolution", "Pick one of the listed resolutions.")
	}
	if len(fields) == 0 {
		return nil
	}
	return fields
}

// ofKind is the task's references of one kind, in pick order.
func (t task) ofKind(kind string) []Media {
	var out []Media
	for _, m := range t.Refs {
		if m.Kind == kind {
			out = append(out, m)
		}
	}
	return out
}

// refTags names each reference the way the studio's prompt does: images
// @1, @2, videos @v1, audio @a1, each kind counted on its own.
func refTags(refs []Media) []string {
	n := map[string]int{}
	tags := make([]string, len(refs))
	for i, m := range refs {
		n[m.Kind]++
		tags[i] = fmt.Sprintf("@%s%d", map[string]string{kindVideo: "v", kindAudio: "a"}[m.Kind], n[m.Kind])
	}
	return tags
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func seconds(s float64) string { return fmt.Sprintf("%.1f s", s) }

// failure is a generation the provider refused or could not finish. Its text
// is shown on the run card as it is.
type failure struct{ msg string }

func (f failure) Error() string { return f.msg }

func failedf(format string, args ...any) error { return failure{fmt.Sprintf(format, args...)} }

// jobFailure is a failed v2 job as the card shows it. A ComfyUI node error
// names the node, because a partner node's refusal (credits, moderation)
// arrives as that node's exception message.
func jobFailure(f *sdk.JobError) error {
	msg := strings.Join(strings.Fields(f.Message), " ")
	if msg == "" {
		msg = f.Code
	}
	if f.ClassType != nil && *f.ClassType != "" {
		return failedf("%s failed: %s", *f.ClassType, msg)
	}
	return failedf("%s (%s)", msg, f.Code)
}
