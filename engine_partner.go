package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"

	"github.com/regiellis/comfy-api-proxy-go/sdk"
)

// wan3Engine runs Wan 3.0 reference-to-video, a ComfyUI partner node
// (Wan3ReferenceToVideoApi), on the local ComfyUI through the proxy. The
// clip goes in as @Video1, reference images as @Image1 onward, reference
// videos as @Video2 onward, and audio as @Audio1 onward. The node calls the
// partner API with the Comfy API key carried in extra_data, so it bills
// that account's credits.
type wan3Engine struct {
	about engineInfo
	local *localRefs
	lib   *library
	key   func() string // the Comfy API key that pays for partner nodes
}

func newWan3Engine(local *localRefs, lib *library, key func() string) *wan3Engine {
	return &wan3Engine{
		about: engineInfo{
			ID: "wan3-r2v", Mode: modeMotion, Name: "Wan 3.0 Reference to Video", Via: "Partner node", Code: "W3",
			Billed: true, Tags: true,
			// The node's own price table (object_info price_badge), read
			// 2026-10-17.
			Rates: map[string]float64{"480P": 0.0715, "720P": 0.143, "1080P": 0.286},
			Blurb: "Recasts the clip with your references, keeping its motion, camera, and setting.",
			// The node takes 10 images, 5 videos, and 5 audio clips
			// (object_info, read 2026-10-17); the clip is one of the videos.
			// Wan's media rules cap reference video and reference audio at
			// 15 s in total each.
			Limits: limits{MinRefs: 1, MaxRefs: 10, MaxVideoRefs: 4, MaxAudioRefs: 5,
				RefVideoTotal: 15, RefAudioTotal: 15, MinSeconds: 2, MaxSeconds: 15, PromptMax: 2000,
				RefsFor: "Images of characters, props, or a setting; videos for motion or a scene; " +
					"audio for voice, music, or sound. Name each in the prompt by its tag.",
				Resolutions: []string{"480P", "720P", "1080P"}, DefaultResolution: "720P"},
		},
		local: local,
		lib:   lib,
		key:   key,
	}
}

// wan3Output is the SaveVideo node whose video is the result.
const wan3Output = "3"

func (e *wan3Engine) info() engineInfo { return e.about }

func (e *wan3Engine) ready(ctx context.Context) error {
	if e.key() == "" {
		return errOffline{"Set COMFY_API_KEY to pay for partner nodes."}
	}
	return e.local.ready(ctx)
}

// wan3Duration is the output length in whole seconds: the clip's, inside
// the node's 2 to 30 second range. The clip limit of 15 s keeps clip and
// output under the 30 s the node allows for both together.
func wan3Duration(clip float64) string {
	return strconv.Itoa(int(min(max(math.Round(clip), 2), 15)))
}

// wan3Prompt follows the Wan 3.0 prompt guide: the scene first, then each
// reference named by its role, audio direction, and the resolution last. A
// prompt that already tags references is the user's own and is kept as it
// is, apart from the resolution.
func wan3Prompt(user string, images, videos, audios int, resolution string) string {
	user = wan3Tags(user)
	list := func(prefix string, n, from int) string {
		tags := make([]string, n)
		for i := range tags {
			tags[i] = fmt.Sprintf("@%s%d", prefix, from+i)
		}
		return strings.Join(tags, ", ")
	}
	var b strings.Builder
	if user == "" {
		b.WriteString("Recast the performer in @Video1 as the character in " + list("Image", images, 1) +
			", keeping @Video1's motion, choreography, timing, camera movement, framing, and setting.")
	} else {
		b.WriteString(user)
	}
	if !strings.Contains(user, "@") {
		b.WriteString("\n\nCharacter appearance: " + list("Image", images, 1) + ". Motion and camera reference: @Video1.")
		if videos > 0 {
			b.WriteString("\nFurther motion and scene reference: " + list("Video", videos, 2) + ".")
		}
		if audios > 0 {
			b.WriteString("\nAudio: " + list("Audio", audios, 1) + ".")
		} else {
			b.WriteString("\nAudio: the original sound of @Video1, no dialogue.")
		}
	}
	b.WriteString("\n" + resolution)
	return b.String()
}

// wan3Inputs are the uploaded asset references for one run.
type wan3Inputs struct {
	video                  any
	images, videos, audios []any
}

// wan3Graph is the API-format workflow: the clip (1), the partner node (2),
// SaveVideo (3), and a loader per reference: images from 10, videos from
// 30, audio from 40. Dynamic inputs serialize as "parent.child", as the
// ComfyUI frontend writes them.
func wan3Graph(in wan3Inputs, prompt, resolution, duration string, seed int64) map[string]any {
	node := map[string]any{
		"model":                         "wan3.0-video",
		"model.prompt":                  prompt,
		"model.resolution":              resolution,
		"model.ratio":                   "adaptive",
		"model.duration":                duration,
		"model.audio":                   true,
		"model.prompt_extend":           true,
		"model.reference_videos.video1": []any{"1", 0},
		"seed":                          seed,
		"watermark":                     false,
	}
	g := map[string]any{
		"1": map[string]any{"class_type": "LoadVideo", "inputs": map[string]any{"file": in.video}},
		"2": map[string]any{"class_type": "Wan3ReferenceToVideoApi", "inputs": node},
		wan3Output: map[string]any{"class_type": "SaveVideo", "inputs": map[string]any{
			"video": []any{"2", 0}, "filename_prefix": "utsushi/wan3", "format": "auto", "codec": "auto"}},
	}
	for _, kind := range []struct {
		refs              []any
		base, from        int
		class, field, key string
	}{
		{in.images, 10, 1, "LoadImage", "image", "model.reference_images.image%d"},
		{in.videos, 30, 2, "LoadVideo", "file", "model.reference_videos.video%d"},
		{in.audios, 40, 1, "LoadAudio", "audio", "model.reference_audios.audio%d"},
	} {
		for i, ref := range kind.refs {
			id := strconv.Itoa(kind.base + i)
			g[id] = map[string]any{"class_type": kind.class, "inputs": map[string]any{kind.field: ref}}
			node[fmt.Sprintf(kind.key, kind.from+i)] = []any{id, 0}
		}
	}
	return g
}

func (e *wan3Engine) generate(ctx context.Context, t task, tr tracker) (outcome, error) {
	if err := e.ready(ctx); err != nil {
		return outcome{}, err
	}
	client := e.local.client
	tr.running("Sending the clip to ComfyUI", 0)
	video, err := client.AssetFromFile(e.lib.path(t.Source)).Reference(ctx)
	if err != nil {
		return outcome{}, fmt.Errorf("upload source: %w", err)
	}
	in := wan3Inputs{video: video}
	for i, m := range t.Refs {
		tr.running(fmt.Sprintf("Sending reference %d of %d to ComfyUI", i+1, len(t.Refs)), 0)
		ref, err := client.AssetFromFile(e.lib.path(m)).Reference(ctx)
		if err != nil {
			return outcome{}, fmt.Errorf("upload reference: %w", err)
		}
		switch m.Kind {
		case kindVideo:
			in.videos = append(in.videos, ref)
		case kindAudio:
			in.audios = append(in.audios, ref)
		default:
			in.images = append(in.images, ref)
		}
	}
	prompt := wan3Prompt(t.Prompt, len(in.images), len(in.videos), len(in.audios), t.Resolution)
	graph := wan3Graph(in, prompt, t.Resolution, wan3Duration(t.Source.Seconds), t.seed(1<<31))
	raw, err := json.Marshal(graph)
	if err != nil {
		return outcome{}, err
	}
	wf, err := sdk.WorkflowFromJSON(raw)
	if err != nil {
		return outcome{}, err
	}
	tr.running("Submitting to ComfyUI", 0)
	job, err := client.Submit(ctx, wf, sdk.SubmitOptions{IdempotencyKey: t.Key, PartnerAPIKey: e.key()})
	if err != nil {
		return outcome{}, apiFailure(err)
	}
	tr.queued(job.ID(), job.QueuePosition())
	return followJob(ctx, job, wan3Output, tr)
}

func (e *wan3Engine) resume(ctx context.Context, jobID string, tr tracker) (outcome, error) {
	job, err := e.local.client.GetJob(ctx, jobID)
	if err != nil {
		return outcome{}, apiFailure(err)
	}
	return followJob(ctx, job, wan3Output, tr)
}

func (e *wan3Engine) cancel(ctx context.Context, jobID string) error {
	return cancelJob(ctx, e.local.client, jobID)
}

var (
	shortTag = regexp.MustCompile(`@(\d+)\b`)
	videoTag = regexp.MustCompile(`@v(\d+)\b`)
	audioTag = regexp.MustCompile(`@a(\d+)\b`)
)

// wan3Tags turns the studio's reference tokens into Wan 3.0's, which number
// each kind on its own: @1 becomes @Image1, @clip @Video1, @v1 @Video2 (the
// clip is the first video), and @a1 @Audio1.
func wan3Tags(prompt string) string {
	prompt = strings.ReplaceAll(prompt, "@clip", "@Video1")
	prompt = videoTag.ReplaceAllStringFunc(prompt, func(m string) string {
		n, err := strconv.Atoi(m[2:])
		if err != nil {
			return m
		}
		return fmt.Sprintf("@Video%d", n+1)
	})
	prompt = audioTag.ReplaceAllString(prompt, "@Audio$1")
	return shortTag.ReplaceAllString(prompt, "@Image$1")
}
