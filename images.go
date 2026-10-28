package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"github.com/regiellis/comfy-api-proxy-go/sdk"
)

// Image engines make library images: characters, objects, and places to use
// as references. Open models run on a ComfyUI (the one behind the proxy, or
// Comfy Cloud, as Settings choose); partner models always run on Comfy
// Cloud, which bills them to the Comfy API key. Each graph follows the
// ComfyUI template of the same name (comfyui_workflow_templates_json 0.1.112,
// read 2026-10-17), with the prompt-enhancer branch left out: it is off by
// default in every template.

// Where image engines run.
const (
	onComfyUI = "comfyui"
	onCloud   = "cloud"
)

// imagePurposes are the kinds of image the dialog offers. Each picks a
// default shape and wraps the subject in a prompt written for reference
// images: one subject, plain background, even light, no text.
var imagePurposes = []struct{ Key, Name, Hint, Aspect string }{
	{"character", "Character sheet", "Who is it? Look, age, build, clothing, colors.", "wide"},
	{"environment", "Environment sheet", "Where is it? Kind of place, era, materials, time of day.", "wide"},
	{"object", "Object", "What is it? Shape, material, color, condition.", "square"},
	{"place", "Place", "Where is it? Kind of space, era, materials, time of day.", "wide"},
	{"free", "Anything", "Describe the image as you want it.", "square"},
}

// imageAspects are the shapes offered: the open models' latent size, the
// partner aspect ratio, and Seedream's 1K and 2K presets.
var imageAspects = []struct {
	Key, Name, Ratio string
	W, H             int
	Seedream1K       string
	Seedream2K       string
}{
	{"square", "Square", "1:1", 1024, 1024, "(1K) 1024x1024 (1:1)", "(2K) 2048x2048 (1:1)"},
	{"portrait", "Portrait", "2:3", 832, 1216, "(1K) 832x1248 (2:3)", "(2K) 1664x2496 (2:3)"},
	{"landscape", "Landscape", "3:2", 1216, 832, "(1K) 1248x832 (3:2)", "(2K) 2496x1664 (3:2)"},
	{"wide", "Wide", "16:9", 1344, 768, "(1K) 1312x736 (16:9)", "(2K) 2848x1600 (16:9)"},
	{"tall", "Tall", "9:16", 768, 1344, "(1K) 736x1312 (9:16)", "(2K) 1600x2848 (9:16)"},
}

// aspectIndex is the position of an aspect key, the first (square) when the
// key is unknown.
func aspectIndex(key string) int {
	for i, a := range imageAspects {
		if a.Key == key {
			return i
		}
	}
	return 0
}

func knownAspect(key string) bool {
	for _, a := range imageAspects {
		if a.Key == key {
			return true
		}
	}
	return false
}

// imageJob is one image to make.
type imageJob struct {
	Prompt     string
	Aspect     string
	Resolution string // "1K" or "2K" for partner engines, empty for open ones
	Refs       []any  // uploaded asset references, in order
	Seed       int64
}

// imageEngine is one model that makes an image.
type imageEngine struct {
	ID, Name, Code, Blurb string
	target                string // explicit picker destination; blank for legacy/default use
	// Family picks how prompts are worded for the model (image_prompts.go).
	Family           string
	Partner          bool // runs on Comfy Cloud and bills per image
	MinRefs, MaxRefs int  // MaxRefs is zero for text to image only
	Resolutions      []string
	// price is the published USD per image for a resolution and reference
	// count, as a range when the provider bills by input size; nil for open
	// models, which publish none.
	price func(aspect, res string, refs int) (lo, hi float64)
	graph func(j imageJob) map[string]any
}

// Open models, in API format. Node ids are stable so tests can find them.

func zimageImage(j imageJob) map[string]any {
	a := imageAspects[aspectIndex(j.Aspect)]
	return map[string]any{
		"1":  node("UNETLoader", "unet_name", "z_image_turbo_bf16.safetensors", "weight_dtype", "default"),
		"2":  node("CLIPLoader", "clip_name", "qwen_3_4b.safetensors", "type", "lumina2", "device", "default"),
		"3":  node("VAELoader", "vae_name", "ae.safetensors"),
		"4":  node("CLIPTextEncode", "clip", link("2"), "text", j.Prompt),
		"5":  node("ConditioningZeroOut", "conditioning", link("4")),
		"6":  node("ModelSamplingAuraFlow", "model", link("1"), "shift", 3.0),
		"7":  node("EmptySD3LatentImage", "width", a.W, "height", a.H, "batch_size", 1),
		"8":  ksampler(link("6"), link("4"), link("5"), link("7"), j.Seed, 8, "res_multistep"),
		"9":  node("VAEDecode", "samples", link("8"), "vae", link("3")),
		"10": saveImage("9", "utsushi-zimage"),
	}
}

func krea2Image(j imageJob) map[string]any {
	a := imageAspects[aspectIndex(j.Aspect)]
	return map[string]any{
		"1":  node("UNETLoader", "unet_name", "krea2_turbo_fp8_scaled.safetensors", "weight_dtype", "default"),
		"2":  node("CLIPLoader", "clip_name", "qwen3vl_4b_fp8_scaled.safetensors", "type", "krea2", "device", "default"),
		"3":  node("VAELoader", "vae_name", "qwen_image_vae.safetensors"),
		"4":  node("CLIPTextEncode", "clip", link("2"), "text", j.Prompt),
		"5":  node("ConditioningZeroOut", "conditioning", link("4")),
		"7":  node("EmptyLatentImage", "width", a.W, "height", a.H, "batch_size", 1),
		"8":  ksampler(link("1"), link("4"), link("5"), link("7"), j.Seed, 8, "euler"),
		"9":  node("VAEDecode", "samples", link("8"), "vae", link("3")),
		"10": saveImage("9", "utsushi-krea2"),
	}
}

// qwenBase is Qwen Image 2.1's loaders and text encoder. References become
// the encoder's images, and with any the encoder's latent sets the size, as
// the Image Edit template does.
func qwenBase(j imageJob, unet string) (map[string]any, any) {
	enc := node("TextEncodeQwenImage21", "clip", link("2"), "vae", link("3"), "prompt", j.Prompt,
		"negative_prompt", "", "resolution", 1024)
	g := map[string]any{
		"1": node("UNETLoader", "unet_name", unet, "weight_dtype", "default"),
		"2": node("CLIPLoader", "clip_name", "qwen3vl_8b_int8_convrot.safetensors", "type", "qwen_image", "device", "default"),
		"3": node("VAELoader", "vae_name", "qwen_image_2.1_vae_bf16.safetensors"),
		"4": node("QwenImage21Cache", "model", link("1"), "device", "auto", "dtype", "default"),
		"5": enc,
	}
	for i, ref := range j.Refs {
		id := fmt.Sprintf("%d", 20+i)
		g[id] = node("LoadImage", "image", ref)
		enc["inputs"].(map[string]any)[fmt.Sprintf("images.image_%d", i+1)] = link(id)
	}
	if len(j.Refs) > 0 {
		return g, []any{"5", 2}
	}
	a := imageAspects[aspectIndex(j.Aspect)]
	g["7"] = node("EmptyLatentImage", "width", a.W, "height", a.H, "batch_size", 1)
	return g, link("7")
}

func qwenImage(j imageJob) map[string]any {
	g, latent := qwenBase(j, "qwen_image_2.1_int8_convrot.safetensors")
	g["8"] = ksampler(link("4"), link("5"), []any{"5", 1}, latent, j.Seed, 25, "euler")
	g["9"] = node("VAEDecode", "samples", link("8"), "vae", link("3"))
	g["10"] = saveImage("9", "utsushi-qwen")
	return g
}

// qwenTurboSigmas is the Turbo template's eight-step schedule.
const qwenTurboSigmas = "1.0, 0.978453, 0.95418, 0.926626, 0.89508, 0.845148, 0.704534, 0.414568, 0"

func qwenTurboImage(j imageJob) map[string]any {
	g, latent := qwenBase(j, "qwen_image_2.1_turbo_int8_convrot.safetensors")
	g["11"] = node("RandomNoise", "noise_seed", j.Seed)
	g["12"] = node("CFGGuider", "model", link("4"), "positive", link("5"), "negative", []any{"5", 1}, "cfg", 1.0)
	g["13"] = node("KSamplerSelect", "sampler_name", "euler")
	g["14"] = node("ManualSigmas", "sigmas", qwenTurboSigmas)
	g["8"] = node("SamplerCustomAdvanced", "noise", link("11"), "guider", link("12"), "sampler", link("13"),
		"sigmas", link("14"), "latent_image", latent)
	g["9"] = node("VAEDecode", "samples", link("8"), "vae", link("3"))
	g["10"] = saveImage("9", "utsushi-qwen-turbo")
	return g
}

// Partner models: one node plus a loader per reference.

func nanoBananaImage(model, thinking string) func(j imageJob) map[string]any {
	return func(j imageJob) map[string]any {
		in := map[string]any{
			"prompt": j.Prompt, "model": model, "model.aspect_ratio": imageAspects[aspectIndex(j.Aspect)].Ratio,
			"model.resolution": j.Resolution, "model.thinking_level": thinking,
			"seed": j.Seed, "response_modalities": "IMAGE", "temperature": 1.0, "top_p": 0.95,
		}
		return partnerGraph("GeminiNanoBanana2V2", in, j.Refs, "model.images.image_%d", 1, "utsushi-nanobanana")
	}
}

func krea3Image(j imageJob) map[string]any {
	in := map[string]any{
		"model": "Krea 3", "model.prompt": j.Prompt, "model.aspect_ratio": imageAspects[aspectIndex(j.Aspect)].Ratio,
		"model.resolution": j.Resolution, "model.thinking_mode": "fast", "model.precise_edit": false,
		"model.seed": j.Seed % (1 << 31),
	}
	return partnerGraph("Krea3ImageNode", in, j.Refs, "model.images.image_%d", 0, "utsushi-krea3")
}

func seedreamImage(j imageJob) map[string]any {
	a := imageAspects[aspectIndex(j.Aspect)]
	preset, scale := a.Seedream1K, 1
	if j.Resolution == "2K" {
		preset, scale = a.Seedream2K, 2
	}
	in := map[string]any{
		"prompt": j.Prompt, "model": "seedream 5.0 pro", "model.size_preset": preset,
		// Width and height count only for the Custom preset, but the node
		// requires them, at 1024 or more.
		"model.width": max(1024, a.W*scale), "model.height": max(1024, a.H*scale),
		"model.prompt_optimization": "standard", "model.seed": j.Seed % (1 << 31),
		"model.watermark": false, "model.thinking": true,
	}
	return partnerGraph("ByteDanceSeedreamNodeV3", in, j.Refs, "model.images.image_%d", 1, "utsushi-seedream")
}

// gptSizes are GPT Image's size presets for each shape. Wide and tall use
// its 2048x1152 and 1152x2048 presets; it has no 1K preset at 16:9.
var gptSizes = map[string]string{
	"square": "1024x1024", "portrait": "1024x1536", "landscape": "1536x1024", "wide": "2048x1152", "tall": "1152x2048",
}

// gptQuality is the quality every GPT Image run asks for: OpenAI's guide
// keeps low for drafts, and these are references meant to hold up.
const gptQuality = "high"

func gptImage(model string) func(j imageJob) map[string]any {
	return func(j imageJob) map[string]any {
		in := map[string]any{
			"prompt": j.Prompt, "model": model, "model.size": gptSizes[imageAspects[aspectIndex(j.Aspect)].Key],
			"model.custom_width": 1024, "model.custom_height": 1024, "model.background": "auto",
			"model.quality": gptQuality, "n": 1, "seed": j.Seed % (1 << 31),
		}
		return partnerGraph("OpenAIGPTImageNodeV2", in, j.Refs, "model.images.image_%d", 1, "utsushi-gpt-image")
	}
}

func partnerGraph(class string, inputs map[string]any, refs []any, key string, from int, prefix string) map[string]any {
	g := map[string]any{
		"1": map[string]any{"class_type": class, "inputs": inputs},
		"2": saveImage("1", prefix),
	}
	for i, ref := range refs {
		id := fmt.Sprintf("%d", 20+i)
		g[id] = node("LoadImage", "image", ref)
		inputs[fmt.Sprintf(key, from+i)] = link(id)
	}
	return g
}

// node is one API-format node from its class and input pairs.
func node(class string, kv ...any) map[string]any {
	in := map[string]any{}
	for i := 0; i+1 < len(kv); i += 2 {
		if k, ok := kv[i].(string); ok {
			in[k] = kv[i+1]
		}
	}
	return map[string]any{"class_type": class, "inputs": in}
}

func link(id string) []any { return []any{id, 0} }

func ksampler(model, pos, neg, latent any, seed int64, steps int, sampler string) map[string]any {
	return node("KSampler", "model", model, "positive", pos, "negative", neg, "latent_image", latent,
		"seed", seed, "steps", steps, "cfg", 1.0, "sampler_name", sampler, "scheduler", "simple", "denoise", 1.0)
}

func saveImage(from, prefix string) map[string]any {
	return node("SaveImage", "images", link(from), "filename_prefix", prefix)
}

// Published prices, from each node's price_badge in Comfy Cloud's
// object_info (read 2026-10-17).

func nanoBanana21Price(_, res string, _ int) (float64, float64) {
	p := map[string]float64{"1K": 0.048048, "2K": 0.072072}[res] + 0.010725 // MEDIUM thinking
	return p, p
}

func nanoBanana2Price(_, res string, _ int) (float64, float64) {
	p := map[string]float64{"1K": 0.0835, "2K": 0.1217}[res]
	return p, p
}

// krea3Price is exact at 1K; at 2K each reference adds 0.0286 to 0.1001.
func krea3Price(_, res string, refs int) (float64, float64) {
	if res == "2K" {
		return 0.0858 + 0.0286*float64(refs), 0.0858 + 0.1001*float64(refs)
	}
	p := 0.0429 + 0.0286*float64(refs)
	return p, p
}

// gptImage25Price is GPT Image 2.5's high-quality preset price for the
// shape, plus 0.0117 to 0.0176 per reference.
func gptImage25Price(aspect, _ string, refs int) (float64, float64) {
	p := map[string]float64{"1024x1024": 0.0753, "1024x1536": 0.0589, "1536x1024": 0.0589,
		"2048x1152": 0.0606, "1152x2048": 0.0606}[gptSizes[imageAspects[aspectIndex(aspect)].Key]]
	return p + 0.0117*float64(refs), p + 0.0176*float64(refs)
}

func seedreamPrice(_, res string, refs int) (float64, float64) {
	p := map[string]float64{"1K": 0.045, "2K": 0.09}[res] + 0.003*float64(max(refs-1, 0))
	return p, p
}

// FLUX 3 schema and price badge checked against ComfyUI object_info, 2026-10-22.
func flux3Image(j imageJob) map[string]any {
	return partnerGraph("Flux3ImageNode", map[string]any{
		"prompt": j.Prompt, "aspect_ratio": imageAspects[aspectIndex(j.Aspect)].Ratio,
		"resolution": j.Resolution, "grounding": false, "safety_tolerance": 2,
		"seed": j.Seed % (1 << 32),
	}, j.Refs, "images.image_%d", 1, "utsushi-flux3")
}
func flux3Price(_, res string, _ int) (float64, float64) {
	p := map[string]float64{"1K": 0.06864, "2K": 0.143}[res]
	return p, p
}

var imageEngines = []imageEngine{
	{ID: "flux3", Family: "flux", Name: "FLUX 3", Code: "F3", Partner: true, MaxRefs: 10, Resolutions: []string{"1K", "2K"},
		Blurb: "Black Forest Labs image creation and editing with up to 10 references. Grounding is off.", price: flux3Price, graph: flux3Image},
	{ID: "zimage", Family: "zimage", Name: "Z-Image Turbo", Code: "ZI",
		Blurb: "Fast and photographic. Follows plain descriptions well.", graph: zimageImage},
	{ID: "krea2", Family: "krea2", Name: "Krea-2 Turbo", Code: "K2",
		Blurb: "Made for looks: stylized, painterly, and concept-art images.", graph: krea2Image},
	{ID: "qwen21", Family: "qwen", Name: "Qwen Image 2.1", Code: "Q2",
		Blurb: "Strong at layout and lettering. 25 steps, so it takes longer.", graph: qwenImage},
	{ID: "qwen21-turbo", Family: "qwen", Name: "Qwen Image 2.1 Turbo", Code: "QT",
		Blurb: "Qwen Image 2.1 in eight steps.", graph: qwenTurboImage},
	{ID: "qwen21-edit", Family: "qwen-edit", Name: "Qwen Image 2.1 Edit", Code: "QE", MinRefs: 1, MaxRefs: 4,
		Blurb: "Reworks your references by instruction and keeps faces and products the same.", graph: qwenImage},
	{ID: "nanobanana21", Family: "gemini", Name: "Nano Banana 2.1", Code: "N1", Partner: true, MaxRefs: 14, Resolutions: []string{"1K", "2K"},
		Blurb: "Google's Gemini image model. Text to image, or edits from up to 14 references.",
		price: nanoBanana21Price, graph: nanoBananaImage("Gemini Nano Banana 2.1", "MEDIUM")},
	{ID: "nanobanana2", Family: "gemini", Name: "Nano Banana 2", Code: "N2", Partner: true, MaxRefs: 14, Resolutions: []string{"1K", "2K"},
		Blurb: "Gemini 3.1 Flash Image. Keeps a subject the same across edits.",
		price: nanoBanana2Price, graph: nanoBananaImage("Nano Banana 2 (Gemini 3.1 Flash Image)", "MINIMAL")},
	{ID: "krea3", Family: "krea3", Name: "Krea 3", Code: "K3", Partner: true, MaxRefs: 8, Resolutions: []string{"1K", "2K"},
		Blurb: "Text to image, or edits that change only what you name. Up to 8 references.",
		price: krea3Price, graph: krea3Image},
	{ID: "gpt-image-25-flare", Family: "gpt", Name: "GPT Image 2.5 Flare", Code: "GF", Partner: true, MaxRefs: 16,
		Blurb: "OpenAI's image model at high quality. Text to image, or edits from up to 16 references.",
		price: gptImage25Price, graph: gptImage("gpt-image-2.5-flare")},
	{ID: "gpt-image-25-sunburst", Family: "gpt", Name: "GPT Image 2.5 Sunburst", Code: "GS", Partner: true, MaxRefs: 16,
		Blurb: "The other GPT Image 2.5 model, same price and inputs. Text to image, or edits from up to 16 references.",
		price: gptImage25Price, graph: gptImage("gpt-image-2.5-sunburst")},
	{ID: "seedream5", Family: "seedream", Name: "Seedream 5.0 Pro", Code: "S5", Partner: true, MaxRefs: 10, Resolutions: []string{"1K", "2K"},
		Blurb: "ByteDance's image model: edits that keep light and texture, and clean lettering. Up to 10 references.",
		price: seedreamPrice, graph: seedreamImage},
}

func imageEngineByID(id string) (imageEngine, bool) {
	for _, e := range imageEngines {
		if e.ID == engineModelID(id) {
			if e.ID != id {
				if e.Partner {
					return imageEngine{}, false
				}
				_, e.target, _ = strings.Cut(id, "@")
				e.ID = id
			}
			return e, true
		}
	}
	return imageEngine{}, false
}

// where says where an engine runs, given the Settings choice for open
// models.
func (e imageEngine) where(open string) string {
	if e.target != "" {
		return e.target
	}
	if e.Partner || open == onCloud {
		return onCloud
	}
	return onComfyUI
}

// cost is the published price of one image, zero for open models.
func (e imageEngine) cost(aspect, res string, refs int) (lo, hi float64) {
	if e.price == nil {
		return 0, 0
	}
	return e.price(aspect, res, refs)
}

// costLabel is the price chip for an engine, and whether it is free.
func (e imageEngine) costLabel(where, aspect, res string, refs int) (string, bool) {
	switch {
	case e.price != nil:
		lo, hi := e.price(aspect, res, refs)
		if math.Abs(hi-lo) > 0.0005 {
			return fmt.Sprintf("$%.3f–%.3f/image", lo, hi), false
		}
		return fmt.Sprintf("$%.3f/image", lo), false
	case where == onCloud:
		return "Billed by Comfy Cloud", false
	default:
		return "Free · your GPU", true
	}
}

// workflow is the engine's graph for a job, ready to submit.
func (e imageEngine) workflow(j imageJob) (*sdk.Workflow, error) {
	raw, err := json.Marshal(e.graph(j))
	if err != nil {
		return nil, err
	}
	return sdk.WorkflowFromJSON(raw)
}

// readiness says why an engine cannot run where it would, or nil.
func (e imageEngine) readiness(ctx context.Context, where, key string, local *localRefs) error {
	switch {
	case where == onCloud && key == "":
		return errOffline{"Add a Comfy API key in Settings to use Comfy Cloud."}
	case where == onCloud:
		return nil
	}
	if err := local.ready(ctx); err != nil {
		return err
	}
	if local.models == nil {
		return nil
	}
	// A graph with no references names every model file the engine needs.
	wf, err := e.workflow(imageJob{Aspect: "square"})
	if err != nil {
		return err
	}
	return local.models.check(ctx, wf)
}

// imageChoices exposes both execution destinations for each open model.
func imageChoices() []imageEngine {
	var out []imageEngine
	for _, e := range imageEngines {
		if e.Partner {
			out = append(out, e)
			continue
		}
		for _, target := range []string{onComfyUI, onCloud} {
			clone := e
			clone.ID, clone.target = e.ID+"@"+target, target
			out = append(out, clone)
		}
	}
	return out
}
