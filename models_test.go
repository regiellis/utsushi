package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/regiellis/comfy-api-proxy-go/sdk"
)

func TestModelIndexFitsGraphsToTheServersFolders(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/health":
			writeJSON(t, w, map[string]any{"status": "healthy", "upstream": srv.URL})
		case "/models/diffusion_models":
			writeJSON(t, w, []string{"ZIMAGE/z_image_turbo_bf16.safetensors"})
		case "/models/text_encoders":
			writeJSON(t, w, []string{"qwen_3_4b.safetensors"})
		case "/models/vae":
			writeJSON(t, w, []string{"flux/ae.safetensors"})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	x := newModelIndex(newGPUNames(srv.URL))
	wf, err := sdk.WorkflowFromJSON([]byte(zimageGraph))
	if err != nil {
		t.Fatal(err)
	}
	if err := x.fit(context.Background(), wf); err != nil {
		t.Fatal(err)
	}
	unet := wf.Graph["1"].(map[string]any)["inputs"].(map[string]any)["unet_name"]
	vae := wf.Graph["3"].(map[string]any)["inputs"].(map[string]any)["vae_name"]
	if unet != "ZIMAGE/z_image_turbo_bf16.safetensors" || vae != "flux/ae.safetensors" {
		t.Fatalf("unet %v, vae %v", unet, vae)
	}
	qwen, err := sdk.WorkflowFromJSON([]byte(restyleGraph))
	if err != nil {
		t.Fatal(err)
	}
	err = x.check(context.Background(), qwen)
	if err == nil || !strings.Contains(err.Error(), "has no") {
		t.Fatalf("a missing model reads %v", err)
	}
}
