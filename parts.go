package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Long clips. An engine that takes less than the clip runs it in parts:
// segments cut from the clip with ffmpeg, split at the clip's own cuts where
// they fit, generated one after another from the same inputs and seed, then
// joined (a straight cut where the clip cuts, else a crossfade across a
// short overlap) with the clip's own sound laid over the whole, so the
// result runs as long as the clip and stays in step with it. A clip less
// than trimOver past the limit is trimmed instead: one generation, no join.
const (
	// longClipMax is the longest clip run in parts, as the page says it. A
	// "30 s" clip measures 30.04 s and the like, so anything under
	// longClipStop counts.
	longClipMax  = 30.0
	longClipStop = longClipMax + 1
	// partOverlap is the seconds neighbouring parts share. The join
	// crossfades across it, so a small difference between two generations
	// blends instead of cutting.
	partOverlap = 0.5
	// partMargin keeps each segment under the engine's limit after
	// re-encoding, which can add a frame.
	partMargin = 0.25
	// joinFPS is the frame rate parts are joined at; engines return 16 to
	// 30 fps.
	joinFPS = 24
	// trimOver is how far past the limit a clip is trimmed rather than run
	// in two billed parts for the sake of a fraction of a second.
	trimOver = 1.0
)

// part is one segment of a long clip and its generation.
type part struct {
	Start  float64 `json:"start"`
	Length float64 `json:"length"`
	// SourceID is the cut segment, RequestID the provider's id while it
	// works (so a restart collects it), and ResultID what the engine made.
	SourceID  string `json:"source,omitempty"`
	RequestID string `json:"request,omitempty"`
	ResultID  string `json:"result,omitempty"`
	// FrameID is a Style preset run's style frame for this part: the
	// previous part's result where this part begins, so the look carries
	// across the seam.
	FrameID string `json:"frame,omitempty"`
	// A long video's shot: its prompt, its frame count (17k+5 at 24 fps),
	// Cut when it starts a new shot rather than continuing the one before,
	// and TailID, the last moment of what came before that it continues
	// from.
	Prompt string `json:"prompt,omitempty"`
	Frames int    `json:"frames,omitempty"`
	Cut    bool   `json:"cut,omitempty"`
	TailID string `json:"tail,omitempty"`
	// Kept marks a shot copied from an earlier take of the same video
	// rather than made again; it cost nothing this time.
	Kept bool `json:"kept,omitempty"`
	// EndID is a library image the shot ends on, or empty.
	EndID string `json:"end,omitempty"`
}

// splits reports whether an engine runs a clip of this length in parts.
func splits(l limits, clip float64) bool {
	return l.MaxSeconds > partOverlap+partMargin && clip > l.MaxSeconds && clip < longClipStop
}

// planParts cuts a clip into the fewest equal segments the engine takes,
// each sharing partOverlap with the next, or one segment trimmed to the
// limit when the clip is less than trimOver past it. Nil when the clip runs
// whole. The studio mirrors it (partsFor) for its note and total; atCuts
// then moves the boundaries onto the clip's cuts when the run is made.
func planParts(l limits, clip float64) []part {
	if !splits(l, clip) {
		return nil
	}
	most := l.MaxSeconds - partMargin
	if clip-l.MaxSeconds < trimOver {
		return []part{{Start: 0, Length: most}}
	}
	n := int(math.Ceil((clip - partOverlap) / (most - partOverlap)))
	length := (clip + float64(n-1)*partOverlap) / float64(n)
	out := make([]part, n)
	for i := range out {
		out[i] = part{Start: float64(i) * (length - partOverlap), Length: length}
	}
	return out
}

// atCuts moves a plan's boundaries onto the clip's own cuts where one fits,
// keeping the number of parts. A part that starts on a cut shares nothing
// with the one before: it opens on a fresh shot, and the join is a straight
// cut where the clip cuts anyway. A boundary with no cut in reach keeps its
// overlap and crossfade. least is the shortest part worth making (the
// engine's shortest clip or output). Any plan that would break a limit is
// dropped for the even one.
func atCuts(l limits, least float64, plan []part, clip float64, cuts []float64) []part {
	n := len(plan)
	if n < 2 || len(cuts) == 0 {
		return plan
	}
	most := l.MaxSeconds - partMargin
	least = max(least, l.MinSeconds, 2)
	out := make([]part, 0, n)
	start := 0.0
	for k := 1; k < n; k++ {
		ideal := plan[k].Start + partOverlap/2 // the middle of the even plan's overlap
		rest := float64(n - k)
		best := -1.0
		for _, c := range cuts {
			fits := c-start >= least && c-start <= most && clip-c >= least && clip-c <= rest*most-(rest-1)*partOverlap
			if fits && (best < 0 || math.Abs(c-ideal) < math.Abs(best-ideal)) {
				best = c
			}
		}
		if best >= 0 {
			out = append(out, part{Start: start, Length: best - start})
			start = best
			continue
		}
		end := min(start+most, max(ideal+partOverlap/2, start+least))
		out = append(out, part{Start: start, Length: end - start})
		start = end - partOverlap
	}
	out = append(out, part{Start: start, Length: clip - start})
	for _, p := range out {
		if p.Length > most+1e-9 || p.Length < least-1e-9 {
			return plan
		}
	}
	return out
}

// shared is the seconds a part shares with the one before it: 0 where the
// boundary is one of the clip's cuts.
func shared(prev, cur part) float64 { return max(0, prev.Start+prev.Length-cur.Start) }

// seedOf is the seed a run renders with: its stored seed, pinned from the
// first take, or for an older run one from its key.
func seedOf(r Run) int64 {
	if r.Seed > 0 {
		return r.Seed
	}
	return runSeed(r.IdempotencyKey)
}

// runSeed is a seed from an idempotency key: different for each version of
// a batch, so versions differ.
func runSeed(key string) int64 {
	h := fnv.New64a()
	// Writing to a hash never fails.
	if _, err := h.Write([]byte(key)); err != nil {
		panic(err)
	}
	return int64(h.Sum64()>>2) + 1
}

// clipSeconds is how much of the clip one generation sees: a segment's
// length when the clip runs in parts.
func clipSeconds(l limits, t task) float64 {
	if t.Parts {
		if p := planParts(l, t.Source.Seconds); p != nil {
			return p[0].Length
		}
	}
	return t.Source.Seconds
}

// heldRequest is the request a provider is working on for a run: the run's
// own, or for a run in parts, the part in flight. Between parts nothing is
// held, so there is nothing to ask the provider to cancel.
func heldRequest(run Run) string {
	if len(run.Parts) == 0 {
		return run.RequestID
	}
	for _, p := range run.Parts {
		if p.ResultID == "" {
			return p.RequestID
		}
	}
	return ""
}

// partTracker reports one part's progress as the run's. The request id goes
// on the part and on the run, which cancel and recover read, and progress
// is the part's share of the whole.
type partTracker struct {
	tr   *runTracker
	i, n int
}

func (p *partTracker) label(s string) string {
	return fmt.Sprintf("Part %d of %d · %s", p.i+1, p.n, s)
}

func (p *partTracker) queued(requestID string, position *int) {
	p.tr.update(func(run *Run) {
		run.RequestID = requestID
		run.Parts[p.i].RequestID = requestID
		run.Status = runQueued
		detail := "Queued"
		if position != nil {
			detail = fmt.Sprintf("Queued, position %d", *position+1)
		}
		run.Detail = p.label(detail)
	})
}

func (p *partTracker) running(detail string, progress float64) {
	p.tr.running(p.label(detail), (float64(p.i)+min(max(progress, 0), 1))/float64(p.n)*0.95)
}

// attemptParts makes each part not yet made, collecting one a provider
// already holds, then joins them into the run's result.
func (r *runner) attemptParts(ctx context.Context, en engine, tr *runTracker) error {
	if r.frames == nil || !r.frames.available() {
		return errOffline{"Running a long clip in parts needs ffmpeg on the server."}
	}
	run := tr.snapshot()
	base, err := r.task(ctx, run)
	if err != nil {
		return err
	}
	if run.Mode == modeStyle {
		if base, err = r.prepareStyle(ctx, tr, base); err != nil {
			return err
		}
	}
	n := len(run.Parts)
	for i := range n {
		if tr.snapshot().Parts[i].ResultID != "" {
			continue
		}
		pt := &partTracker{tr: tr, i: i, n: n}
		out, err := r.generatePart(ctx, en, tr, pt, base)
		if err != nil {
			return err
		}
		pt.running("Saving the part", 1)
		m, err := r.keepAs(ctx, out, originPart, fmt.Sprintf("Part %d of %d", i+1, n))
		if err != nil {
			return err
		}
		tr.update(func(run *Run) { run.Parts[i].ResultID = m.ID })
	}
	tr.running(fmt.Sprintf("Joining %d parts", n), 0.97)
	m, err := r.joinParts(ctx, tr.snapshot())
	if err != nil {
		// The parts are made and billed; a join that fails will fail again,
		// so it is a verdict, not lost contact.
		return failedf("Joining the parts failed: %v", err)
	}
	r.finish(tr, &m, nil)
	r.done(tr.snapshot(), m)
	return nil
}

// generatePart runs one part, or collects it when a provider holds it.
func (r *runner) generatePart(ctx context.Context, en engine, tr *runTracker, pt *partTracker, base task) (outcome, error) {
	run := tr.snapshot()
	if id := run.Parts[pt.i].RequestID; id != "" {
		pt.running("Reconnecting to the provider", 0)
		return en.resume(ctx, id, pt)
	}
	seg, err := r.partSource(ctx, tr, pt, base.Source)
	if err != nil {
		return outcome{}, err
	}
	t := base
	t.Source = seg
	t.Key = fmt.Sprintf("%s-p%d", run.IdempotencyKey, pt.i+1)
	info := en.info()
	switch {
	case pt.i == 0:
	case run.Mode == modeStyle && run.Style != "":
		// The style frame becomes the previous part's look at this moment.
		frame, err := r.partFrame(ctx, tr, pt)
		if err != nil {
			return outcome{}, err
		}
		t.StyleImage = frame
		t.Refs = append([]Media{frame}, base.Refs[1:]...)
	case run.Mode != modeStyle && info.Tags && t.Prompt != "" && len(t.ofKind(kindImage)) < info.Limits.MaxRefs:
		// The previous part's last moment joins the references, so the
		// cast this part draws is the one the last part drew.
		frame, err := r.partFrame(ctx, tr, pt)
		if err != nil {
			return outcome{}, err
		}
		t.Refs = append(slices.Clone(base.Refs), frame)
		tag := refTags(t.Refs)[len(t.Refs)-1]
		t.Prompt += " " + tag + " is a frame from just before this shot with the same people: keep their faces, hair, " +
			"and clothes exactly as there."
	}
	return en.generate(ctx, t, pt)
}

// partSource is a part's segment of the clip, cut once and kept on the run.
func (r *runner) partSource(ctx context.Context, tr *runTracker, pt *partTracker, src Media) (Media, error) {
	p := tr.snapshot().Parts[pt.i]
	if p.SourceID != "" {
		return r.st.media(ctx, p.SourceID)
	}
	pt.running("Cutting the clip", 0)
	m, err := r.storeFile(ctx, originPart, fmt.Sprintf("Part %d of %d source", pt.i+1, pt.n), func(dst string) error {
		return r.frames.encode(ctx, "-ss", fmt.Sprintf("%.3f", p.Start), "-i", r.lib.path(src), "-t", fmt.Sprintf("%.3f", p.Length),
			"-map", "0:v:0", "-map", "0:a?", "-c:v", "libx264", "-preset", "veryfast", "-crf", "14", "-pix_fmt", "yuv420p",
			"-c:a", "aac", "-b:a", "192k", "-movflags", "+faststart", "-y", dst)
	})
	if err != nil {
		return Media{}, fmt.Errorf("cut part %d: %w", pt.i+1, err)
	}
	tr.update(func(run *Run) { run.Parts[pt.i].SourceID = m.ID })
	return m, nil
}

// partFrame is the previous part's result at the moment this part begins,
// or its last moment where the two share nothing: a Style preset run's style
// frame, and a later part's look reference elsewhere.
func (r *runner) partFrame(ctx context.Context, tr *runTracker, pt *partTracker) (Media, error) {
	run := tr.snapshot()
	if id := run.Parts[pt.i].FrameID; id != "" {
		return r.st.media(ctx, id)
	}
	prev, cur := run.Parts[pt.i-1], run.Parts[pt.i]
	made, err := r.st.media(ctx, prev.ResultID)
	if err != nil {
		return Media{}, fmt.Errorf("part %d result: %w", pt.i, err)
	}
	at := cur.Start - prev.Start
	if made.Seconds > 0 {
		at = min(at*made.Seconds/prev.Length, made.Seconds-0.1)
	}
	width := made.Width
	if width <= 0 {
		width = 1024
	}
	pt.running("Taking the look from the part before", 0)
	img, err := r.frames.still(ctx, r.lib.path(made), at, width)
	if err != nil {
		return Media{}, err
	}
	m, err := r.lib.store(bytes.NewReader(img), originPart, fmt.Sprintf("Part %d of %d look frame", pt.i+1, pt.n), 0)
	if err != nil {
		return Media{}, err
	}
	if err := r.st.addMedia(ctx, m); err != nil {
		return Media{}, errors.Join(err, r.lib.remove(m))
	}
	tr.update(func(run *Run) { run.Parts[pt.i].FrameID = m.ID })
	return m, nil
}

// joinParts joins a run's finished parts into its result, with the clip's
// sound over the whole.
func (r *runner) joinParts(ctx context.Context, run Run) (Media, error) {
	src, err := r.st.media(ctx, run.SourceID)
	if err != nil {
		return Media{}, fmt.Errorf("source video: %w", err)
	}
	args := []string{"-y"}
	lengths := make([]float64, len(run.Parts))
	w, h := 0, 0
	for i, p := range run.Parts {
		m, err := r.st.media(ctx, p.ResultID)
		if err != nil {
			return Media{}, fmt.Errorf("part %d result: %w", i+1, err)
		}
		if i == 0 {
			w, h = m.Width, m.Height
		}
		lengths[i] = m.Seconds
		args = append(args, "-i", r.lib.path(m))
	}
	if w <= 0 || h <= 0 {
		w, h = src.Width, src.Height
	}
	if w <= 0 || h <= 0 {
		return Media{}, errors.New("the parts' frame size is unknown")
	}
	graph, out := joinGraph(run.Parts, lengths, w/2*2, h/2*2)
	// A trimmed clip covers less than the source; the sound stops with it.
	last := run.Parts[len(run.Parts)-1]
	span := min(src.Seconds, last.Start+last.Length)
	args = append(args, "-i", r.lib.path(src), "-filter_complex", graph,
		"-map", "["+out+"]", "-map", fmt.Sprintf("%d:a?", len(run.Parts)), "-t", fmt.Sprintf("%.3f", span),
		"-c:v", "libx264", "-preset", "medium", "-crf", "16", "-pix_fmt", "yuv420p",
		"-c:a", "aac", "-b:a", "192k", "-movflags", "+faststart")
	return r.storeFile(ctx, originResult, modeLabels[run.Mode]+" result", func(dst string) error {
		return r.frames.encode(ctx, append(args, dst)...)
	})
}

// joinGraph is the ffmpeg filter that joins parts: each fitted to its
// segment's length (lengths are the results' own, 0 when unknown), frame
// rate, and size, then crossfaded across each overlap, or joined straight
// where the boundary is one of the clip's cuts. It returns the graph and
// its output label.
func joinGraph(parts []part, lengths []float64, w, h int) (string, string) {
	var b strings.Builder
	for i, p := range parts {
		speed := 1.0
		if lengths[i] > 0 {
			speed = p.Length / lengths[i]
		}
		fmt.Fprintf(&b, "[%d:v]setpts=(PTS-STARTPTS)*%.6f,fps=%d,scale=%d:%d:force_original_aspect_ratio=decrease,"+
			"pad=%d:%d:(ow-iw)/2:(oh-ih)/2,setsar=1,format=yuv420p,trim=duration=%.3f,setpts=PTS-STARTPTS[p%d];",
			i, speed, joinFPS, w, h, w, h, p.Length, i)
	}
	last, end := "p0", parts[0].Length
	for i := 1; i < len(parts); i++ {
		next := fmt.Sprintf("x%d", i)
		if o := shared(parts[i-1], parts[i]); o > 0.01 {
			fmt.Fprintf(&b, "[%s][p%d]xfade=transition=fade:duration=%.3f:offset=%.3f[%s];", last, i, o, end-o, next)
			end += parts[i].Length - o
		} else {
			fmt.Fprintf(&b, "[%s][p%d]concat=n=2:v=1:a=0[%s];", last, i, next)
			end += parts[i].Length
		}
		last = next
	}
	return strings.TrimSuffix(b.String(), ";"), last
}

// storeFile has ffmpeg write a file, then keeps it in the library.
func (r *runner) storeFile(ctx context.Context, origin, label string, write func(dst string) error) (Media, error) {
	dst := filepath.Join(r.frames.cache, "work-"+newID()+".mp4")
	defer func() {
		if err := os.Remove(dst); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Printf("utsushi: remove %s: %v", dst, err)
		}
	}()
	if err := write(dst); err != nil {
		return Media{}, err
	}
	f, err := os.Open(dst)
	if err != nil {
		return Media{}, err
	}
	m, err := r.lib.store(f, origin, label, 0)
	if cerr := f.Close(); cerr != nil && err == nil {
		err = cerr
	}
	if err != nil {
		return Media{}, err
	}
	if err := r.st.addMedia(ctx, m); err != nil {
		return Media{}, errors.Join(err, r.lib.remove(m))
	}
	return m, nil
}
