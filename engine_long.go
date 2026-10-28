package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/regiellis/comfy-api-proxy-go/sdk"
)

// MiniMax H3's frame grid and trained range: a shot is 17k+5 frames at 24
// fps, trained from 124 to 362 (about 5 to 15 s).
const (
	longFPS       = 24
	longMinFrames = 124
	longMaxFrames = 362
	// longTail is how many frames of what came before a continuing shot is
	// anchored on: the guide node keeps clips of 5, 22, 39... frames, and 22
	// (0.92 s) carries the motion through. The shot replays them first, so
	// the join drops them.
	longTail = 22
)

// longFrames is a shot's frame count for seconds of new footage: on the
// 17k+5 grid, inside the trained range, with the replayed tail on top when
// it continues from what came before.
func longFrames(seconds float64, continues bool) int {
	n := int(seconds*longFPS + 0.5)
	if continues {
		n += longTail
	}
	n = max(n, longMinFrames)
	if r := (n - 5) % 17; r != 0 {
		n += 17 - r
	}
	for n > longMaxFrames {
		n -= 17
	}
	return n
}

// longEngine renders a long video's shots with MiniMax H3's open
// reference-to-video model: ComfyUI's "MiniMax H3 Reference to Video"
// template (workflows/minimax-h3-r2v.api.json), on your ComfyUI or on Comfy
// Cloud as Settings, Engines, "Open models run on" says. The cast are its
// reference images. A shot that continues has the last 22 frames and sound
// of what came before anchored at frame 0 (MiniMaxH3AddGuide), so it picks up
// in motion instead of cutting. Checked on the remote ComfyUI 2026-10-18: the
// step across that join measured like any other frame step.
type longEngine struct {
	path  string
	local *localRefs
	lib   *library
	cloud *cloudLink
	where func() string
	// frames fits end images to the shot's size; its cache also holds
	// character sheets cut to their first figure.
	frames *frames
}

func newLongEngine(local *localRefs, lib *library, cloud *cloudLink, where func() string, fr *frames) *longEngine {
	return &longEngine{path: "workflows/minimax-h3-r2v.api.json", local: local, lib: lib, cloud: cloud, where: where, frames: fr}
}

func (e *longEngine) inCloud() bool { return e.where != nil && e.where() == onCloud }

func (e *longEngine) info() engineInfo {
	in := engineInfo{
		ID: "minimax-h3-long", Mode: modeLong, Name: "MiniMax H3", Via: "Your ComfyUI", Code: "H3", Tags: true,
		Rates: map[string]float64{"": 0},
		Blurb: "MiniMax H3's open model makes each shot from its prompt and your cast, with its own sound, " +
			"and continues one shot into the next.",
		Limits: limits{MaxRefs: 9, PromptMax: 4000,
			RefsFor: "The cast, props, or setting every shot draws on. Name each by its tag."},
	}
	if e.inCloud() {
		in.Via, in.Billed, in.Rates = "Comfy Cloud", true, nil
	}
	return in
}

func (e *longEngine) client() (*sdk.Client, error) {
	if !e.inCloud() {
		return e.local.client, nil
	}
	c := e.cloud.cloud()
	if c == nil {
		return nil, errOffline{"Add a Comfy API key in Settings to run open models on Comfy Cloud."}
	}
	return c, nil
}

func (e *longEngine) load() (*sdk.Workflow, error) {
	wf, err := sdk.WorkflowFromFile(e.path)
	if err != nil {
		return nil, errOffline{"The workflow export " + e.path + " does not load: " + err.Error()}
	}
	return wf, nil
}

func (e *longEngine) ready(ctx context.Context) error {
	wf, err := e.load()
	if err != nil {
		return err
	}
	if e.inCloud() {
		_, err := e.client()
		return err
	}
	if err := e.local.ready(ctx); err != nil {
		return err
	}
	if e.local.models != nil {
		return e.local.models.check(ctx, wf)
	}
	return nil
}

// longGraph is the template set up for one shot: its size, length, prompt,
// seed, and turbo choice, a loader per cast image, the tail anchored at
// frame 0 when the shot continues, and the end image at the last frame when
// it ends on one. refs, tail, and end are what the SDK uploads (assets or
// their references), nil where there is none.
func longGraph(wf *sdk.Workflow, t task, prompt string, refs []any, tail, end any) {
	// The template sizes by aspect and seconds; a shot sets both itself.
	for _, id := range []string{"115", "131", "132", "137", "139"} {
		wf.RemoveNode(id)
	}
	wf.SetInput("136", "width", t.Width)
	wf.SetInput("136", "height", t.Height)
	wf.SetInput("136", "length", t.Frames)
	wf.SetInput("138", "value", prompt)
	wf.SetInput("129", "noise_seed", t.seed(1<<53))
	wf.SetInput("146", "value", t.Turbo)
	if t.RefSize != "" {
		wf.SetInput("136", "ref_image_size", t.RefSize)
	}
	wf.SetInput("92", "filename_prefix", "video/utsushi_long")
	for i, ref := range refs {
		id := fmt.Sprintf("utsushi_ref_%d", i)
		wf.Graph[id] = map[string]any{"class_type": "LoadImage", "inputs": map[string]any{"image": ref}}
		wf.SetInput("136", fmt.Sprintf("ref_images.ref_image_%d", i), []any{id, 0})
	}
	// The guides chain: each takes the conditioning before it, and the
	// guider reads the last.
	cond := []any{"136", 0}
	if tail != nil {
		wf.Graph["utsushi_tail"] = map[string]any{"class_type": "LoadVideo", "inputs": map[string]any{"file": tail}}
		wf.Graph["utsushi_tail_parts"] = map[string]any{"class_type": "GetVideoComponents",
			"inputs": map[string]any{"video": []any{"utsushi_tail", 0}}}
		wf.Graph["utsushi_guide"] = map[string]any{"class_type": "MiniMaxH3AddGuide", "inputs": map[string]any{
			"positive": cond, "latent": []any{"136", 1}, "frame_idx": 0,
			"vae": []any{"119", 0}, "audio_vae": []any{"120", 0},
			"image": []any{"utsushi_tail_parts", 0}, "audio": []any{"utsushi_tail_parts", 1}}}
		cond = []any{"utsushi_guide", 0}
	}
	if end != nil {
		// A negative index counts from the end: -1 is the last frame.
		wf.Graph["utsushi_end"] = map[string]any{"class_type": "LoadImage", "inputs": map[string]any{"image": end}}
		wf.Graph["utsushi_end_guide"] = map[string]any{"class_type": "MiniMaxH3AddGuide", "inputs": map[string]any{
			"positive": cond, "latent": []any{"136", 1}, "frame_idx": -1,
			"vae": []any{"119", 0}, "image": []any{"utsushi_end", 0}}}
		cond = []any{"utsushi_end_guide", 0}
	}
	wf.SetInput("126", "conditioning", cond)
}

// endFrame is a shot's end image cropped and scaled to fill the shot's
// frame, made once into the cache: the guide anchors it frame for frame.
func (e *longEngine) endFrame(ctx context.Context, m Media, w, h int) (string, error) {
	out := filepath.Join(e.frames.cache, fmt.Sprintf("end-%s-%dx%d.png", m.ID, w, h))
	if _, err := os.Stat(out); err == nil {
		return out, nil
	}
	err := e.frames.encode(ctx, "-i", e.lib.path(m), "-frames:v", "1",
		"-vf", fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=increase,crop=%d:%d", w, h, w, h), "-y", out)
	return out, err
}

// longTags writes the studio's tags the way the model's prompts name
// references: <Picture 1>.
func longTags(prompt string) string {
	return tagStyle("<Picture %d>", "<Video %d>", "<Audio %d>")(strings.TrimSpace(prompt))
}

func (e *longEngine) generate(ctx context.Context, t task, tr tracker) (outcome, error) {
	wf, err := e.load()
	if err != nil {
		return outcome{}, err
	}
	client, err := e.client()
	if err != nil {
		return outcome{}, err
	}
	images := t.ofKind(kindImage)
	refs := make([]any, len(images))
	for i, m := range images {
		tr.running(fmt.Sprintf("Sending cast %d of %d", i+1, len(images)), 0)
		path := e.lib.path(m)
		if t.FrontViews {
			if path, err = frontView(e.lib, e.frames.cache, m); err != nil {
				return outcome{}, fmt.Errorf("cut cast %d to its front view: %w", i+1, err)
			}
		}
		if refs[i], err = client.AssetFromFile(path).Reference(ctx); err != nil {
			return outcome{}, fmt.Errorf("upload cast %d: %w", i+1, err)
		}
	}
	var tail any
	if t.Source.ID != "" {
		tr.running("Sending the moment it continues from", 0)
		if tail, err = client.AssetFromFile(e.lib.path(t.Source)).Reference(ctx); err != nil {
			return outcome{}, fmt.Errorf("upload the tail: %w", err)
		}
	}
	var end any
	if t.EndImage.ID != "" {
		tr.running("Sending the image it ends on", 0)
		path, err := e.endFrame(ctx, t.EndImage, t.Width, t.Height)
		if err != nil {
			return outcome{}, fmt.Errorf("fit the end image: %w", err)
		}
		if end, err = client.AssetFromFile(path).Reference(ctx); err != nil {
			return outcome{}, fmt.Errorf("upload the end image: %w", err)
		}
	}
	longGraph(wf, t, longTags(t.Prompt), refs, tail, end)
	if !e.inCloud() {
		if err := e.local.fit(ctx, wf); err != nil {
			return outcome{}, err
		}
	}
	tr.running("Submitting to ComfyUI", 0)
	job, err := client.Submit(ctx, wf, sdk.SubmitOptions{IdempotencyKey: t.Key})
	if err != nil {
		return outcome{}, apiFailure(err)
	}
	tr.queued(job.ID(), job.QueuePosition())
	return followJob(ctx, job, "92", tr)
}

func (e *longEngine) resume(ctx context.Context, jobID string, tr tracker) (outcome, error) {
	client, err := e.client()
	if err != nil {
		return outcome{}, err
	}
	job, err := client.GetJob(ctx, jobID)
	if err != nil {
		return outcome{}, apiFailure(err)
	}
	return followJob(ctx, job, "92", tr)
}

func (e *longEngine) cancel(ctx context.Context, jobID string) error {
	client, err := e.client()
	if err != nil {
		return err
	}
	return cancelJob(ctx, client, jobID)
}
