package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/regiellis/comfy-api-proxy-go/sdk"
)

// fakeRouter is a Comfy Router queue for one model: submit, two status
// polls, then the model's native result document. The result's video URL
// points back at the same TLS server, which serves a small MP4.
type fakeRouter struct {
	t      *testing.T
	srv    *httptest.Server
	model  string
	result func(videoURL string) map[string]any

	mu      sync.Mutex
	polls   int
	body    map[string]any
	keys    []string
	cancels int
	down    bool // status polls answer 503, as an unreachable provider
}

func newFakeRouter(t *testing.T, model string, result func(string) map[string]any) *fakeRouter {
	f := &fakeRouter{t: t, model: model, result: result}
	base := "/v2/models/" + model + "/requests"
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+base, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("submit body: %v", err)
		}
		f.mu.Lock()
		f.body = body
		f.keys = append(f.keys, r.Header.Get("Idempotency-Key"))
		f.mu.Unlock()
		writeJSON(t, w, map[string]any{"request_id": "req-1", "status": "IN_QUEUE"})
	})
	mux.HandleFunc("GET "+base+"/req-1/status", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.polls++
		n, down := f.polls, f.down
		f.mu.Unlock()
		if down {
			w.WriteHeader(http.StatusServiceUnavailable)
			writeJSON(t, w, map[string]any{"error": map[string]any{"code": "upstream_unreachable", "message": "down"}})
			return
		}
		switch n {
		case 1:
			writeJSON(t, w, map[string]any{"status": "IN_QUEUE", "queue_position": 0})
		case 2:
			writeJSON(t, w, map[string]any{"status": "IN_PROGRESS"})
		default:
			writeJSON(t, w, map[string]any{"status": sdk.Completed})
		}
	})
	mux.HandleFunc("GET "+base+"/req-1", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, f.result(f.srv.URL+"/out/video.mp4"))
	})
	mux.HandleFunc("PUT "+base+"/req-1/cancel", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.cancels++
		f.mu.Unlock()
		writeJSON(t, w, map[string]any{"status": "CANCELLED"})
	})
	mux.HandleFunc("GET /out/video.mp4", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		if _, err := w.Write(fakeMP4(9)); err != nil {
			t.Error(err)
		}
	})
	f.srv = httptest.NewTLSServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func writeJSON(t *testing.T, w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Error(err)
	}
}

func (f *fakeRouter) models(t *testing.T) *sdk.Models {
	c, err := sdk.New(sdk.WithBaseURL(f.srv.URL), sdk.WithRouterBaseURL(f.srv.URL),
		sdk.WithAPIKey("test-key"), sdk.WithHTTPClient(f.srv.Client()), sdk.WithRetry(sdk.NoRetry))
	if err != nil {
		t.Fatal(err)
	}
	return c.Models
}

// harness wires a runner over a fresh store and library.
type harness struct {
	st   *store
	lib  *library
	pub  *recorder
	rn   *runner
	urls *staticURLs
}

func newHarness(t *testing.T, build func(urls urlSource) engine, web *http.Client) *harness {
	h := &harness{st: newTestStore(t), lib: newTestLibrary(t), pub: &recorder{}, urls: &staticURLs{}}
	h.lib.web = web
	refs, err := newLocalRefs("http://127.0.0.1:1", "")
	if err != nil {
		t.Fatal(err)
	}
	h.rn = newRunner(h.st, h.lib, &engines{list: []engine{build(h.urls)}}, refs, h.pub)
	t.Cleanup(h.rn.shutdown)
	return h
}

// seed stores a source clip and n references and returns a run over them.
func (h *harness) seed(t *testing.T, engineID, mode string, nrefs int, prompt string) Run {
	t.Helper()
	ctx := context.Background()
	src, err := h.lib.store(bytes.NewReader(fakeMP4(8)), originUpload, "clip.mp4", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.st.addMedia(ctx, src); err != nil {
		t.Fatal(err)
	}
	refs := []string{}
	for range nrefs {
		m, err := h.lib.store(bytes.NewReader(fakePNG(t)), originUpload, "ref.png", 0)
		if err != nil {
			t.Fatal(err)
		}
		if err := h.st.addMedia(ctx, m); err != nil {
			t.Fatal(err)
		}
		refs = append(refs, m.ID)
	}
	run := Run{ID: newID(), Batch: "batch", Mode: mode, Engine: engineID, Prompt: prompt,
		Resolution: "720p", SourceID: src.ID, RefIDs: refs, Status: runPreparing,
		IdempotencyKey: "utsushi-test-a", CreatedAt: now()}
	if err := h.st.addRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	return run
}

func (h *harness) wait(t *testing.T, id string) Run {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		r, err := h.st.run(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if !runActive(r.Status) {
			return r
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("run did not finish")
	return Run{}
}

func TestGenjutsuRunUploadsSubmitsFollowsAndKeepsTheResult(t *testing.T) {
	model := "higgsfield/higgsfield-genjutsu-motion-transfer-v1.0"
	f := newFakeRouter(t, model, func(u string) map[string]any {
		return map[string]any{"status": "completed", "request_id": "req-1", "video": map[string]any{"url": u}}
	})
	h := newHarness(t, func(urls urlSource) engine { return newGenjutsuEngine(fixedModels(f.models(t)), urls) }, f.srv.Client())
	run := h.seed(t, "genjutsu", modeMotion, 2, "recast as the knight")
	h.rn.start(run)
	done := h.wait(t, run.ID)

	if done.Status != runSucceeded || done.RequestID != "req-1" || done.ResultID == "" {
		t.Fatalf("finished as %+v", done)
	}
	result, err := h.st.media(context.Background(), done.ResultID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Origin != originResult || result.Seconds != 9 {
		t.Fatalf("kept result %+v", result)
	}
	f.mu.Lock()
	body, keys := f.body, f.keys
	f.mu.Unlock()
	if !slices.Equal(keys, []string{"utsushi-test-a"}) {
		t.Fatalf("idempotency keys %v", keys)
	}
	urls, _ := body["image_urls"].([]any)
	if len(urls) != 2 || body["prompt"] != "recast as the knight" || body["resolution"] != "720p" ||
		!strings.HasPrefix(body["video_url"].(string), "https://cdn.example.test/") {
		t.Fatalf("request body %v", body)
	}
	got := h.pub.statuses()
	for _, want := range []string{runQueued, runRunning, runSucceeded} {
		if !slices.Contains(got, want) {
			t.Fatalf("published %v, missing %s", got, want)
		}
	}
}

func TestProviderFailureLandsOnTheRunCard(t *testing.T) {
	f := newFakeRouter(t, "runway/aleph2", func(string) map[string]any {
		return map[string]any{"id": "t", "status": "FAILED"}
	})
	h := newHarness(t, func(urls urlSource) engine { return newAlephEngine(fixedModels(f.models(t)), urls) }, f.srv.Client())
	run := h.seed(t, "aleph2", modeSwap, 0, "swap the cup")
	h.rn.start(run)
	done := h.wait(t, run.ID)
	if done.Status != runFailed || !strings.Contains(done.Error, "FAILED") || done.ResultID != "" {
		t.Fatalf("finished as %+v", done)
	}
}

func TestWanEditBodyCarriesVideoAndReferences(t *testing.T) {
	f := newFakeRouter(t, "wan/wan2.7-videoedit", func(u string) map[string]any {
		return map[string]any{"request_id": "r", "output": map[string]any{"task_id": "t", "task_status": "SUCCEEDED", "video_url": u}}
	})
	h := newHarness(t, func(urls urlSource) engine { return newWanEditEngine(fixedModels(f.models(t)), urls) }, f.srv.Client())
	run := h.seed(t, "wan27-edit", modeSwap, 1, "make the cup glass")
	h.rn.start(run)
	if done := h.wait(t, run.ID); done.Status != runSucceeded {
		t.Fatalf("finished as %+v", done)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	input, _ := f.body["input"].(map[string]any)
	media, _ := input["media"].([]any)
	if len(media) != 2 || media[0].(map[string]any)["type"] != "video" || media[1].(map[string]any)["type"] != "reference_image" {
		t.Fatalf("input %v", input)
	}
}

func TestRecoverCollectsHeldRunsAndInterruptsTheRest(t *testing.T) {
	model := "higgsfield/higgsfield-genjutsu-motion-transfer-v1.0"
	f := newFakeRouter(t, model, func(u string) map[string]any {
		return map[string]any{"status": "completed", "request_id": "req-1", "video": map[string]any{"url": u}}
	})
	h := newHarness(t, func(urls urlSource) engine { return newGenjutsuEngine(fixedModels(f.models(t)), urls) }, f.srv.Client())
	ctx := context.Background()

	held := h.seed(t, "genjutsu", modeMotion, 1, "")
	held.Status, held.RequestID = runRunning, "req-1"
	if err := h.st.saveRun(ctx, held); err != nil {
		t.Fatal(err)
	}
	early := h.seed(t, "genjutsu", modeMotion, 1, "")

	if err := h.rn.recover(ctx); err != nil {
		t.Fatal(err)
	}
	if r := h.wait(t, held.ID); r.Status != runSucceeded {
		t.Fatalf("held run finished as %+v", r)
	}
	r, err := h.st.run(ctx, early.ID)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != runInterrupted {
		t.Fatalf("a run that never reached the provider is %s", r.Status)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.keys) != 0 {
		t.Fatalf("recovery submitted again: %v", f.keys)
	}
}

func TestOfflineEngineFailsWithoutBilling(t *testing.T) {
	h := newHarness(t, func(urls urlSource) engine { return newGenjutsuEngine(fixedModels(nil), urls) }, http.DefaultClient)
	run := h.seed(t, "genjutsu", modeMotion, 1, "")
	h.rn.start(run)
	done := h.wait(t, run.ID)
	if done.Status != runFailed || !strings.Contains(done.Error, "COMFY_API_KEY") {
		t.Fatalf("finished as %+v", done)
	}
	if len(h.urls.seen) != 0 {
		t.Fatal("an offline engine uploaded files")
	}
}

func TestAnimateExportBindsToRealNodes(t *testing.T) {
	refs, err := newLocalRefs("http://127.0.0.1:1", "")
	if err != nil {
		t.Fatal(err)
	}
	e := newAnimateEngine("workflows/wan-animate2.api.json", refs, nil)
	wf, b, err := e.load()
	if err != nil {
		t.Fatal(err)
	}
	for target, class := range map[string]string{b.Video: "LoadVideo", b.Reference: "LoadImage", b.Prompt: "CLIPTextEncode",
		b.Seed: "SamplerCustom", b.Width: "ResizeImageMaskNode", b.Output: "SaveVideo"} {
		node, input, _ := strings.Cut(target, ".")
		n, _ := wf.Graph[node].(map[string]any)
		if n["class_type"] != class {
			t.Errorf("%s is a %v, want %s", target, n["class_type"], class)
		}
		if inputs, _ := n["inputs"].(map[string]any); input != "" {
			if _, ok := inputs[input]; !ok {
				t.Errorf("%s: node %s has no input %q", target, node, input)
			}
		}
	}
	if err := e.ready(context.Background()); err == nil || !strings.Contains(err.Error(), "not running") {
		t.Fatalf("with no proxy, ready says %v", err)
	}
}

func TestFrameSizeKeepsTheClipAspect(t *testing.T) {
	for _, c := range []struct{ w, h, short, ww, wh int }{
		{1920, 1080, 480, 848, 480},
		{1080, 1920, 480, 480, 848},
		{1280, 720, 720, 1280, 720},
		{1000, 1000, 480, 480, 480},
	} {
		if w, h := frameSize(c.w, c.h, c.short); w != c.ww || h != c.wh {
			t.Errorf("%dx%d at %d: got %dx%d, want %dx%d", c.w, c.h, c.short, w, h, c.ww, c.wh)
		}
	}
}

func TestWan3PromptNamesEachReferenceByRole(t *testing.T) {
	p := wan3Prompt("", 2, 0, 0, "720P")
	for _, want := range []string{"@Video1", "Character appearance: @Image1, @Image2.", "Audio:"} {
		if !strings.Contains(p, want) {
			t.Errorf("default prompt lacks %q:\n%s", want, p)
		}
	}
	if !strings.HasSuffix(p, "\n720P") {
		t.Errorf("resolution is not last:\n%s", p)
	}
	own := wan3Prompt("@Image1 dances through @Video1 in the rain", 1, 0, 0, "480P")
	if strings.Contains(own, "Character appearance") || !strings.HasPrefix(own, "@Image1 dances") {
		t.Errorf("a prompt that tags its own references was rewritten:\n%s", own)
	}
	media := wan3Prompt("", 1, 2, 1, "720P")
	for _, want := range []string{"Further motion and scene reference: @Video2, @Video3.", "Audio: @Audio1."} {
		if !strings.Contains(media, want) {
			t.Errorf("prompt with videos and audio lacks %q:\n%s", want, media)
		}
	}
	if strings.Contains(media, "original sound") {
		t.Errorf("a prompt with reference audio still asks for the clip's sound:\n%s", media)
	}
}

func TestWan3DurationStaysInsideTheNodeRange(t *testing.T) {
	for clip, want := range map[float64]string{0.8: "2", 5.4: "5", 6.5: "7", 15: "15", 40: "15"} {
		if got := wan3Duration(clip); got != want {
			t.Errorf("clip %.1f s: duration %s, want %s", clip, got, want)
		}
	}
}

func TestWan3GraphWiresClipAndReferences(t *testing.T) {
	g := wan3Graph(wan3Inputs{video: "clip.mp4", images: []any{"a.png", "b.png"}, videos: []any{"walk.mp4"},
		audios: []any{"voice.wav", "beat.mp3"}}, "p", "480P", "4", 7)
	node := g["2"].(map[string]any)["inputs"].(map[string]any)
	if node["model"] != "wan3.0-video" || node["model.duration"] != "4" || node["model.resolution"] != "480P" {
		t.Fatalf("node inputs %v", node)
	}
	for input, src := range map[string]string{"model.reference_videos.video1": "1",
		"model.reference_images.image1": "10", "model.reference_images.image2": "11",
		"model.reference_videos.video2": "30", "model.reference_audios.audio1": "40", "model.reference_audios.audio2": "41"} {
		link, _ := node[input].([]any)
		if len(link) != 2 || link[0] != src {
			t.Errorf("%s links %v, want node %s", input, node[input], src)
		}
	}
	for id, want := range map[string]string{"30": "LoadVideo", "40": "LoadAudio", "41": "LoadAudio"} {
		if n, _ := g[id].(map[string]any); n["class_type"] != want {
			t.Errorf("node %s is %v, want %s", id, n["class_type"], want)
		}
	}
	if in := g["40"].(map[string]any)["inputs"].(map[string]any); in["audio"] != "voice.wav" {
		t.Errorf("LoadAudio inputs %v", in)
	}
	out := g[wan3Output].(map[string]any)
	if out["class_type"] != "SaveVideo" {
		t.Fatalf("output node %v", out)
	}
}

func TestLostContactRetriesThenWaitsForCollect(t *testing.T) {
	saved := collectBackoff
	collectBackoff = []time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { collectBackoff = saved })
	model := "higgsfield/higgsfield-genjutsu-motion-transfer-v1.0"
	f := newFakeRouter(t, model, func(u string) map[string]any {
		return map[string]any{"status": "completed", "request_id": "req-1", "video": map[string]any{"url": u}}
	})
	f.down = true
	h := newHarness(t, func(urls urlSource) engine { return newGenjutsuEngine(fixedModels(f.models(t)), urls) }, f.srv.Client())
	run := h.seed(t, "genjutsu", modeMotion, 1, "")
	h.rn.start(run)
	lost := h.wait(t, run.ID)
	if lost.Status != runLost || lost.RequestID != "req-1" || !strings.Contains(lost.Error, "Collect") {
		t.Fatalf("finished as %+v", lost)
	}
	f.mu.Lock()
	f.down, f.polls = false, 2
	keys := len(f.keys)
	f.mu.Unlock()
	if keys != 1 {
		t.Fatalf("retries submitted again: %d submissions", keys)
	}
	if err := h.rn.collect(context.Background(), run.ID); err != nil {
		t.Fatal(err)
	}
	if done := h.wait(t, run.ID); done.Status != runSucceeded || done.ResultID == "" {
		t.Fatalf("collected as %+v", done)
	}
}

func TestDescribeReadsTransportAndNodeErrors(t *testing.T) {
	refused := &url.Error{Op: "Post", URL: "http://127.0.0.1:8189/api/v2/assets", Err: syscall.ECONNREFUSED}
	if got := describe(fmt.Errorf("upload source: %w", refused)); got != "Could not reach 127.0.0.1:8189 (connection refused)." {
		t.Errorf("transport error reads %q", got)
	}
	class := "Wan3ReferenceToVideoApi"
	got := describe(jobFailure(&sdk.JobError{Code: "node_execution_error", Message: "insufficient\n credits", ClassType: &class}))
	if got != "Wan3ReferenceToVideoApi failed: insufficient credits." {
		t.Errorf("node failure reads %q", got)
	}
}

func TestBilledFailureBeforeSubmitSaysNothingWasBilled(t *testing.T) {
	h := newHarness(t, func(urls urlSource) engine {
		return newGenjutsuEngine(fixedModels(nil), urls) // offline: fails before any provider sees it
	}, http.DefaultClient)
	run := h.seed(t, "genjutsu", modeMotion, 1, "")
	h.rn.start(run)
	if done := h.wait(t, run.ID); !strings.HasSuffix(done.Error, "Nothing was billed.") {
		t.Fatalf("error %q", done.Error)
	}
}

func TestProgressLabelSkipsEmptyCounts(t *testing.T) {
	n := func(v int) *int { return &v }
	for _, c := range []struct {
		p    sdk.Progress
		want string
	}{
		{sdk.Progress{Step: n(3), Steps: n(8)}, "Step 3 of 8"},
		{sdk.Progress{Step: n(0), Steps: n(1)}, "Generating"},
		{sdk.Progress{NodesDone: n(0), NodesTotal: n(0)}, "Generating"},
		{sdk.Progress{NodesDone: n(2), NodesTotal: n(5)}, "Node 2 of 5"},
	} {
		if got := progressLabel(&c.p); got != c.want {
			t.Errorf("%+v reads %q, want %q", c.p, got, c.want)
		}
	}
}

func TestStyleRunSendsTheStyleImageFirst(t *testing.T) {
	model := "higgsfield/higgsfield-genjutsu-motion-transfer-v1.0"
	f := newFakeRouter(t, model, func(u string) map[string]any {
		return map[string]any{"status": "completed", "request_id": "req-1", "video": map[string]any{"url": u}}
	})
	h := newHarness(t, func(urls urlSource) engine {
		return newRestyler(newGenjutsuEngine(fixedModels(f.models(t)), urls), "genjutsu-style", 7, nil)
	}, f.srv.Client())
	ctx := context.Background()
	style, err := h.lib.store(bytes.NewReader(fakePNG(t)), originUpload, "look.png", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.st.addMedia(ctx, style); err != nil {
		t.Fatal(err)
	}
	run := h.seed(t, "genjutsu-style", modeStyle, 1, "")
	run.StyleImageID = style.ID
	if err := h.st.saveRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	h.rn.start(run)
	if done := h.wait(t, run.ID); done.Status != runSucceeded {
		t.Fatalf("finished as %+v", done)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	urls, _ := f.body["image_urls"].([]any)
	if len(urls) != 2 || !strings.HasSuffix(urls[0].(string), style.File) {
		t.Fatalf("image_urls %v, want the style image first", urls)
	}
	if p, _ := f.body["prompt"].(string); !strings.Contains(p, "Restyle the whole video") || !strings.Contains(p, "reference 2") {
		t.Fatalf("prompt %q", p)
	}
}

func TestPresetStyleRunNeedsTheLocalProxyAndBillsNothing(t *testing.T) {
	model := "higgsfield/higgsfield-genjutsu-motion-transfer-v1.0"
	f := newFakeRouter(t, model, func(u string) map[string]any { return map[string]any{} })
	h := newHarness(t, func(urls urlSource) engine {
		return newRestyler(newGenjutsuEngine(fixedModels(f.models(t)), urls), "genjutsu-style", 7, nil)
	}, f.srv.Client())
	run := h.seed(t, "genjutsu-style", modeStyle, 0, "")
	run.Style = "knitted-yarn"
	if err := h.st.saveRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	h.rn.start(run)
	done := h.wait(t, run.ID)
	if done.Status != runFailed || !strings.Contains(done.Error, "local proxy") || !strings.HasSuffix(done.Error, "Nothing was billed.") {
		t.Fatalf("finished as %+v", done)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.keys) != 0 {
		t.Fatal("the provider was called without a style frame")
	}
}

func TestLocalTemplateExportsBindToRealNodes(t *testing.T) {
	refs, err := newLocalRefs("http://127.0.0.1:1", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		e    *workflowEngine
		want map[string]string // binding -> class of its node
	}{
		{newLTXStyleBase(refs, nil), map[string]string{"video": "LoadVideo", "reference": "LoadImage", "prompt": "ComfySwitchNode",
			"seed": "KSampler", "width": "PrimitiveInt", "height": "PrimitiveInt", "length": "Video Slice", "output": "SaveVideo"}},
		{newMiniMaxH3LocalEngine(refs, nil), map[string]string{"video": "LoadVideo", "references": "MiniMaxH3ReferenceToVideo",
			"prompt": "PrimitiveStringMultiline", "seed": "RandomNoise", "width": "MiniMaxH3ReferenceToVideo",
			"height": "MiniMaxH3ReferenceToVideo", "length": "PrimitiveFloat", "output": "SaveVideo"}},
	} {
		wf, b, err := c.e.load()
		if err != nil {
			t.Fatalf("%s: %v", c.e.about.ID, err)
		}
		targets := map[string]string{"video": b.Video, "reference": b.Reference, "references": b.References, "prompt": b.Prompt,
			"seed": b.Seed, "width": b.Width, "height": b.Height, "length": b.Length, "output": b.Output}
		for name, class := range c.want {
			node, input, _ := strings.Cut(targets[name], ".")
			n, _ := wf.Graph[node].(map[string]any)
			if n["class_type"] != class {
				t.Errorf("%s %s: node %s is a %v, want %s", c.e.about.ID, name, node, n["class_type"], class)
			}
			if inputs, _ := n["inputs"].(map[string]any); input != "" && name != "references" {
				if _, ok := inputs[input]; !ok {
					t.Errorf("%s %s: node %s has no input %q", c.e.about.ID, name, node, input)
				}
			}
		}
		// LTX's IC-LoRA halves a latent that is already 1/32 of the frame.
		if want := map[string]int{"ltx23-iclora": 64, "minimax-h3-local": 32}[c.e.about.ID]; b.Multiple != want {
			t.Errorf("%s rounds frames to %d, want %d", c.e.about.ID, b.Multiple, want)
		}
	}
	// The LTX export lost its prompt-enhancer branch, which would demand a
	// LoRA the run never uses.
	wf, _, err := newLTXStyleBase(refs, nil).load()
	if err != nil {
		t.Fatal(err)
	}
	for id, raw := range wf.Graph {
		if n, _ := raw.(map[string]any); n["class_type"] == "TextGenerateLTX2Prompt" || n["class_type"] == "LoraLoader" {
			t.Errorf("node %s (%v) is still in the LTX export", id, n["class_type"])
		}
	}
}

func TestMiniMaxLocalNamesPicturesItsWay(t *testing.T) {
	e := newMiniMaxH3LocalEngine(nil, nil)
	if got := e.tags("@1 dances in @clip beside @2"); got != "<Picture 1> dances in the pose video beside <Picture 2>" {
		t.Errorf("tags %q", got)
	}
	if got := e.tags(e.prompt(task{})); !strings.HasPrefix(got, "The character from <Picture 1> performs") {
		t.Errorf("default prompt %q", got)
	}
}

func TestTemplateEnginesFollowTheOpenModelsSetting(t *testing.T) {
	refs, err := newLocalRefs("http://127.0.0.1:1", "")
	if err != nil {
		t.Fatal(err)
	}
	link, err := newCloudLink("https://cloud.example", "")
	if err != nil {
		t.Fatal(err)
	}
	where := onComfyUI
	ltx := newLTXStyleBase(refs, nil).onCloud(link, func() string { return where })
	style := newRestyler(ltx, "ltx23-style", 0, nil)
	if in := style.info(); in.Billed || in.Via != "Your ComfyUI" {
		t.Errorf("on your ComfyUI: %+v", in)
	}
	where = onCloud
	if in := style.info(); !in.Billed || in.Via != "Comfy Cloud" || in.Rates != nil || in.Mode != modeStyle {
		t.Errorf("on Comfy Cloud the Style engine reads %+v", in)
	}
	var off errOffline
	if err := ltx.ready(context.Background()); !errors.As(err, &off) || !strings.Contains(off.reason, "API key") {
		t.Errorf("on Cloud with no key, ready says %v", err)
	}
	if err := link.set("comfy-key"); err != nil {
		t.Fatal(err)
	}
	if err := ltx.ready(context.Background()); err != nil {
		t.Errorf("on Cloud with a key, ready says %v", err)
	}
}
