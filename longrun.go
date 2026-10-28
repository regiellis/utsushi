package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// attemptShots renders a long video's shots in order, then joins them. Each
// shot continues from the last moment of what came before it (the start
// video for the first, if there is one) or cuts to a new shot. A shot a
// provider holds is collected, and a finished one is kept, so a restart or a
// Collect picks up where the run stopped.
func (r *runner) attemptShots(ctx context.Context, en engine, tr *runTracker) error {
	if r.frames == nil || !r.frames.available() {
		return errOffline{"A long video needs ffmpeg on the server to join its shots."}
	}
	run := tr.snapshot()
	cast, err := r.st.mediaByIDs(ctx, run.RefIDs)
	if err != nil {
		return err
	}
	n := len(run.Parts)
	for i := range n {
		p := tr.snapshot().Parts[i]
		if p.ResultID != "" {
			continue
		}
		pt := &partTracker{tr: tr, i: i, n: n}
		var out outcome
		if p.RequestID != "" {
			pt.running("Reconnecting to the provider", 0)
			out, err = en.resume(ctx, p.RequestID, pt)
		} else {
			t := task{Prompt: p.Prompt, Refs: cast, Key: fmt.Sprintf("%s-s%d", run.IdempotencyKey, i+1),
				Seed: seedOf(run), Frames: p.Frames, Width: run.Width, Height: run.Height,
				Turbo: run.Quality == "turbo", FrontViews: run.CastView == "front", RefSize: run.RefSize}
			if !p.Cut {
				if t.Source, err = r.shotTail(ctx, tr, pt); err != nil {
					return err
				}
			}
			if p.EndID != "" {
				if t.EndImage, err = r.st.media(ctx, p.EndID); err != nil {
					return fmt.Errorf("shot %d's end image: %w", i+1, err)
				}
			}
			out, err = en.generate(ctx, t, pt)
		}
		if err != nil {
			return err
		}
		pt.running("Saving the shot", 1)
		m, err := r.keepAs(ctx, out, originPart, fmt.Sprintf("Shot %d of %d", i+1, n))
		if err != nil {
			return err
		}
		tr.update(func(run *Run) { run.Parts[i].ResultID = m.ID })
	}
	tr.running(fmt.Sprintf("Joining %d shots", n), 0.97)
	m, err := r.joinShots(ctx, tr.snapshot())
	if err != nil {
		// The shots are made; a join that fails will fail again.
		return failedf("Joining the shots failed: %v", err)
	}
	r.finish(tr, &m, nil)
	r.done(tr.snapshot(), m)
	return nil
}

// shotTail is the last longTail frames and their sound of what a shot
// continues from, at the shot's frame size, cut once and kept on the run.
func (r *runner) shotTail(ctx context.Context, tr *runTracker, pt *partTracker) (Media, error) {
	run := tr.snapshot()
	if id := run.Parts[pt.i].TailID; id != "" {
		return r.st.media(ctx, id)
	}
	from := run.SourceID
	if pt.i > 0 {
		from = run.Parts[pt.i-1].ResultID
	}
	if from == "" {
		return Media{}, errors.New("this shot continues, but nothing comes before it")
	}
	prev, err := r.st.media(ctx, from)
	if err != nil {
		return Media{}, fmt.Errorf("what shot %d continues from: %w", pt.i+1, err)
	}
	pt.running("Taking the last moment of what came before", 0)
	secs := fmt.Sprintf("%.6f", float64(longTail)/longFPS)
	m, err := r.storeFile(ctx, originPart, fmt.Sprintf("Shot %d of %d lead-in", pt.i+1, pt.n), func(dst string) error {
		// Reversing, trimming, and reversing back takes exactly the last
		// frames, where a seek near the end can fall short.
		return r.frames.encode(ctx, "-sseof", "-3", "-i", r.lib.path(prev),
			"-map", "0:v:0", "-map", "0:a?",
			"-vf", fmt.Sprintf("fps=%d,scale=%d:%d:force_original_aspect_ratio=increase,crop=%d:%d,setsar=1,reverse,trim=end_frame=%d,reverse,setpts=PTS-STARTPTS",
				longFPS, run.Width, run.Height, run.Width, run.Height, longTail),
			"-af", "areverse,atrim=end="+secs+",areverse,asetpts=PTS-STARTPTS",
			"-c:v", "libx264", "-preset", "veryfast", "-crf", "14", "-pix_fmt", "yuv420p",
			"-c:a", "aac", "-b:a", "192k", "-movflags", "+faststart", "-y", dst)
	})
	if err != nil {
		return Media{}, fmt.Errorf("take shot %d's lead-in: %w", pt.i+1, err)
	}
	tr.update(func(run *Run) { run.Parts[pt.i].TailID = m.ID })
	return m, nil
}

// joinInput is one piece of a long video's join: a file, the frames to skip
// at its start (a continuing shot's replayed lead-in), its length in
// seconds, and whether it has sound.
type joinInput struct {
	path    string
	skip    int
	seconds float64
	sound   bool
}

// joinShots joins the start video, if any, and the shots into the run's
// result: each fitted to the run's size at longFPS, a continuing shot
// without the lead-in it replays, and each piece's own sound, or silence
// where it has none, so sound and picture stay together.
func (r *runner) joinShots(ctx context.Context, run Run) (Media, error) {
	var ins []joinInput
	add := func(id string, skip int) error {
		m, err := r.st.media(ctx, id)
		if err != nil {
			return err
		}
		p := r.lib.path(m)
		ins = append(ins, joinInput{path: p, skip: skip, seconds: m.Seconds, sound: r.frames.hasSound(ctx, p)})
		return nil
	}
	if run.SourceID != "" {
		if err := add(run.SourceID, 0); err != nil {
			return Media{}, fmt.Errorf("start video: %w", err)
		}
	}
	for i, p := range run.Parts {
		skip := longTail
		if p.Cut {
			skip = 0
		}
		if err := add(p.ResultID, skip); err != nil {
			return Media{}, fmt.Errorf("shot %d: %w", i+1, err)
		}
	}
	args := []string{"-y"}
	for _, in := range ins {
		args = append(args, "-i", in.path)
	}
	graph := joinShotsGraph(ins, run.Width, run.Height)
	if run.SoundtrackID != "" {
		track, err := r.st.media(ctx, run.SoundtrackID)
		if err != nil {
			return Media{}, fmt.Errorf("soundtrack: %w", err)
		}
		args = append(args, "-i", r.lib.path(track))
		graph = withSoundtrack(graph) + soundtrackGraph(len(ins), joinedSeconds(ins), run.SoundtrackMode)
	}
	args = append(args, "-filter_complex", graph, "-map", "[v]", "-map", "[a]",
		"-c:v", "libx264", "-preset", "medium", "-crf", "16", "-pix_fmt", "yuv420p",
		"-c:a", "aac", "-b:a", "192k", "-movflags", "+faststart")
	return r.storeFile(ctx, originResult, modeLabels[run.Mode]+" result", func(dst string) error {
		return r.frames.encode(ctx, append(args, dst)...)
	})
}

// joinedSeconds is how long the joined video runs.
func joinedSeconds(ins []joinInput) float64 {
	total := 0.0
	for _, in := range ins {
		total += max(in.seconds-float64(in.skip)/longFPS, 0.1)
	}
	return total
}

// soundtrackGraph lays the input at index track over the joined video: as
// its sound ("replace"), or under the shots' own at a third ("mix"), padded
// or cut to the video's length. It renames the joined sound and takes [a].
func soundtrackGraph(track int, total float64, mode string) string {
	shape := fmt.Sprintf("[%d:a]aformat=sample_rates=48000:channel_layouts=stereo,apad=whole_dur=%.6f,atrim=end=%.6f", track, total, total)
	if mode == "mix" {
		return fmt.Sprintf(";%s,volume=0.33[track];[shots][track]amix=inputs=2:duration=first:normalize=0[a]", shape)
	}
	return fmt.Sprintf(";%s[a];[shots]anullsink", shape)
}

// joinShotsGraph is the ffmpeg filter for joinShots. With a soundtrack, its
// sound is [shots], for soundtrackGraph to lay the track over.
func joinShotsGraph(ins []joinInput, w, h int) string {
	var b strings.Builder
	var pairs strings.Builder
	for i, in := range ins {
		cut := float64(in.skip) / longFPS
		length := max(in.seconds-cut, 0.1)
		fmt.Fprintf(&b, "[%d:v]fps=%d,trim=start_frame=%d,setpts=PTS-STARTPTS,scale=%d:%d:force_original_aspect_ratio=decrease,"+
			"pad=%d:%d:(ow-iw)/2:(oh-ih)/2,setsar=1,format=yuv420p[v%d];", i, longFPS, in.skip, w, h, w, h, i)
		if in.sound {
			fmt.Fprintf(&b, "[%d:a]aformat=sample_rates=48000:channel_layouts=stereo,atrim=start=%.6f,asetpts=PTS-STARTPTS,"+
				"apad=whole_dur=%.6f,atrim=end=%.6f[a%d];", i, cut, length, length, i)
		} else {
			fmt.Fprintf(&b, "anullsrc=r=48000:cl=stereo,atrim=end=%.6f[a%d];", length, i)
		}
		fmt.Fprintf(&pairs, "[v%d][a%d]", i, i)
	}
	fmt.Fprintf(&b, "%sconcat=n=%d:v=1:a=1[v][a]", pairs.String(), len(ins))
	return b.String()
}

// withSoundtrack renames the joined sound so a soundtrack can take [a].
func withSoundtrack(graph string) string {
	return strings.TrimSuffix(graph, "[v][a]") + "[v][shots]"
}
