package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/regiellis/comfy-api-proxy-go/sdk"
)

// serverReport is what Test connection learned about a ComfyUI.
type serverReport struct {
	OK                           bool
	Err                          string
	Version, GPU, Queue, Latency string
	Ready                        []readyRow
}

// readyRow says whether one feature can run on the server, and what it
// needs there.
type readyRow struct {
	Name, Need, State string
	OK                bool
}

// serverProbe reads a ComfyUI directly, before the proxy is moved to it.
type serverProbe struct {
	base, token string
	client      *http.Client
}

func newServerProbe(base, token string) (*serverProbe, error) {
	u, err := url.Parse(strings.TrimSpace(base))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("the address must be an http or https URL with a host, like http://192.168.1.20:8188")
	}
	return &serverProbe{base: strings.TrimRight(u.String(), "/"), token: token, client: &http.Client{Timeout: 5 * time.Second}}, nil
}

func (p *serverProbe) get(ctx context.Context, path string, out any) (err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.base+path, nil)
	if err != nil {
		return err
	}
	if p.token != "" {
		req.Header.Set("Authorization", "Bearer "+p.token)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("the server refused the request (%s); check the access token", resp.Status)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered %s", path, resp.Status)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(out)
}

// probe tests the server and checks each feature's models and nodes there.
// features maps a feature to the graph it runs, nil for one that needs only
// the Comfy key (key says whether one is set).
func (p *serverProbe) probe(ctx context.Context, features []probeFeature, key bool) serverReport {
	start := time.Now()
	var stats struct {
		System struct {
			Version string `json:"comfyui_version"`
		} `json:"system"`
		Devices []struct {
			Name      string `json:"name"`
			Type      string `json:"type"`
			VRAMTotal int64  `json:"vram_total"`
		} `json:"devices"`
	}
	if err := p.get(ctx, "/system_stats", &stats); err != nil {
		return serverReport{Err: describe(err)}
	}
	r := serverReport{OK: true, Version: stats.System.Version, Latency: fmt.Sprintf("%d ms", time.Since(start).Milliseconds())}
	if r.Version == "" {
		r.Version = "unknown"
	}
	r.GPU = "CPU only"
	for _, d := range stats.Devices {
		if d.Type != "cpu" && d.Name != "" {
			r.GPU = gpuLabel(d.Name, d.VRAMTotal)
			break
		}
	}
	var queue struct {
		Running []any `json:"queue_running"`
		Pending []any `json:"queue_pending"`
	}
	if err := p.get(ctx, "/queue", &queue); err == nil {
		r.Queue = fmt.Sprintf("%d running, %d waiting", len(queue.Running), len(queue.Pending))
	} else {
		r.Queue = "unknown"
	}
	folders := map[string][]string{}
	for _, f := range features {
		row := readyRow{Name: f.Name, Need: f.Need, OK: true, State: "Ready"}
		if f.NeedsKey && !key {
			row.OK, row.State = false, "Needs key"
		}
		if f.Graph != nil && row.OK {
			if miss := p.missing(ctx, f.Graph, folders); miss != "" {
				row.OK, row.State, row.Need = false, "Missing", miss
			}
		}
		r.Ready = append(r.Ready, row)
	}
	return r
}

// probeFeature is one thing the server may or may not run.
type probeFeature struct {
	Name, Need string
	Graph      *sdk.Workflow
	NeedsKey   bool
}

// missing names the first model file or node class graph needs that the
// server lacks, or is empty.
func (p *serverProbe) missing(ctx context.Context, wf *sdk.Workflow, folders map[string][]string) string {
	classes := map[string]bool{}
	for _, raw := range wf.Graph {
		node, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		class, _ := node["class_type"].(string)
		if class == "" || classes[class] {
			continue
		}
		classes[class] = true
		var info map[string]any
		if err := p.get(ctx, "/object_info/"+url.PathEscape(class), &info); err != nil || info[class] == nil {
			return "node " + class
		}
		loader, ok := loaderInputs[class]
		if !ok {
			continue
		}
		inputs, _ := node["inputs"].(map[string]any)
		name, _ := inputs[loader.input].(string)
		if name == "" {
			continue
		}
		files, ok := folders[loader.folder]
		if !ok {
			if err := p.get(ctx, "/models/"+loader.folder, &files); err != nil {
				return "models/" + loader.folder + " is unreadable"
			}
			folders[loader.folder] = files
		}
		if !hasModel(files, name) {
			return path.Base(name) + " in models/" + loader.folder
		}
	}
	return ""
}

func hasModel(files []string, name string) bool {
	want := path.Base(strings.ReplaceAll(name, `\`, "/"))
	for _, f := range files {
		if path.Base(strings.ReplaceAll(f, `\`, "/")) == want {
			return true
		}
	}
	return false
}
