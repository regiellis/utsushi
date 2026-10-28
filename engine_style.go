package main

import (
	"context"
	"fmt"
)

// restyler is an engine in Style mode. It runs another engine's model with
// the style frame as the first reference: the runner restyles the clip's
// first frame locally (or uses the visitor's own style image) before the
// provider is called, and puts it at t.Refs[0]. Characters follow it.
type restyler struct {
	base     engine
	id       string
	maxChars int
	tag      func(int) string // how the base engine's prompt names reference n, or nil
}

// newRestyler wraps base for Style mode. maxChars is how many character
// references fit beside the style frame.
func newRestyler(base engine, id string, maxChars int, tag func(int) string) *restyler {
	return &restyler{base: base, id: id, maxChars: maxChars, tag: tag}
}

// info is the base engine's, read on each call (where a template engine runs
// can change with Settings), shaped for Style mode.
func (r *restyler) info() engineInfo {
	in := r.base.info()
	id, maxChars := r.id, r.maxChars
	in.ID, in.Mode, in.Tags = id, modeStyle, false
	in.Blurb = fmt.Sprintf("Restyles the whole clip with %s, matching the style frame. Motion, camera, and framing stay.", in.Name)
	in.Limits.MinRefs, in.Limits.MaxRefs = 0, maxChars
	// The style frame leads the references, so tag numbering for other
	// kinds is left to the base engine's own mode.
	in.Limits.MaxVideoRefs, in.Limits.MaxAudioRefs = 0, 0
	in.Limits.RefsFor = "Characters to put in the restyled clip. The style frame takes one more slot."
	if maxChars == 0 {
		in.Limits.RefsFor = "The style frame takes this model's only reference slot."
	}
	if wf, ok := r.base.(*workflowEngine); ok && wf.inCloud() {
		in.Blurb += " Presets also create a reference frame on Comfy Cloud; both steps use billed GPU time. Upload a style image to skip frame generation."
	}
	in.Limits.PromptRequired = false
	return in
}

func (r *restyler) ready(ctx context.Context) error             { return r.base.ready(ctx) }
func (r *restyler) cancel(ctx context.Context, id string) error { return r.base.cancel(ctx, id) }

func (r *restyler) resume(ctx context.Context, id string, tr tracker) (outcome, error) {
	return r.base.resume(ctx, id, tr)
}

func (r *restyler) generate(ctx context.Context, t task, tr tracker) (outcome, error) {
	if len(t.Refs) == 0 || t.Refs[0].ID != t.StyleImage.ID || t.StyleImage.ID == "" {
		return outcome{}, failedf("The style frame is missing")
	}
	t.Prompt = stylePrompt(t.Look, t.Prompt, len(t.Refs)-1, r.tag)
	return r.base.generate(ctx, t, tr)
}

// styleEngines are the Style mode engines: every billed model that takes a
// clip and a style image. Nothing local restyles a video yet; Wan Animate 2
// recasts a performer and keeps the look.
func styleEngines(genjutsu, wan3, wanEdit, aleph, seedanceEdit, ltx engine) []engine {
	imageTag := func(n int) string { return fmt.Sprintf("@Image%d", n) }
	seedanceTag := func(n int) string { return fmt.Sprintf("@Image %d", n) }
	return []engine{
		newRestyler(genjutsu, "genjutsu-style", 7, nil),
		newRestyler(wan3, "wan3-style", 9, imageTag),
		newRestyler(seedanceEdit, "seedance25-style", 8, seedanceTag),
		newRestyler(wanEdit, "wan27-style", 3, nil),
		newRestyler(aleph, "aleph2-style", 0, nil),
		newRestyler(ltx, "ltx23-style", 0, nil),
	}
}
