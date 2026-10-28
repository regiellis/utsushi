package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/regiellis/comfy-api-proxy-go/sdk"
)

func TestPrefsRoundTripPrivately(t *testing.T) {
	path := filepath.Join(t.TempDir(), "utsushi", "settings.toml")
	s, err := openPrefs(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.get(); got.Server.Mode != "local" || got.Engines.Parallel != 1 {
		t.Fatalf("a missing file should read as the defaults, got %+v", got.Server)
	}
	if _, err := s.update(func(p *Prefs) {
		p.Server.Mode, p.Server.RemoteURL = "remote", "http://10.0.0.5:8188"
		p.LLM.Keys = map[string]string{"claude": "sk-ant-secret"}
	}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("settings file is %o, want 600: it holds keys", info.Mode().Perm())
	}
	again, err := openPrefs(path)
	if err != nil {
		t.Fatal(err)
	}
	p := again.get()
	if p.Server.URL() != "http://10.0.0.5:8188" || p.LLM.Keys["claude"] != "sk-ant-secret" {
		t.Errorf("reloaded prefs lost values: %+v %v", p.Server, p.LLM.Keys)
	}
	p.LLM.Keys["claude"] = "changed"
	if again.get().LLM.Keys["claude"] != "sk-ant-secret" {
		t.Error("get hands out the stored map, so a caller's edit leaks into the prefs")
	}
}

func TestPrefsRefuseAnUnknownKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.toml")
	if err := os.WriteFile(path, []byte("[server]\nmdoe = 'remote'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := openPrefs(path); err == nil {
		t.Fatal("a typo in the settings file must fail, or the next save replaces it with defaults")
	}
}

func TestPrefsKeepTheOldValueWhenTheWriteFails(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "utsushi")
	s, err := openPrefs(filepath.Join(dir, "settings.toml"))
	if err != nil {
		t.Fatal(err)
	}
	// A regular file where the settings folder should be makes the save fail.
	if err := os.WriteFile(dir, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.update(func(p *Prefs) { p.Engines.Parallel = 4 }); err == nil {
		t.Fatal("writing under a regular file should fail")
	}
	if s.get().Engines.Parallel != 1 {
		t.Error("a failed save changed the prefs in memory")
	}
}

func TestExportNameFillsThePattern(t *testing.T) {
	at := time.Date(2026, 10, 17, 9, 5, 7, 0, time.Local)
	r := Run{ID: "abcdef1234567890", Mode: modeStyle, Engine: "genjutsu-style", Style: "felt-puppet"}
	for _, c := range []struct{ pattern, version, want string }{
		{"", "", "2026-10-17_genjutsu-style_v1"},
		{"{date}_{time}_{mode}_{style}_{v}_{id}", "v2", "2026-10-17_090507_style_felt-puppet_v2_abcdef12"},
		{"../{engine}/{v}", "v3", "genjutsu-style-v3"},
		{"{style}__{v}", "v1", "felt-puppet_v1"},
	} {
		if got := exportName(c.pattern, r, c.version, at); got != c.want {
			t.Errorf("exportName(%q) = %q, want %q", c.pattern, got, c.want)
		}
	}
	if got := exportName("{style}", Run{ID: "x", Mode: modeStyle}, "v1", at); got != "image" {
		t.Errorf("a style run with its own image names the style %q, want image", got)
	}
}

func TestGateHoldsRunsToTheLimit(t *testing.T) {
	g := newGate(1)
	ctx := context.Background()
	if err := g.enter(ctx, nil); err != nil {
		t.Fatal(err)
	}
	busy := 0
	entered := make(chan error, 1)
	go func() { entered <- g.enter(ctx, func() { busy++ }) }()
	select {
	case err := <-entered:
		t.Fatalf("second run entered a full gate (%v)", err)
	case <-time.After(50 * time.Millisecond):
	}
	g.setLimit(2)
	if err := <-entered; err != nil {
		t.Fatal(err)
	}
	if busy != 1 {
		t.Errorf("busy called %d times, want once", busy)
	}

	g.setLimit(1)
	g.leave()
	cctx, cancel := context.WithCancel(ctx)
	waiting := make(chan error, 1)
	go func() { waiting <- g.enter(cctx, nil) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	if err := <-waiting; !errors.Is(err, context.Canceled) {
		t.Errorf("a canceled wait returned %v, want context.Canceled", err)
	}
}

func TestEstimateUSDUsesPublishedRatesOnly(t *testing.T) {
	wan := engineInfo{Billed: true, Rates: map[string]float64{"480P": 0.0715, "720P": 0.143}, Limits: limits{MaxSeconds: 10}}
	for _, c := range []struct {
		in       engineInfo
		res      string
		seconds  float64
		versions int
		want     float64
		known    bool
	}{
		{engineInfo{Billed: false}, "", 30, 4, 0, true},
		{engineInfo{Billed: true}, "720P", 5, 1, 0, false},
		{wan, "720P", 4.4, 2, 0.143 * 4 * 2, true},
		{wan, "480P", 0.5, 1, 0.0715 * 2, true},
		{wan, "480P", 60, 1, 0.0715 * 10, true},
		{wan, "1080P", 5, 1, 0, false},
	} {
		got, known := estimateUSD(c.in, task{Resolution: c.res, Source: Media{Seconds: c.seconds}}, c.versions)
		if known != c.known || (known && !near(got, c.want)) {
			t.Errorf("%s %.1fs x%d = %.4f (known %v), want %.4f (known %v)", c.res, c.seconds, c.versions, got, known, c.want, c.known)
		}
	}
}

func near(a, b float64) bool { return a-b < 1e-9 && b-a < 1e-9 }

func TestNewLLMExplainsWhyItIsOffline(t *testing.T) {
	for _, c := range []struct {
		p    LLMPrefs
		want string
	}{
		{LLMPrefs{}, "off in Settings"},
		{LLMPrefs{On: true, Provider: "claude"}, "API key for Claude"},
		{LLMPrefs{On: true, Provider: "custom"}, "base URL"},
	} {
		_, err := newLLM(c.p)
		var off errOffline
		if !errors.As(err, &off) || !strings.Contains(off.reason, c.want) {
			t.Errorf("%+v gave %v, want an offline reason naming %q", c.p, err, c.want)
		}
	}
	client, err := newLLM(LLMPrefs{On: true, Provider: "llamacpp", URLs: map[string]string{"llamacpp": "http://127.0.0.1:9903/v1/"}})
	if err != nil {
		t.Fatal(err)
	}
	if client.base != "http://127.0.0.1:9903/v1" || client.model != "" {
		t.Errorf("llama.cpp client is %q model %q, want the saved URL and the server default", client.base, client.model)
	}
}

func TestLLMChatSendsImagesAndKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			if err := json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{{"id": "zeta"}, {"id": "alpha"}}}); err != nil {
				t.Error(err)
			}
			return
		case "/v1/chat/completions":
		default:
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer k-123" || r.Header.Get("x-api-key") != "k-123" {
			t.Errorf("key headers: %q %q", r.Header.Get("Authorization"), r.Header.Get("x-api-key"))
		}
		var body struct {
			Model    string `json:"model"`
			Messages []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body.Model != "m1" || len(body.Messages) != 2 || !strings.Contains(string(body.Messages[1].Content), "data:image/jpeg;base64,AQID") ||
			!strings.Contains(string(body.Messages[1].Content), "Reference @1:") {
			t.Errorf("chat body: model %q, %d messages, user %s", body.Model, len(body.Messages), body.Messages[1].Content)
		}
		reply := "  a reply  "
		if strings.Contains(string(body.Messages[1].Content), "empty") {
			reply = ""
		}
		if err := json.NewEncoder(w).Encode(map[string]any{"choices": []map[string]any{{"message": map[string]string{"content": reply}}}}); err != nil {
			t.Error(err)
		}
	}))
	defer srv.Close()
	client, err := newLLM(LLMPrefs{On: true, Provider: "custom", URLs: map[string]string{"custom": srv.URL + "/v1"},
		Keys: map[string]string{"custom": "k-123"}, Models: map[string]string{"custom": "m1"}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	img := []llmImage{{Type: "image/jpeg", Data: []byte{1, 2, 3}, Label: "Reference @1:"}}
	out, err := client.chat(ctx, "sys", "describe", img)
	if err != nil || out != "a reply" {
		t.Errorf("chat = %q, %v", out, err)
	}
	if _, err := client.chat(ctx, "sys", "empty", img); !errors.As(err, new(failure)) {
		t.Errorf("an empty reply gave %v, want a failure", err)
	}
	ids, err := client.models(ctx)
	if err != nil || strings.Join(ids, ",") != "alpha,zeta" {
		t.Errorf("models = %v, %v", ids, err)
	}
}

func TestSceneCheckKeepsTagsHonest(t *testing.T) {
	tagged := llmScene{tags: true, allowed: []string{"@1", "@2", "@clip"}, need: []string{"@1", "@2"}}
	plain := llmScene{}
	for _, c := range []struct {
		sc   llmScene
		out  string
		fail bool
	}{
		{tagged, "@1 drives the car from @clip, @2 rides along", false},
		{tagged, "@1 drives the car", true},
		{tagged, "@1 and @2 meet @3", true},
		{plain, "a woman with red hair drives the car", false},
		{plain, "@1 drives the car", true},
	} {
		if problem := c.sc.check(c.out); (problem != "") != c.fail {
			t.Errorf("check(%q) = %q, want failure %v", c.out, problem, c.fail)
		}
	}
}

func TestTagsCompareAsASet(t *testing.T) {
	if got := strings.Join(tags("@2 in @clip, then @1 and @2 again"), " "); got != "@1 @2 @clip" {
		t.Errorf("tags = %q", got)
	}
}

func TestMaskedShowsOnlyTheEnds(t *testing.T) {
	for in, want := range map[string]string{"short": "•••••", "comfy_abcdefghijkl": "comfy_••••••••ijkl", "abcdefghijklmnop": "••••••••mnop"} {
		if got := masked(in); got != want {
			t.Errorf("masked(%q) = %q, want %q", in, got, want)
		}
	}
}

// fakeComfy serves the ComfyUI routes the server probe reads.
func fakeComfy(t *testing.T, token string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token != "" && r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var v any
		switch r.URL.Path {
		case "/system_stats":
			v = map[string]any{"system": map[string]any{"comfyui_version": "0.41.3"},
				"devices": []map[string]any{{"name": "cuda:0 NVIDIA GeForce RTX 3090 : cudaMallocAsync", "type": "cuda", "vram_total": int64(24) << 30}}}
		case "/queue":
			v = map[string]any{"queue_running": []any{1}, "queue_pending": []any{}}
		case "/object_info/UNETLoader":
			v = map[string]any{"UNETLoader": map[string]any{}}
		case "/object_info/KSampler":
			v = map[string]any{"KSampler": map[string]any{}}
		case "/models/diffusion_models":
			v = []string{"ZIMAGE/z_image_turbo_bf16.safetensors"}
		default:
			http.NotFound(w, r)
			return
		}
		if err := json.NewEncoder(w).Encode(v); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestServerProbeReportsWhatEachFeatureLacks(t *testing.T) {
	srv := fakeComfy(t, "tok")
	graph := func(class, model string) *sdk.Workflow {
		return sdk.NewWorkflow(map[string]any{"1": map[string]any{"class_type": class, "inputs": map[string]any{"unet_name": model}}})
	}
	features := []probeFeature{
		{Name: "found in a subfolder", Graph: graph("UNETLoader", "z_image_turbo_bf16.safetensors")},
		{Name: "missing model", Graph: graph("UNETLoader", "wan2.2_animate.safetensors")},
		{Name: "missing node", Graph: graph("WanAnimateToVideo", "")},
		{Name: "cloud", NeedsKey: true},
	}
	p, err := newServerProbe(srv.URL+"/", "tok")
	if err != nil {
		t.Fatal(err)
	}
	r := p.probe(context.Background(), features, false)
	if !r.OK || r.Version != "0.41.3" || r.Queue != "1 running, 0 waiting" || !strings.Contains(r.GPU, "3090") {
		t.Fatalf("report: %+v", r)
	}
	want := []string{"Ready", "Missing", "Missing", "Needs key"}
	for i, row := range r.Ready {
		if row.State != want[i] {
			t.Errorf("%s: %s (%s), want %s", row.Name, row.State, row.Need, want[i])
		}
	}
	if r.Ready[1].Need != "wan2.2_animate.safetensors in models/diffusion_models" || r.Ready[2].Need != "node WanAnimateToVideo" {
		t.Errorf("missing reasons: %q, %q", r.Ready[1].Need, r.Ready[2].Need)
	}

	p, err = newServerProbe(srv.URL, "wrong")
	if err != nil {
		t.Fatal(err)
	}
	if r := p.probe(context.Background(), nil, true); r.OK || !strings.Contains(r.Err, "access token") {
		t.Errorf("a refused token reads %+v", r)
	}
	if _, err := newServerProbe("192.168.1.20:8188", ""); err == nil {
		t.Error("an address without a scheme should be refused")
	}
}
