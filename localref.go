package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strings"
	"time"

	"github.com/regiellis/comfy-api-proxy-go/sdk"
)

// zimageGraph is Z-Image Turbo in API format: eight steps, CFG 1, which the
// local 20 GB card renders in seconds. Node 4 takes the prompt, 7 the size,
// and 8 the seed.
const zimageGraph = `{
 "1": {"class_type": "UNETLoader", "inputs": {"unet_name": "z_image_turbo_bf16.safetensors", "weight_dtype": "default"}},
 "2": {"class_type": "CLIPLoader", "inputs": {"clip_name": "qwen_3_4b.safetensors", "type": "lumina2", "device": "default"}},
 "3": {"class_type": "VAELoader", "inputs": {"vae_name": "ae.safetensors"}},
 "4": {"class_type": "CLIPTextEncode", "inputs": {"clip": ["2", 0], "text": ""}},
 "5": {"class_type": "ConditioningZeroOut", "inputs": {"conditioning": ["4", 0]}},
 "6": {"class_type": "ModelSamplingAuraFlow", "inputs": {"model": ["1", 0], "shift": 3.0}},
 "7": {"class_type": "EmptySD3LatentImage", "inputs": {"width": 1024, "height": 1024, "batch_size": 1}},
 "8": {"class_type": "KSampler", "inputs": {"model": ["6", 0], "positive": ["4", 0], "negative": ["5", 0],
       "latent_image": ["7", 0], "seed": 0, "steps": 8, "cfg": 1.0, "sampler_name": "euler",
       "scheduler": "simple", "denoise": 1.0}},
 "9": {"class_type": "VAEDecode", "inputs": {"samples": ["8", 0], "vae": ["3", 0]}},
 "10": {"class_type": "SaveImage", "inputs": {"images": ["9", 0], "filename_prefix": "utsushi-ref"}}
}`

// aspects are the reference shapes offered, sized for Z-Image and above
// every partner's 240 pixel floor.
var aspects = map[string][2]int{
	"portrait":  {832, 1216},
	"square":    {1024, 1024},
	"landscape": {1216, 832},
}

// localRefs generates reference images on this machine: the SDK talks to the
// local comfy-api-proxy, which runs the graph on ComfyUI. Nothing is billed.
type localRefs struct {
	client   *sdk.Client
	proxyURL string
	probe    *http.Client
	// models fits a graph's model files to the ComfyUI behind the proxy;
	// nil leaves graphs as written (tests).
	models *modelIndex
}

func newLocalRefs(proxyURL, token string) (*localRefs, error) {
	opts := []sdk.Option{sdk.WithBaseURL(proxyURL), sdk.WithClientInfo("utsushi")}
	if token != "" {
		opts = append(opts, sdk.WithAPIKey(token))
	}
	c, err := sdk.New(opts...)
	if err != nil {
		return nil, err
	}
	return &localRefs{
		client:   c,
		proxyURL: strings.TrimRight(proxyURL, "/"),
		probe:    &http.Client{Timeout: 2 * time.Second},
	}, nil
}

// ready checks the proxy answers its health route, GET /api/v2/health.
func (l *localRefs) ready(ctx context.Context) (err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, l.proxyURL+"/api/v2/health", nil)
	if err != nil {
		return err
	}
	resp, err := l.probe.Do(req)
	if err != nil {
		return errOffline{"The local proxy at " + l.proxyURL + " is not running. Start it with task run."}
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()
	if resp.StatusCode != http.StatusOK {
		return errOffline{fmt.Sprintf("The local proxy answered its health check with %s.", resp.Status)}
	}
	return nil
}

// generate renders one image and streams it into w. progress receives a
// fraction and a label as the sampler steps.
func (l *localRefs) generate(ctx context.Context, prompt, aspect string, w io.Writer, progress func(string, float64)) error {
	size, ok := aspects[aspect]
	if !ok {
		return fmt.Errorf("unknown aspect %q", aspect)
	}
	wf, err := sdk.WorkflowFromJSON([]byte(zimageGraph))
	if err != nil {
		return err
	}
	wf.SetInput("4", "text", prompt)
	wf.SetInput("7", "width", size[0])
	wf.SetInput("7", "height", size[1])
	wf.SetInput("8", "seed", rand.Int64N(1<<53))
	return l.render(ctx, wf, w, progress)
}

// restyleGraph restyles a clip's first frame with Qwen Image 2.1 Edit, an
// instruction-following editor, as the owner's saved "Qwen Edit" workflow
// does: node 1 takes the clip, 8 the instruction, 9 the seed. Plain
// image-to-image (Z-Image at any denoise) either kept the photo or lost the
// people; the edit model keeps faces, pose, and framing.
const restyleGraph = `{
 "1": {"class_type": "LoadVideo", "inputs": {"file": ""}},
 "2": {"class_type": "GetVideoComponents", "inputs": {"video": ["1", 0]}},
 "3": {"class_type": "ImageFromBatch", "inputs": {"image": ["2", 0], "batch_index": 0, "length": 1}},
 "4": {"class_type": "UNETLoader", "inputs": {"unet_name": "qwen_image_2.1_int8_convrot.safetensors", "weight_dtype": "default"}},
 "5": {"class_type": "CLIPLoader", "inputs": {"clip_name": "qwen3vl_8b_int8_convrot.safetensors", "type": "qwen_image", "device": "default"}},
 "6": {"class_type": "VAELoader", "inputs": {"vae_name": "qwen_image_2.1_vae_bf16.safetensors"}},
 "7": {"class_type": "QwenImage21Cache", "inputs": {"model": ["4", 0], "device": "auto", "dtype": "default"}},
 "8": {"class_type": "TextEncodeQwenImage21", "inputs": {"clip": ["5", 0], "vae": ["6", 0], "prompt": "",
       "negative_prompt": "", "resolution": 1024, "images.image_1": ["3", 0]}},
 "9": {"class_type": "KSampler", "inputs": {"model": ["7", 0], "positive": ["8", 0], "negative": ["8", 1],
       "latent_image": ["8", 2], "seed": 0, "steps": 25, "cfg": 1.0, "sampler_name": "euler", "scheduler": "simple", "denoise": 1.0}},
 "10": {"class_type": "VAEDecode", "inputs": {"samples": ["9", 0], "vae": ["6", 0]}},
 "11": {"class_type": "SaveImage", "inputs": {"images": ["10", 0], "filename_prefix": "utsushi-style"}}
}`

// restyleFrame restyles the first frame of the clip at clipPath as look and
// streams the image into w.
func (l *localRefs) restyleFrame(ctx context.Context, clipPath, look string, w io.Writer, progress func(string, float64)) error {
	clip, err := l.client.AssetFromFile(clipPath).Reference(ctx)
	if err != nil {
		return fmt.Errorf("upload the clip for style preparation: %w", err)
	}
	wf, err := sdk.WorkflowFromJSON([]byte(restyleGraph))
	if err != nil {
		return err
	}
	wf.SetInput("1", "file", clip)
	wf.SetInput("8", "prompt", frameInstruction(look))
	wf.SetInput("9", "seed", rand.Int64N(1<<53))
	return l.render(ctx, wf, w, progress)
}

// render runs an image graph on the local proxy and streams its first
// output into w.
func (l *localRefs) render(ctx context.Context, wf *sdk.Workflow, w io.Writer, progress func(string, float64)) error {
	if err := l.fit(ctx, wf); err != nil {
		return err
	}
	return renderOn(ctx, l.client, wf, sdk.SubmitOptions{}, w, progress)
}

// renderOn submits an image graph with client (the local proxy or Comfy
// Cloud), follows it, and streams its first output into w.
func renderOn(ctx context.Context, client *sdk.Client, wf *sdk.Workflow, opts sdk.SubmitOptions, w io.Writer, progress func(string, float64)) error {
	job, err := client.Submit(ctx, wf, opts)
	if err != nil {
		return apiFailure(err)
	}
	err = watchJob(ctx, job, func(ev sdk.Event) {
		if p, ok := ev.(*sdk.Progress); ok {
			progress(progressLabel(p), p.Value)
		}
	})
	if err != nil {
		return err
	}
	if err := job.Refresh(ctx); err != nil {
		return apiFailure(err)
	}
	if job.Status() != sdk.StatusSucceeded {
		if f := job.Failure(); f != nil {
			return jobFailure(f)
		}
		return failedf("The image job ended %s", job.Status())
	}
	outs := job.Outputs()
	if len(outs) == 0 {
		return errors.New("the image job finished without an image")
	}
	_, err = outs[0].ToWriter(ctx, w, nil)
	return err
}

// fit points a graph's loaders at the model files the current ComfyUI has.
func (l *localRefs) fit(ctx context.Context, wf *sdk.Workflow) error {
	if l.models == nil {
		return nil
	}
	return l.models.fit(ctx, wf)
}

// frameGraph takes a clip's first frame as a PNG, for ComfyUI to serve when
// the server has no ffmpeg.
const frameGraph = `{
 "1": {"class_type": "LoadVideo", "inputs": {"file": ""}},
 "2": {"class_type": "GetVideoComponents", "inputs": {"video": ["1", 0]}},
 "3": {"class_type": "ImageFromBatch", "inputs": {"image": ["2", 0], "batch_index": 0, "length": 1}},
 "4": {"class_type": "SaveImage", "inputs": {"images": ["3", 0], "filename_prefix": "utsushi-frame"}}
}`

// firstFrame streams the first frame of the clip at clipPath into w.
func (l *localRefs) firstFrame(ctx context.Context, clipPath string, w io.Writer) error {
	clip, err := l.client.AssetFromFile(clipPath).Reference(ctx)
	if err != nil {
		return fmt.Errorf("upload the clip for style preparation: %w", err)
	}
	wf, err := sdk.WorkflowFromJSON([]byte(frameGraph))
	if err != nil {
		return err
	}
	wf.SetInput("1", "file", clip)
	return l.render(ctx, wf, w, func(string, float64) {})
}
