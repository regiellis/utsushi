package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/regiellis/hyperchi/hyperchi"
)

// The clip editor shows a video's cuts on a timeline: the ones ffmpeg
// finds, or the ones saved for it. Saved cuts are where a run in parts
// splits the clip. Split makes 2 or 3 new library clips at the cuts; the
// clip itself stays.

const (
	// clipEdge keeps a cut this far from either end of the clip.
	clipEdge = 0.5
	// clipShortest is the shortest piece Split makes.
	clipShortest = 1.0
	// clipMostPieces is how many clips Split makes at most.
	clipMostPieces = 3
)

// clipEditorView is the editor dialog's content.
type clipEditorView struct {
	ID, Label, URL string
	Seconds        float64
	Strip          bool
	Cuts           string // JSON, in seconds: saved, else detected
	Detected       string // JSON, the cuts ffmpeg finds
	Saved          bool
}

// clipEditor opens the editor for a library video.
func (w *web) clipEditor(c *hyperchi.Context) error {
	ctx := c.Ctx()
	m, err := w.st.media(ctx, c.Param("id"))
	if errors.Is(err, errNotFound) || (err == nil && (m.Kind != kindVideo || m.Status != mediaReady)) {
		return hyperchi.NotFound("No such clip.")
	}
	if err != nil {
		return err
	}
	if !w.longClips() {
		return c.HX().Trigger("toast", hyperchi.H{"message": "The clip editor needs ffmpeg on the server."}).NoContent()
	}
	detected, err := w.frames.cuts(ctx, w.lib.path(m))
	if err != nil {
		return err
	}
	saved, ok, err := w.st.clipCuts(ctx, m.ID)
	if err != nil {
		return err
	}
	cuts := detected
	if ok {
		cuts = saved
	}
	return c.Fragment("clip-editor", clipEditorView{
		ID: m.ID, Label: m.Label, URL: "/media/" + m.ID, Seconds: m.Seconds, Strip: w.frames.available(),
		Cuts: jsonFloats(cleanCuts(cuts, m.Seconds)), Detected: jsonFloats(cleanCuts(detected, m.Seconds)), Saved: ok,
	})
}

func jsonFloats(v []float64) string {
	parts := make([]string, len(v))
	for i, f := range v {
		parts[i] = strconv.FormatFloat(f, 'f', 3, 64)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// cleanCuts sorts cuts, rounds them to milliseconds, and drops any too near
// an end or another cut.
func cleanCuts(cuts []float64, length float64) []float64 {
	out := []float64{}
	sorted := slices.Clone(cuts)
	slices.Sort(sorted)
	for _, t := range sorted {
		t = math.Round(t*1000) / 1000
		if t < clipEdge || t > length-clipEdge {
			continue
		}
		if len(out) > 0 && t-out[len(out)-1] < clipEdge {
			continue
		}
		out = append(out, t)
	}
	return out
}

// formCuts reads the cuts a form sends.
func formCuts(c *hyperchi.Context, length float64) ([]float64, error) {
	if err := c.Request().ParseForm(); err != nil {
		return nil, err
	}
	var cuts []float64
	for _, v := range c.Request().Form["cuts"] {
		t, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return nil, fmt.Errorf("a cut is not a time: %q", v)
		}
		cuts = append(cuts, t)
	}
	return cleanCuts(cuts, length), nil
}

// saveClipCuts keeps the editor's cuts for runs in parts, or with reset,
// forgets them so cuts are detected again.
func (w *web) saveClipCuts(c *hyperchi.Context) error {
	ctx := c.Ctx()
	m, err := w.st.media(ctx, c.Param("id"))
	if errors.Is(err, errNotFound) || (err == nil && m.Kind != kindVideo) {
		return hyperchi.NotFound("No such clip.")
	}
	if err != nil {
		return err
	}
	cuts, err := formCuts(c, m.Seconds)
	if err != nil {
		return c.HX().Trigger("toast", hyperchi.H{"message": sentence(err.Error())}).NoContent()
	}
	msg := fmt.Sprintf("Saved %s. A run in parts splits this clip there.", plural2(len(cuts), "cut"))
	if c.Request().Form.Get("reset") != "" {
		cuts, msg = nil, "Cuts reset: they are found in the clip again."
	}
	if err := w.st.setClipCuts(ctx, m.ID, cuts); err != nil {
		return err
	}
	return c.HX().Trigger("clip-edited", hyperchi.H{"message": msg}).NoContent()
}

// plural2 is "1 cut" or "3 cuts".
func plural2(n int, word string) string { return strconv.Itoa(n) + " " + word + plural(n) }

// splitClip makes the clips between the cuts: 2 or 3, each at least
// clipShortest long, re-encoded so each starts on its first frame.
func (w *web) splitClip(ctx context.Context, m Media, cuts []float64) ([]Media, error) {
	if len(cuts) == 0 || len(cuts) >= clipMostPieces {
		return nil, failedf("Split makes 2 or 3 clips: keep 1 or 2 cuts")
	}
	bounds := append(append([]float64{0}, cuts...), m.Seconds)
	for i := 1; i < len(bounds); i++ {
		if bounds[i]-bounds[i-1] < clipShortest {
			return nil, failedf("Each piece must run at least %s", seconds(clipShortest))
		}
	}
	made := make([]Media, 0, len(bounds)-1)
	for i := 1; i < len(bounds); i++ {
		start, length := bounds[i-1], bounds[i]-bounds[i-1]
		label := fmt.Sprintf("%s · part %d of %d", m.Label, i, len(bounds)-1)
		piece, err := w.run.storeFile(ctx, originUpload, label, func(dst string) error {
			return w.frames.encode(ctx, "-ss", fmt.Sprintf("%.3f", start), "-i", w.lib.path(m), "-t", fmt.Sprintf("%.3f", length),
				"-map", "0:v:0", "-map", "0:a?", "-c:v", "libx264", "-preset", "veryfast", "-crf", "14", "-pix_fmt", "yuv420p",
				"-c:a", "aac", "-b:a", "192k", "-movflags", "+faststart", "-y", dst)
		})
		if err != nil {
			return made, fmt.Errorf("make part %d: %w", i, err)
		}
		made = append(made, piece)
	}
	return made, nil
}

// trimClip makes a new library clip of the stretch from start to end, at
// least clipShortest long, re-encoded so it starts on its first frame.
func (w *web) trimClip(ctx context.Context, m Media, start, end float64) (Media, error) {
	start, end = max(start, 0), min(end, m.Seconds)
	if end-start < clipShortest {
		return Media{}, failedf("Keep at least %s", seconds(clipShortest))
	}
	if start < 0.05 && end > m.Seconds-0.05 {
		return Media{}, failedf("Move the start or the end in to trim")
	}
	label := fmt.Sprintf("%s · %s to %s", m.Label, seconds(start), seconds(end))
	return w.run.storeFile(ctx, originUpload, label, func(dst string) error {
		return w.frames.encode(ctx, "-ss", fmt.Sprintf("%.3f", start), "-i", w.lib.path(m), "-t", fmt.Sprintf("%.3f", end-start),
			"-map", "0:v:0", "-map", "0:a?", "-c:v", "libx264", "-preset", "veryfast", "-crf", "14", "-pix_fmt", "yuv420p",
			"-c:a", "aac", "-b:a", "192k", "-movflags", "+faststart", "-y", dst)
	})
}

// trimClipHandler trims a clip into a new library clip and adds it to the
// page's lists.
func (w *web) trimClipHandler(c *hyperchi.Context) error {
	ctx := c.Ctx()
	m, err := w.st.media(ctx, c.Param("id"))
	if errors.Is(err, errNotFound) || (err == nil && (m.Kind != kindVideo || m.Status != mediaReady)) {
		return hyperchi.NotFound("No such clip.")
	}
	if err != nil {
		return err
	}
	if err := c.Request().ParseForm(); err != nil {
		return err
	}
	start, err1 := strconv.ParseFloat(c.Request().Form.Get("start"), 64)
	end, err2 := strconv.ParseFloat(c.Request().Form.Get("end"), 64)
	if err := errors.Join(err1, err2); err != nil {
		return c.HX().Trigger("toast", hyperchi.H{"message": "Set where the clip starts and ends."}).NoContent()
	}
	made, err := w.trimClip(ctx, m, start, end)
	var f failure
	if errors.As(err, &f) {
		return c.HX().Trigger("toast", hyperchi.H{"message": sentence(f.msg)}).NoContent()
	}
	if err != nil {
		return err
	}
	return c.HX().Trigger("clip-edited", hyperchi.H{"message": "Made " + made.Label + " in the library."}).
		Fragment("clips-split", hyperchi.H{"clips": []mediaView{viewMedia(made)}})
}

// splitClipHandler splits a clip at the editor's cuts into new library
// clips and adds them to whichever lists the page shows.
func (w *web) splitClipHandler(c *hyperchi.Context) error {
	ctx := c.Ctx()
	m, err := w.st.media(ctx, c.Param("id"))
	if errors.Is(err, errNotFound) || (err == nil && (m.Kind != kindVideo || m.Status != mediaReady)) {
		return hyperchi.NotFound("No such clip.")
	}
	if err != nil {
		return err
	}
	cuts, err := formCuts(c, m.Seconds)
	if err != nil {
		return c.HX().Trigger("toast", hyperchi.H{"message": sentence(err.Error())}).NoContent()
	}
	made, err := w.splitClip(ctx, m, cuts)
	var f failure
	if errors.As(err, &f) {
		return c.HX().Trigger("toast", hyperchi.H{"message": sentence(f.msg)}).NoContent()
	}
	if err != nil {
		return err
	}
	views := make([]mediaView, len(made))
	for i, p := range made {
		views[i] = viewMedia(p)
	}
	return c.HX().Trigger("clip-edited", hyperchi.H{
		"message": fmt.Sprintf("Made %s in the library from %s.", plural2(len(made), "clip"), m.Label),
	}).Fragment("clips-split", hyperchi.H{"clips": views})
}
