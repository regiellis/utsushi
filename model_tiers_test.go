package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMemoryAdviceReservesHeadroomAndAllowsUnknown(t *testing.T) {
	for _, tc := range []struct {
		name    string
		hw      hardwareInfo
		dim     bool
		warning string
	}{
		{"below budget", hardwareInfo{Total: 12 << 30}, true, "exceed"},
		{"at budget", hardwareInfo{Total: 16 << 30}, false, ""},
		{"unknown", hardwareInfo{}, false, "unknown"},
		{"shared", hardwareInfo{Total: 32 << 30, DeviceType: "mps"}, false, "Shared-memory"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := localAdvice("zimage", tc.hw)
			if a.Dim != tc.dim || !strings.Contains(a.Warning, tc.warning) || !strings.Contains(a.Detail, "including 2 GB") {
				t.Fatalf("%+v", a)
			}
		})
	}
	free := int64(4 << 30)
	if a := localAdvice("zimage", hardwareInfo{Total: 24 << 30, Free: &free}); !a.Dim || !strings.Contains(a.Warning, "reclaim") {
		t.Fatalf("%+v", a)
	}
	if a := localAdvice("future-model", hardwareInfo{Total: 24 << 30}); a.Dim || a.Tier != "Local · unverified" {
		t.Fatalf("%+v", a)
	}
	if memoryTier(12) != "Low · 12 GB" || memoryTier(20) != "Mid · 16–20 GB" || memoryTier(32) != "High · 24–32 GB" {
		t.Fatal("wrong hardware bands")
	}
}

func TestCloudValueRequiresPriceAndQualityEvidence(t *testing.T) {
	if a := cloudAdvice(0, tierBest, "image"); a.Tier != "Cloud · price unknown" {
		t.Fatal(a)
	}
	if a := cloudAdvice(.04, tierUntested, "image"); a.Tier != "Cloud · Budget" {
		t.Fatal(a)
	}
	if a := cloudAdvice(.04, tierBest, "image"); a.Tier != "Cloud · Value" {
		t.Fatal(a)
	}
	if a := cloudAdvice(.25, tierWeak, "image"); !strings.Contains(a.Detail, "weak") {
		t.Fatal(a)
	}
}

func TestHardwareReadsExecutionServerMemory(t *testing.T) {
	var upstream string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/health":
			writeJSON(t, w, map[string]any{"upstream": upstream})
		case "/system_stats":
			writeJSON(t, w, map[string]any{"devices": []map[string]any{{"name": "cpu", "type": "cpu"}, {"name": "NVIDIA Test GPU", "type": "cuda", "vram_total": 24 << 30, "vram_free": 10 << 30}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	upstream = srv.URL
	g := newGPUNames(srv.URL)
	hw := g.hardwareSnapshot(context.Background())
	if hw.Name != "Test GPU" || hw.Total != 24<<30 || hw.Free == nil || *hw.Free != 10<<30 {
		t.Fatalf("%+v", hw)
	}
}

func TestFlux3WorkflowAndPrice(t *testing.T) {
	e, ok := imageEngineByID("flux3")
	if !ok || !e.Partner || e.MaxRefs != 10 || featuredModel(e.ID) != "Black Forest Labs" {
		t.Fatalf("%+v", e)
	}
	g := e.graph(imageJob{Prompt: "Use image 1", Aspect: "wide", Resolution: "2K", Seed: 9, Refs: []any{"a.png"}})
	raw, err := json.Marshal(g)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Flux3ImageNode", "images.image_1", "16:9", "2K", "SaveImage"} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("missing %s: %s", want, raw)
		}
	}
	lo, hi := e.cost("wide", "2K", 1)
	if lo != .143 || hi != .143 {
		t.Fatalf("%v–%v", lo, hi)
	}
	if !strings.Contains(imagePrompt(e, "character", "courier", 1), "image 1") {
		t.Fatal("wrong reference notation")
	}
}
