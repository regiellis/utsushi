package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"strings"

	"github.com/regiellis/comfy-api-proxy-go/sdk"
)

// binding names the inputs of an exported workflow that this app fills, as
// "node_id.input_name", and the node whose video is the result. It lives
// beside the workflow as NAME.bind.json, so a re-export only needs its node
// ids checked, never code changes. Prompt, seed, width, height, and length
// are optional. A reference goes into an existing loader (Reference), or,
// for a template with none, into image loaders this app adds at a node's
// numbered inputs (References, such as "136.ref_images.ref_image_%d",
// numbered from 0).
type binding struct {
	Video      string `json:"video"`
	Reference  string `json:"reference,omitempty"`
	References string `json:"references,omitempty"`
	Prompt     string `json:"prompt"`
	Seed       string `json:"seed"`
	Width      string `json:"width"`
	Height     string `json:"height"`
	// Multiple is what width and height round to; 16 when unset.
	Multiple int `json:"multiple,omitempty"`
	// Length takes the clip's length in seconds.
	Length string `json:"length,omitempty"`
	Output string `json:"output"`
	// Control and ControlFrom serve an engine that follows a control clip
	// from the Tools page (workflowEngine.control): Control are the inputs
	// the template's own control video feeds ("172.control_video"), and
	// ControlFrom the node whose output 0 is the clip's frames. The
	// engine feeds each Control input from it, so the preprocessor the
	// template ran (SDPose, MoGe) drops out with its models.
	Control     []string `json:"control,omitempty"`
	ControlFrom string   `json:"control_from,omitempty"`
}

// shortSides maps the resolutions a workflow engine offers to the short side
// of the frame it renders.
var shortSides = map[string]int{"480p": 480, "720p": 720}

// workflowEngine runs an exported ComfyUI workflow on this machine's GPU:
// the SDK submits it to the local comfy-api-proxy, which uploads the inputs
// into ComfyUI and runs the graph. Nothing is billed.
type workflowEngine struct {
	about engineInfo
	path  string
	local *localRefs // the proxy client and its health check
	lib   *library
	// prompt writes the prompt when the visitor gives none, so a template's
	// sample prompt never reaches a run; nil keeps the template's.
	prompt func(t task) string
	// tags turns the studio's @1 into the model's reference names; nil
	// sends the prompt as typed.
	tags func(string) string
	// cloud and where let Settings run the template on Comfy Cloud instead
	// of the ComfyUI behind the proxy (Open models run on); nil runs local.
	cloud *cloudLink
	where func() string
	// control feeds the clip's own frames to the binding's Control input:
	// the clip is a control video made on the Tools page.
	control bool
}

// onCloud lets the engine run on Comfy Cloud when where says so.
func (e *workflowEngine) onCloud(c *cloudLink, where func() string) *workflowEngine {
	e.cloud, e.where = c, where
	return e
}

func (e *workflowEngine) inCloud() bool { return e.where != nil && e.where() == onCloud }

// client is the SDK client for where the engine runs now.
func (e *workflowEngine) client() (*sdk.Client, error) {
	if !e.inCloud() {
		return e.local.client, nil
	}
	c := e.cloud.cloud()
	if c == nil {
		return nil, errOffline{"Add a Comfy API key in Settings to run open models on Comfy Cloud."}
	}
	return c, nil
}

func newAnimateEngine(path string, local *localRefs, lib *library) *workflowEngine {
	return &workflowEngine{
		about: engineInfo{
			ID: "wan-animate2", Mode: modeMotion, Name: "Wan Animate 2", Via: "Your ComfyUI", Code: "WA",
			Rates: map[string]float64{"480p": 0, "720p": 0},
			Blurb: "Your reference character performs the clip's motion, on your ComfyUI. " +
				"Describe the character's look and the background, not the motion.",
			Limits: limits{MinRefs: 1, MaxRefs: 1, MinSeconds: 1, MaxSeconds: 30, PromptMax: 2000,
				RefsFor:     "The character who performs the clip's motion.",
				Resolutions: []string{"480p", "720p"}, DefaultResolution: "480p"},
		},
		path:  path,
		local: local,
		lib:   lib,
	}
}

// newMiniMaxH3LocalEngine runs ComfyUI's "MiniMax H3 Fun ControlNet
// Union" template on this machine: the clip's poses drive MiniMax H3, and
// the picked images are its references (<Picture 1> onward), loaders the
// template does not have, added per run.
func newMiniMaxH3LocalEngine(local *localRefs, lib *library) *workflowEngine {
	return &workflowEngine{
		about: engineInfo{
			ID: "minimax-h3-local", Mode: modeMotion, Name: "MiniMax H3 Pose", Via: "Your ComfyUI", Code: "MP", Tags: true,
			Rates: map[string]float64{"480p": 0, "720p": 0},
			Blurb: "Your references perform the clip's poses, on your ComfyUI: MiniMax H3 with its pose ControlNet.",
			// The node takes 9 reference images; it was trained on 124 to
			// 362 frames at 24 fps, about 5 to 15 s.
			Limits: limits{MinRefs: 1, MaxRefs: 9, MinSeconds: 2, MaxSeconds: 15, PromptMax: 4000,
				RefsFor:     "The character who performs the clip's poses, and any props or setting. Name each by its tag.",
				Resolutions: []string{"480p", "720p"}, DefaultResolution: "480p"},
		},
		path: "workflows/minimax-h3-control.api.json", local: local, lib: lib,
		tags: func(p string) string {
			p = strings.ReplaceAll(p, "@clip", "the pose video")
			return tagStyle("<Picture %d>", "<Video %d>", "<Audio %d>")(p)
		},
		prompt: func(t task) string {
			return "The character from @1 performs the motion of the pose video with its timing and camera, " +
				"keeping the face, hair, build, clothing, and colors from @1."
		},
	}
}

// newMiniMaxH3ControlEngine runs the same MiniMax H3 ControlNet Union
// template as the Pose engine, but follows a depth, pose, or edge clip made
// on the Tools page as its control video instead of running SDPose on the
// clip, so it needs no SDPose model.
func newMiniMaxH3ControlEngine(local *localRefs, lib *library) *workflowEngine {
	e := newMiniMaxH3LocalEngine(local, lib)
	e.control = true
	e.about.ID, e.about.Name, e.about.Code = "minimax-h3-control", "MiniMax H3 Control", "MC"
	e.about.Blurb = "Your references move as a depth, pose, or edge clip from Tools shows, on your ComfyUI: MiniMax H3 with " +
		"its union ControlNet. Pick the control clip as the source."
	e.about.Limits.ControlClip = true
	e.about.Limits.RefsFor = "The character who moves as the control clip shows, and any props or setting. Name each by its tag."
	e.tags = func(p string) string {
		p = strings.ReplaceAll(p, "@clip", "the control video")
		return tagStyle("<Picture %d>", "<Video %d>", "<Audio %d>")(p)
	}
	e.prompt = func(t task) string {
		return "The character from @1 moves as the control video shows, with its timing and camera, keeping the face, " +
			"hair, build, clothing, and colors from @1."
	}
	return e
}

// newLTXControlEngine runs the LTX-2.3 IC-LoRA Union template on a depth,
// pose, or edge clip made on the Tools page instead of the depth it makes
// with MoGe. Its one image is the video's opening frame.
func newLTXControlEngine(local *localRefs, lib *library) *workflowEngine {
	e := newLTXStyleBase(local, lib)
	e.control = true
	e.about.ID, e.about.Name, e.about.Code = "ltx23-control", "LTX-2.3 Control", "LC"
	e.about.Blurb = "A depth, pose, or edge clip from Tools drives LTX-2.3 from an opening image you pick, on your ComfyUI. " +
		"Pick the control clip as the source."
	e.about.Limits.ControlClip = true
	e.about.Limits.RefsFor = "The opening frame: your cast already in the place, as the video starts. Make one with Create image " +
		"from a frame of the original clip."
	return e
}

// newLTXStyleBase runs ComfyUI's "LTX 2.3 IC-LoRA Union Control" template
// on this machine: depth from the clip, and an image as the first frame.
// It is only used through Style mode, where that image is the restyled
// first frame, so the look carries through the clip's own motion.
func newLTXStyleBase(local *localRefs, lib *library) *workflowEngine {
	return &workflowEngine{
		about: engineInfo{
			ID: "ltx23-iclora", Mode: modeMotion, Name: "LTX-2.3 IC-LoRA", Via: "Your ComfyUI", Code: "LX",
			Rates: map[string]float64{"480p": 0, "720p": 0},
			Limits: limits{MinRefs: 1, MaxRefs: 1, MinSeconds: 2, MaxSeconds: 20, PromptMax: 4000,
				RefsFor:     "The first frame: the clip's opening shot in the new look.",
				Resolutions: []string{"480p", "720p"}, DefaultResolution: "480p"},
		},
		path: "workflows/ltx23-iclora.api.json", local: local, lib: lib,
	}
}

// info says where the engine runs now. On Comfy Cloud it is billed by GPU
// time, which has no published price, so the studio asks before each run.
func (e *workflowEngine) info() engineInfo {
	in := e.about
	if e.inCloud() {
		in.Via, in.Billed, in.Rates = "Comfy Cloud", true, nil
	}
	return in
}

func (e *workflowEngine) bindPath() string {
	return strings.TrimSuffix(e.path, ".api.json") + ".bind.json"
}

func (e *workflowEngine) ready(ctx context.Context) error {
	wf, _, err := e.load()
	if err != nil {
		return err
	}
	if e.inCloud() {
		// Cloud's published model lists leave models out, so they are not
		// checked here; a missing one fails the job with its name.
		_, err := e.client()
		return err
	}
	if err := e.local.ready(ctx); err != nil {
		return err
	}
	if e.local.models != nil {
		return e.local.models.check(ctx, wf)
	}
	return nil
}

// load reads the workflow and its binding, and checks every bound input
// names a node that exists.
func (e *workflowEngine) load() (*sdk.Workflow, binding, error) {
	wf, err := sdk.WorkflowFromFile(e.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, binding{}, errOffline{"The workflow export " + e.path + " is missing."}
	}
	if err != nil {
		return nil, binding{}, errOffline{"The workflow export does not load: " + err.Error()}
	}
	raw, err := os.ReadFile(e.bindPath())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, binding{}, errOffline{"Write " + e.bindPath() + " naming the video, reference, and output nodes."}
	}
	if err != nil {
		return nil, binding{}, err
	}
	var b binding
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, binding{}, errOffline{"The binding file is not valid JSON: " + err.Error()}
	}
	if b.Reference == "" && b.References == "" {
		return nil, binding{}, errOffline{"The binding file must name the reference input."}
	}
	targets := map[string]string{"video": b.Video, "reference": b.Reference, "prompt": b.Prompt,
		"seed": b.Seed, "width": b.Width, "height": b.Height, "length": b.Length, "references": b.References}
	for name, target := range targets {
		if target == "" {
			if name == "video" {
				return nil, binding{}, errOffline{"The binding file must name the video input."}
			}
			continue
		}
		node, input, ok := strings.Cut(target, ".")
		if !ok || node == "" || input == "" {
			return nil, binding{}, errOffline{fmt.Sprintf("Binding %q must be node_id.input, got %q.", name, target)}
		}
		if _, ok := wf.Graph[node].(map[string]any); !ok {
			return nil, binding{}, errOffline{fmt.Sprintf("Binding %q names node %s, which the workflow does not have.", name, node)}
		}
	}
	if (b.Width == "") != (b.Height == "") {
		return nil, binding{}, errOffline{"The binding file must name both width and height, or neither."}
	}
	if _, ok := wf.Graph[b.Output].(map[string]any); !ok {
		return nil, binding{}, errOffline{fmt.Sprintf("The binding output %q is not a node in the workflow.", b.Output)}
	}
	if e.control {
		if len(b.Control) == 0 || b.ControlFrom == "" {
			return nil, binding{}, errOffline{"The binding file must name the control input and the node that gives the clip's frames."}
		}
		if _, ok := wf.Graph[b.ControlFrom].(map[string]any); !ok {
			return nil, binding{}, errOffline{fmt.Sprintf("The binding's control_from %q is not a node in the workflow.", b.ControlFrom)}
		}
		for _, in := range b.Control {
			setBound(wf, in, []any{b.ControlFrom, 0})
		}
		prune(wf, b.Output)
	}
	return wf, b, nil
}

// prune drops every node the output no longer reaches. ComfyUI runs every
// output node, so a preprocessor's PreviewImage left behind would still
// run it, and the model check would still ask for its models.
func prune(wf *sdk.Workflow, output string) {
	keep := map[string]bool{}
	var visit func(id string)
	visit = func(id string) {
		if keep[id] {
			return
		}
		node, ok := wf.Graph[id].(map[string]any)
		if !ok {
			return
		}
		keep[id] = true
		inputs, ok := node["inputs"].(map[string]any)
		if !ok {
			return
		}
		for _, v := range inputs {
			if link, ok := v.([]any); ok && len(link) == 2 {
				if from, ok := link[0].(string); ok {
					visit(from)
				}
			}
		}
	}
	visit(output)
	for id := range wf.Graph {
		if !keep[id] {
			delete(wf.Graph, id)
		}
	}
}

// setBound sets one input. Node ids hold no dots, so the first one ends the
// id and input names such as "resize_type.width" stay whole.
func setBound(wf *sdk.Workflow, target string, value any) {
	node, input, _ := strings.Cut(target, ".")
	wf.SetInput(node, input, value)
}

// frameSize keeps the clip's aspect at the chosen short side, in multiples
// of 16 as Wan's latent grid needs.
func frameSize(w, h, short int) (int, int) { return frameSizeBy(w, h, short, 16) }

// frameSizeBy is frameSize in multiples of m (LTX and MiniMax H3 need 32).
func frameSizeBy(w, h, short, m int) (int, int) {
	step := float64(m)
	round := func(v float64) int { return max(m, int(math.Round(v/step))*m) }
	if w >= h {
		return round(float64(short) * float64(w) / float64(h)), round(float64(short))
	}
	return round(float64(short)), round(float64(short) * float64(h) / float64(w))
}

func (e *workflowEngine) clipSize(m Media) (int, int, error) {
	f, err := e.lib.open(m)
	if err != nil {
		return 0, 0, err
	}
	w, h, err := mp4Size(f)
	if cerr := f.Close(); cerr != nil && err == nil {
		err = cerr
	}
	return w, h, err
}

func (e *workflowEngine) generate(ctx context.Context, t task, tr tracker) (outcome, error) {
	wf, b, err := e.load()
	if err != nil {
		return outcome{}, err
	}
	if b.Width != "" {
		short, ok := shortSides[t.Resolution]
		if !ok {
			return outcome{}, failedf("Resolution %q is not one this engine renders", t.Resolution)
		}
		w, h, err := e.clipSize(t.Source)
		if err != nil {
			return outcome{}, failedf("Could not read the clip's frame size: %v", err)
		}
		fw, fh := frameSizeBy(w, h, short, cmp.Or(b.Multiple, 16))
		setBound(wf, b.Width, fw)
		setBound(wf, b.Height, fh)
	}
	if !e.inCloud() {
		if err := e.local.fit(ctx, wf); err != nil {
			return outcome{}, err
		}
	}
	client, err := e.client()
	if err != nil {
		return outcome{}, err
	}
	tr.running("Sending the clip", 0)
	video, err := client.AssetFromFile(e.lib.path(t.Source)).Reference(ctx)
	if err != nil {
		return outcome{}, fmt.Errorf("upload source: %w", err)
	}
	images := t.ofKind(kindImage)
	if len(images) == 0 {
		return outcome{}, failedf("%s needs a reference image", e.about.Name)
	}
	setBound(wf, b.Video, video)
	if b.Reference != "" {
		images = images[:1]
	}
	for i, m := range images {
		tr.running(fmt.Sprintf("Sending reference %d of %d", i+1, len(images)), 0)
		ref, err := client.AssetFromFile(e.lib.path(m)).Reference(ctx)
		if err != nil {
			return outcome{}, fmt.Errorf("upload reference: %w", err)
		}
		if b.Reference != "" {
			setBound(wf, b.Reference, ref)
			continue
		}
		// The template has no loader for references: add one per image.
		id := fmt.Sprintf("utsushi_ref_%d", i)
		wf.Graph[id] = map[string]any{"class_type": "LoadImage", "inputs": map[string]any{"image": ref}}
		setBound(wf, fmt.Sprintf(b.References, i), []any{id, 0})
	}
	if b.Length != "" {
		setBound(wf, b.Length, math.Round(t.Source.Seconds*10)/10)
	}
	prompt := t.Prompt
	if prompt == "" && e.prompt != nil {
		prompt = e.prompt(t)
	}
	if e.tags != nil {
		prompt = e.tags(prompt)
	}
	if b.Prompt != "" && prompt != "" {
		setBound(wf, b.Prompt, prompt)
	}
	if b.Seed != "" {
		// Each version gets its own seed, or four versions render alike;
		// the parts of one run share it.
		setBound(wf, b.Seed, t.seed(1<<53))
	}
	tr.running("Submitting to ComfyUI", 0)
	job, err := client.Submit(ctx, wf, sdk.SubmitOptions{IdempotencyKey: t.Key})
	if err != nil {
		return outcome{}, apiFailure(err)
	}
	tr.queued(job.ID(), job.QueuePosition())
	return followJob(ctx, job, b.Output, tr)
}

func (e *workflowEngine) resume(ctx context.Context, jobID string, tr tracker) (outcome, error) {
	_, b, err := e.load()
	if err != nil {
		return outcome{}, err
	}
	client, err := e.client()
	if err != nil {
		return outcome{}, err
	}
	job, err := client.GetJob(ctx, jobID)
	if err != nil {
		return outcome{}, apiFailure(err)
	}
	return followJob(ctx, job, b.Output, tr)
}

// followJob streams a v2 job's progress into tr, then returns the first video
// the output node saved.
func followJob(ctx context.Context, job *sdk.Job, output string, tr tracker) (outcome, error) {
	err := watchJob(ctx, job, func(ev sdk.Event) {
		switch ev := ev.(type) {
		case *sdk.Progress:
			tr.running(progressLabel(ev), ev.Value)
		case *sdk.StatusChange:
			if ev.QueuePosition != nil {
				tr.queued(job.ID(), ev.QueuePosition)
			} else if !sdk.IsTerminal(ev.Status) {
				tr.running(queueLabel(ev.Status), 0)
			}
		}
	})
	if err != nil {
		return outcome{}, err
	}
	if err := job.Refresh(ctx); err != nil {
		return outcome{}, apiFailure(err)
	}
	if job.Status() != sdk.StatusSucceeded {
		if f := job.Failure(); f != nil {
			return outcome{}, jobFailure(f)
		}
		return outcome{}, failedf("The workflow ended %s", job.Status())
	}
	for _, o := range job.GetOutputs(output) {
		if strings.HasPrefix(o.ContentType(), "video/") {
			return outcome{Write: func(ctx context.Context, w io.Writer) error {
				_, err := o.ToWriter(ctx, w, nil)
				return err
			}}, nil
		}
	}
	return outcome{}, failedf("The workflow finished without a video from node %s", output)
}

func (e *workflowEngine) cancel(ctx context.Context, jobID string) error {
	client, err := e.client()
	if err != nil {
		return err
	}
	return cancelJob(ctx, client, jobID)
}

func cancelJob(ctx context.Context, client *sdk.Client, jobID string) error {
	job, err := client.GetJob(ctx, jobID)
	if err != nil {
		return err
	}
	return job.Cancel(ctx)
}

// progressLabel reads a v2 progress event as "Step 4 of 8". Counts that
// say nothing (a loop node's "Step 0 of 1", a resumed job's "Node 0 of 0")
// read as "Generating"; the card's bar still shows the fraction.
func progressLabel(p *sdk.Progress) string {
	switch {
	case p.Message != nil && *p.Message != "":
		return *p.Message
	case p.Step != nil && p.Steps != nil && *p.Steps > 1:
		return fmt.Sprintf("Step %d of %d", *p.Step, *p.Steps)
	case p.NodesDone != nil && p.NodesTotal != nil && *p.NodesTotal > 0:
		return fmt.Sprintf("Node %d of %d", *p.NodesDone, *p.NodesTotal)
	}
	return "Generating"
}

// watchJob passes a job's events to on until it ends. When the proxy has no
// event stream to spare (429 too_many_streams) it polls instead, as the
// refusal itself advises, and progress simply goes unreported.
func watchJob(ctx context.Context, job *sdk.Job, on func(sdk.Event)) error {
	for ev, err := range job.Events(ctx) {
		if err != nil {
			var api *sdk.APIError
			if errors.As(err, &api) && api.Code == "too_many_streams" {
				return apiFailure(job.Wait(ctx))
			}
			return apiFailure(err)
		}
		on(ev)
	}
	return nil
}
