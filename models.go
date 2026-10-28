package main

import (
	"context"
	"fmt"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/regiellis/comfy-api-proxy-go/sdk"
)

// loaderInputs maps a loader node's model input to the ComfyUI models folder
// it names a file in.
var loaderInputs = map[string]struct{ input, folder string }{
	"UNETLoader":             {"unet_name", "diffusion_models"},
	"CLIPLoader":             {"clip_name", "text_encoders"},
	"CLIPVisionLoader":       {"clip_name", "clip_vision"},
	"VAELoader":              {"vae_name", "vae"},
	"LoraLoaderModelOnly":    {"lora_name", "loras"},
	"CheckpointLoaderSimple": {"ckpt_name", "checkpoints"},
	"ModelPatchLoader":       {"name", "model_patches"},
}

// modelIndex resolves the model files a graph names against the ComfyUI
// behind the proxy. Installs file the same model under different folders
// (one keeps z_image_turbo_bf16.safetensors in ZIMAGE/), so a graph names
// files by base name and is rewritten to the server's path before submit.
// The ComfyUI is found through the proxy's health route; folder listings
// are cached for a minute.
type modelIndex struct {
	names *gpuNames // shares the upstream lookup and HTTP client

	mu      sync.Mutex
	folders map[string][]string
	read    map[string]time.Time
}

func newModelIndex(names *gpuNames) *modelIndex {
	return &modelIndex{names: names, folders: map[string][]string{}, read: map[string]time.Time{}}
}

// list is a models folder's files on the current ComfyUI.
func (x *modelIndex) list(ctx context.Context, folder string) ([]string, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if files, ok := x.folders[folder]; ok && time.Since(x.read[folder]) < time.Minute {
		return files, nil
	}
	upstream, err := x.names.upstream(ctx)
	if err != nil {
		return nil, err
	}
	var files []string
	if err := x.names.fetch(ctx, upstream+"/models/"+folder, &files); err != nil {
		return nil, fmt.Errorf("list ComfyUI models/%s: %w", folder, err)
	}
	x.folders[folder], x.read[folder] = files, time.Now()
	return files, nil
}

// resolve is the server's path for a model file named by its base name.
func (x *modelIndex) resolve(ctx context.Context, folder, name string) (string, error) {
	files, err := x.list(ctx, folder)
	if err != nil {
		return "", err
	}
	want := path.Base(strings.ReplaceAll(name, `\`, "/"))
	for _, f := range files {
		if f == name {
			return f, nil
		}
	}
	for _, f := range files {
		if path.Base(strings.ReplaceAll(f, `\`, "/")) == want {
			return f, nil
		}
	}
	return "", errOffline{fmt.Sprintf("The ComfyUI at %s has no %s in models/%s.", x.names.lastUpstream(), want, folder)}
}

// fit rewrites every loader in wf to the server's model paths, or names
// the first file the server lacks.
func (x *modelIndex) fit(ctx context.Context, wf *sdk.Workflow) error { return x.walk(ctx, wf, true) }

// check reports the first model wf names that the server lacks, and
// changes nothing.
func (x *modelIndex) check(ctx context.Context, wf *sdk.Workflow) error {
	return x.walk(ctx, wf, false)
}

func (x *modelIndex) walk(ctx context.Context, wf *sdk.Workflow, apply bool) error {
	for id, raw := range wf.Graph {
		node, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		class, _ := node["class_type"].(string)
		loader, ok := loaderInputs[class]
		if !ok {
			continue
		}
		inputs, _ := node["inputs"].(map[string]any)
		name, ok := inputs[loader.input].(string)
		if !ok || name == "" {
			continue
		}
		got, err := x.resolve(ctx, loader.folder, name)
		if err != nil {
			return err
		}
		if apply {
			wf.SetInput(id, loader.input, got)
		}
	}
	return nil
}
