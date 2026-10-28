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

// cloudVideoEngine runs a partner video node on Comfy Cloud, which bills
// it to the Comfy API key: Seedance 2.5 and MiniMax H3. The clip is always
// the first reference video; picked images, videos, and audio follow. Each
// graph is the node plus a loader per input and SaveVideo, following the
// ComfyUI templates of the same name (comfyui_workflow_templates_json
// 0.1.112, read 2026-10-17). Node inputs and prices are from Comfy Cloud's
// object_info, read the same day.
type cloudVideoEngine struct {
	about engineInfo
	cloud *cloudLink
	lib   *library
	// node is the partner node's class and its inputs for a task, without
	// the reference links, which generate adds.
	node func(t task, prompt string, seed int64) (class string, inputs map[string]any)
	// keys name the node's reference inputs: images, videos, audio, each
	// with the number of its first slot.
	images, videos, audios string
	// tags turns the studio's @1, @v1, @a1, and @clip into the model's own
	// reference names.
	tags func(string) string
}

// cloudVideoOutput is the SaveVideo node.
const cloudVideoOutput = "2"

func (e *cloudVideoEngine) info() engineInfo { return e.about }

func (e *cloudVideoEngine) ready(context.Context) error {
	if e.cloud.cloud() == nil {
		return errOffline{"Add a Comfy API key in Settings to use Comfy Cloud."}
	}
	return nil
}

func (e *cloudVideoEngine) generate(ctx context.Context, t task, tr tracker) (outcome, error) {
	client := e.cloud.cloud()
	if client == nil {
		return outcome{}, errOffline{"Add a Comfy API key in Settings to use Comfy Cloud."}
	}
	tr.running("Sending the clip to Comfy Cloud", 0)
	clip, err := client.AssetFromFile(e.lib.path(t.Source)).Reference(ctx)
	if err != nil {
		return outcome{}, fmt.Errorf("upload source: %w", err)
	}
	refs := make([]any, len(t.Refs))
	for i, m := range t.Refs {
		tr.running(fmt.Sprintf("Sending reference %d of %d to Comfy Cloud", i+1, len(t.Refs)), 0)
		if refs[i], err = client.AssetFromFile(e.lib.path(m)).Reference(ctx); err != nil {
			return outcome{}, fmt.Errorf("upload reference %d: %w", i+1, err)
		}
	}
	graph := e.graph(t, clip, refs, t.seed(1<<31))
	raw, err := json.Marshal(graph)
	if err != nil {
		return outcome{}, err
	}
	wf, err := sdk.WorkflowFromJSON(raw)
	if err != nil {
		return outcome{}, err
	}
	tr.running("Submitting to Comfy Cloud", 0)
	job, err := client.Submit(ctx, wf, sdk.SubmitOptions{IdempotencyKey: t.Key, PartnerAPIKey: e.cloud.apiKey()})
	if err != nil {
		return outcome{}, apiFailure(err)
	}
	tr.queued(job.ID(), job.QueuePosition())
	return followJob(ctx, job, cloudVideoOutput, tr)
}

// graph wires the clip and references into the node, in pick order within
// each kind, the clip first among the videos.
func (e *cloudVideoEngine) graph(t task, clip any, refs []any, seed int64) map[string]any {
	class, inputs := e.node(t, e.prompt(t), seed)
	g := map[string]any{
		"1":              map[string]any{"class_type": class, "inputs": inputs},
		cloudVideoOutput: node("SaveVideo", "video", link("1"), "filename_prefix", "utsushi/"+e.about.ID, "format", "auto", "codec", "auto"),
		"10":             node("LoadVideo", "file", clip),
	}
	inputs[fmt.Sprintf(e.videos, 1)] = link("10")
	n := map[string]int{kindVideo: 1}
	for i, m := range t.Refs {
		id := strconv.Itoa(20 + i)
		n[m.Kind]++
		switch m.Kind {
		case kindVideo:
			g[id] = node("LoadVideo", "file", refs[i])
			inputs[fmt.Sprintf(e.videos, n[m.Kind])] = link(id)
		case kindAudio:
			g[id] = node("LoadAudio", "audio", refs[i])
			inputs[fmt.Sprintf(e.audios, n[m.Kind])] = link(id)
		default:
			g[id] = node("LoadImage", "image", refs[i])
			inputs[fmt.Sprintf(e.images, n[m.Kind])] = link(id)
		}
	}
	return g
}

// prompt is the visitor's prompt in the model's reference names, or, with
// none, a recast prompt naming the clip and each reference.
func (e *cloudVideoEngine) prompt(t task) string {
	if p := strings.TrimSpace(t.Prompt); p != "" {
		return e.tags(p)
	}
	tags := refTags(t.Refs)
	var people []string
	for i, m := range t.Refs {
		if m.Kind == kindImage {
			people = append(people, tags[i])
		}
	}
	return e.tags("Recast the people in @clip as " + strings.Join(people, ", ") +
		", keeping @clip's motion, timing, camera movement, framing, and setting exactly.")
}

func (e *cloudVideoEngine) resume(ctx context.Context, jobID string, tr tracker) (outcome, error) {
	client := e.cloud.cloud()
	if client == nil {
		return outcome{}, errOffline{"Add a Comfy API key in Settings to collect this Comfy Cloud run."}
	}
	job, err := client.GetJob(ctx, jobID)
	if err != nil {
		return outcome{}, apiFailure(err)
	}
	return followJob(ctx, job, cloudVideoOutput, tr)
}

func (e *cloudVideoEngine) cancel(ctx context.Context, jobID string) error {
	client := e.cloud.cloud()
	if client == nil {
		return errOffline{"Add a Comfy API key in Settings to reach Comfy Cloud."}
	}
	return cancelJob(ctx, client, jobID)
}

// outputSeconds is the output length the node is asked for: the clip's,
// inside the node's range.
func outputSeconds(clip float64, lo, hi int) int {
	return min(max(int(math.Round(clip)), lo), hi)
}

var (
	studioImage = regexp.MustCompile(`@(\d+)\b`)
	studioVideo = regexp.MustCompile(`@v(\d+)\b`)
	studioAudio = regexp.MustCompile(`@a(\d+)\b`)
)

// tagStyle maps the studio's reference tokens to a model's: @clip is the
// first video, @v1 the second, and images and audio keep their numbers.
func tagStyle(image, video, audio string) func(string) string {
	return func(p string) string {
		p = strings.ReplaceAll(p, "@clip", fmt.Sprintf(video, 1))
		p = studioVideo.ReplaceAllStringFunc(p, func(m string) string {
			n, err := strconv.Atoi(m[2:])
			if err != nil {
				return m
			}
			return fmt.Sprintf(video, n+1)
		})
		p = studioAudio.ReplaceAllStringFunc(p, func(m string) string {
			n, err := strconv.Atoi(m[2:])
			if err != nil {
				return m
			}
			return fmt.Sprintf(audio, n)
		})
		return studioImage.ReplaceAllStringFunc(p, func(m string) string {
			n, err := strconv.Atoi(m[1:])
			if err != nil {
				return m
			}
			return fmt.Sprintf(image, n)
		})
	}
}

// Seedance 2.5 bills by output frames at the resolution's token rate, and
// a reference video is billed by its length too (price_badge of
// ByteDance2ReferenceNodeV2). Per second, at the 16:9 and adaptive rows:
// tokens per frame times 24 frames, per thousand, times the price with a
// reference video.
var seedanceRates = map[string]float64{
	"480p":  400.3125 * 24.0 / 1000 * 0.009152,
	"720p":  900.0 * 24 / 1000 * 0.009152,
	"1080p": 2025.0 * 24 / 1000 * 0.01001,
}

// seedanceLimits are Seedance 2.0 and 2.5's reference caps (9 images, 3
// videos, 3 audio clips; the clip is one of the videos) and its 4 to 30
// second output.
func seedanceLimits(mode, refsFor string, prompt bool) limits {
	return limits{
		MinRefs: map[string]int{"motion": 1}[mode], MaxRefs: 9, MaxVideoRefs: 2, MaxAudioRefs: 3,
		MinSeconds: 2, MaxSeconds: 30, PromptRequired: prompt, PromptMax: 5000, RefsFor: refsFor,
		Resolutions: []string{"480p", "720p", "1080p"}, DefaultResolution: "720p",
	}
}

func seedanceNode(taskType string) func(t task, prompt string, seed int64) (string, map[string]any) {
	return func(t task, prompt string, seed int64) (string, map[string]any) {
		return "ByteDance2ReferenceNodeV2", map[string]any{
			"model": "Seedance 2.5", "model.prompt": prompt, "model.resolution": t.Resolution,
			"model.ratio": "adaptive", "model.duration": outputSeconds(t.Source.Seconds, 4, 30),
			"model.generate_audio": true, "model.task_type": taskType, "model.output_format": "mp4",
			"model.auto_downscale": true, "model.auto_upscale": false, "seed": seed, "watermark": false,
		}
	}
}

func newSeedanceEngines(cloud *cloudLink, lib *library) (motion, edit *cloudVideoEngine) {
	base := func(id, mode, name, code, blurb string, l limits, taskType string) *cloudVideoEngine {
		return &cloudVideoEngine{
			about: engineInfo{
				ID: id, Mode: mode, Name: name, Via: "Comfy Cloud", Code: code, Billed: true, Tags: true,
				Rates: seedanceRates, InputRates: seedanceRates, MinOutput: 4, Blurb: blurb, Limits: l,
			},
			cloud: cloud, lib: lib, node: seedanceNode(taskType),
			images: "model.reference_images.image_%d", videos: "model.reference_videos.video_%d",
			audios: "model.reference_audios.audio_%d",
			// The Seedance templates name references as "@Image 1".
			tags: tagStyle("@Image %d", "@Video %d", "@Audio %d"),
		}
	}
	motion = base("seedance25", modeMotion, "Seedance 2.5", "S2",
		"Recasts the clip with your references, keeping its motion, camera, and timing. Takes images, videos, and audio.",
		seedanceLimits("motion", "Images of the cast, props, or setting; videos for motion; audio for voice or music. Name each in the prompt by its tag.", false),
		"reference")
	edit = base("seedance25-edit", modeSwap, "Seedance 2.5 Edit", "SE",
		"Edits the clip from your prompt: replace, remove, or restyle something and keep the rest. Optional references.",
		seedanceLimits("swap", "What the edit brings in: a person, an object, or a look.", true),
		"edit")
	return motion, edit
}

// MiniMax H3 bills per second of output by resolution, plus about two
// seconds' worth per reference video, up to the video's own length capped
// at 15 s (price_badge of MinimaxHailuo03ReferenceNode).
var minimaxH3Rates = map[string]float64{"768P": 0.1287, "2K": 0.1859}

// MiniMax H3 Max bills 1.6 units a second at 0.0715 USD, plus tokens for
// references: a reference video counts about 7 frames a second at 1008
// tokens each, at 0.0004 units a token.
var (
	minimaxH3MaxRates      = map[string]float64{"768P": 1.6 * 0.0715, "2K": 1.6 * 0.0715}
	minimaxH3MaxInputRates = map[string]float64{"768P": 7.0 * 1008 * 0.0004 * 0.0715, "2K": 7.0 * 1008 * 0.0004 * 0.0715}
)

func newMiniMaxH3Engine(cloud *cloudLink, lib *library, isMax bool) *cloudVideoEngine {
	id, name, code, model := "minimax-h3", "MiniMax H3", "H3", "MiniMax H3"
	rates, input := minimaxH3Rates, minimaxH3Rates
	if isMax {
		id, name, code, model = "minimax-h3-max", "MiniMax H3 Max", "HM", "MiniMax H3 Max"
		rates, input = minimaxH3MaxRates, minimaxH3MaxInputRates
	}
	return &cloudVideoEngine{
		about: engineInfo{
			ID: id, Mode: modeMotion, Name: name, Via: "Comfy Cloud", Code: code, Billed: true, Tags: true,
			Rates: rates, InputRates: input, MinOutput: 4,
			Blurb: "Recasts the clip from your references, keeping its motion and camera. Takes images, videos, and audio.",
			// 9 images, 3 videos (the clip is one), 3 audio clips, 4 to 15 s.
			Limits: limits{MinRefs: 1, MaxRefs: 9, MaxVideoRefs: 2, MaxAudioRefs: 3, MinSeconds: 2, MaxSeconds: 15,
				PromptMax:   5000,
				RefsFor:     "Images of the cast, props, or setting; videos for motion; audio for voice or music. Name each in the prompt by its tag.",
				Resolutions: []string{"768P", "2K"}, DefaultResolution: "768P"},
		},
		cloud: cloud, lib: lib,
		node: func(t task, prompt string, seed int64) (string, map[string]any) {
			return "MinimaxHailuo03ReferenceNode", map[string]any{
				"model": model, "model.prompt": prompt, "model.resolution": t.Resolution, "model.ratio": "adaptive",
				"model.duration": outputSeconds(t.Source.Seconds, 4, 15), "seed": seed, "watermark": false,
			}
		},
		images: "model.reference_images.image_%d", videos: "model.reference_videos.video_%d",
		audios: "model.reference_audios.audio_%d",
		// The node says to name references "Image 1", "Video 1", "Audio 1".
		tags: tagStyle("Image %d", "Video %d", "Audio %d"),
	}
}
