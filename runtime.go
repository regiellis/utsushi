package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"
)

// runtimeView is the top bar's two pills: where work can run right now.
type runtimeView struct {
	Local, Cloud     string
	LocalOK, CloudOK bool
}

// gpuNames reads the local GPU's name and memory for the top bar. The proxy
// reports its ComfyUI upstream in GET /api/v2/health, and ComfyUI's
// /system_stats names the device. The answer is cached, since the card does
// not change while the app runs.
type gpuNames struct {
	proxyURL string
	client   *http.Client

	mu       sync.Mutex
	hardware hardwareInfo
	label    string
	labelOf  string // the upstream the label describes
	checked  time.Time

	upMu   sync.Mutex
	up     string
	upRead time.Time
}

// upstream is the ComfyUI base URL the proxy fronts, from its health route,
// cached for a minute so a proxy pointed elsewhere is noticed.
func (g *gpuNames) upstream(ctx context.Context) (string, error) {
	g.upMu.Lock()
	defer g.upMu.Unlock()
	if g.up != "" && time.Since(g.upRead) < time.Minute {
		return g.up, nil
	}
	var health struct {
		Upstream string `json:"upstream"`
	}
	if err := g.fetch(ctx, g.proxyURL+"/api/v2/health", &health); err != nil {
		return "", err
	}
	if health.Upstream == "" {
		return "", fmt.Errorf("the proxy at %s reports no ComfyUI upstream", g.proxyURL)
	}
	g.up, g.upRead = strings.TrimRight(health.Upstream, "/"), time.Now()
	return g.up, nil
}

// lastUpstream is the most recent upstream read, for messages.
func (g *gpuNames) lastUpstream() string {
	g.upMu.Lock()
	defer g.upMu.Unlock()
	if g.up == "" {
		return "the proxy's upstream"
	}
	return g.up
}

func newGPUNames(proxyURL string) *gpuNames {
	return &gpuNames{proxyURL: strings.TrimRight(proxyURL, "/"), client: &http.Client{Timeout: 2 * time.Second}}
}

// get is "RTX 4000 SFF Ada · 20 GB", or empty when it cannot be read.
func (g *gpuNames) get(ctx context.Context) string {
	up, err := g.upstream(ctx)
	if err != nil {
		return ""
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.labelOf == up && time.Since(g.checked) < 30*time.Second {
		return g.label
	}
	g.checked, g.labelOf, g.label = time.Now(), up, ""
	g.hardware = hardwareInfo{}
	var stats struct {
		Devices []struct {
			Name      string `json:"name"`
			Type      string `json:"type"`
			VRAMTotal int64  `json:"vram_total"`
			VRAMFree  *int64 `json:"vram_free"`
		} `json:"devices"`
	}
	if err := g.fetch(ctx, up+"/system_stats", &stats); err != nil {
		return ""
	}
	for _, d := range stats.Devices {
		if d.Type == "cpu" || d.Name == "" {
			continue
		}
		g.label = gpuLabel(d.Name, d.VRAMTotal)
		g.hardware = hardwareInfo{Name: gpuLabel(d.Name, 0), Total: d.VRAMTotal, Free: d.VRAMFree, DeviceType: d.Type}
		break
	}
	return g.label
}

func (g *gpuNames) fetch(ctx context.Context, url string, out any) (err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered %s", url, resp.Status)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
}

// gpuLabel shortens ComfyUI's device name ("cuda:0 NVIDIA RTX 4000 SFF Ada
// Generation : cudaMallocAsync") to the card and its memory.
func gpuLabel(name string, vram int64) string {
	if i := strings.Index(name, " : "); i > 0 {
		name = name[:i]
	}
	if f := strings.Fields(name); len(f) > 1 && strings.Contains(f[0], ":") {
		name = strings.Join(f[1:], " ")
	}
	for _, cut := range []string{"NVIDIA GeForce ", "NVIDIA ", "AMD Radeon "} {
		name = strings.TrimPrefix(name, cut)
	}
	name = strings.TrimSuffix(name, " Generation")
	if vram <= 0 {
		return name
	}
	return fmt.Sprintf("%s · %.0f GB", name, math.Round(float64(vram)/(1<<30)))
}

func (w *web) runtime(ctx context.Context) runtimeView {
	ready := w.cloud.apiKey() != ""
	v := runtimeView{Cloud: "Comfy Cloud", CloudOK: ready}
	if !ready {
		v.Cloud = "Comfy Cloud · no key"
	}
	if err := w.run.refs.ready(ctx); err != nil {
		v.Local = "Local · proxy offline"
		return v
	}
	v.LocalOK = true
	v.Local = "Local"
	if gpu := w.gpu.get(ctx); gpu != "" {
		v.Local += " · " + gpu
	}
	return v
}

// hardwareSnapshot describes the ComfyUI execution device, not the browser GPU.
func (g *gpuNames) hardwareSnapshot(ctx context.Context) hardwareInfo {
	if g == nil {
		return hardwareInfo{}
	}
	if g.get(ctx) == "" {
		return hardwareInfo{}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.hardware
}
