package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// Tools make library items from one library item, on this machine, free.
// Each tool is an entry in toolList: what it takes, what an order makes,
// and how it makes it. The page, the job plumbing, and the cards are
// shared, and what a tool makes is ordinary library media of origin
// "tool", so every picker offers it.
type tool struct {
	ID, Name, Code string
	Blurb          string // one line in the tool picker
	Action         string // the button: "Make depth"
	// Takes is the source kinds the tool reads, in the switch's order.
	Takes []string
	// Model marks a tool that runs a model. Those run one at a time,
	// since each keeps every core busy; the rest start at once.
	Model bool
	// plan reads the order's form and names the items it makes, or
	// refuses it with a failure.
	plan func(o toolOrder) ([]toolOut, error)
	// run makes the items, keeping each with job.keep as it is ready.
	run func(j *toolJob) error
	// formOf, when set, rebuilds the options of an item made before
	// orders kept them, from its label.
	formOf func(label string) url.Values
}

// toolOrder is one press of a tool's button.
type toolOrder struct {
	Source Media
	Form   url.Values
}

// toolOut is one item an order makes.
type toolOut struct{ Kind, Label string }

// toolList is set in init, since its entries refer to functions that
// refer back to it.
var toolList []*tool

func init() {
	toolList = []*tool{depthTool, poseTool, edgesTool, matteTool, frameTool, conformTool, audioTool, upscaleTool, interpTool}
}

func toolByID(id string) (*tool, bool) {
	for _, t := range toolList {
		if t.ID == id {
			return t, true
		}
	}
	return nil, false
}

// toolKit is what tools share on the runner: the model files, the device
// the models run on, and the lock that runs model tools one at a time.
type toolKit struct {
	models *models
	device toolDevice
	heavy  sync.Mutex
}

// makeTool adds a pending library item per output and makes them in the
// background.
func (r *runner) makeTool(ctx context.Context, t *tool, o toolOrder) ([]Media, error) {
	outs, err := r.toolPlan(t, o)
	if err != nil {
		return nil, err
	}
	form := orderForm(o.Form).Encode()
	made := make([]Media, 0, len(outs))
	for _, out := range outs {
		m := Media{
			ID: newID(), Kind: out.Kind, Origin: originTool, Tool: t.ID, Label: out.Label,
			Status: mediaPending, CreatedAt: now(), Note: "Starting", SourceID: o.Source.ID, ToolForm: form,
		}
		if err := r.st.addMedia(ctx, m); err != nil {
			return made, err
		}
		made = append(made, m)
	}
	r.wg.Go(func() {
		r.safely("tool "+t.ID+" "+made[0].ID, func() { r.runTool(t, o, made) })
	})
	return made, nil
}

// toolPlan checks the tool can read the source here and plans the order.
func (r *runner) toolPlan(t *tool, o toolOrder) ([]toolOut, error) {
	if r.tools == nil || r.frames == nil || !r.frames.available() || r.frames.probe == "" {
		return nil, errOffline{"Tools need ffmpeg and ffprobe on the server."}
	}
	if t.Model {
		if err := modelPlatform(); err != nil {
			return nil, err
		}
	}
	if !slices.Contains(t.Takes, o.Source.Kind) {
		return nil, failedf("%s takes %s", t.Name, kindWords(t.Takes))
	}
	if o.Source.Width <= 0 || o.Source.Height <= 0 {
		return nil, failedf("The size of %s is unknown, so %s cannot read it", o.Source.Label, strings.ToLower(t.Name))
	}
	return t.plan(o)
}

// orderForm is a posted form's tool options: what the page sends less the
// tool and source, which the item keeps in columns of their own.
func orderForm(f url.Values) url.Values {
	out := url.Values{}
	for k, v := range f {
		switch k {
		case "tool", "source", "source_kind", "image":
			continue
		}
		out[k] = v
	}
	return out
}

// retryTool makes a failed item again, as its order asked: the same tool,
// source, and options. It is planned afresh and only this item is kept, in
// place; the order's other items stay as they are.
func (r *runner) retryTool(ctx context.Context, m Media) (Media, error) {
	t, ok := toolByID(m.Tool)
	if !ok {
		return Media{}, failedf("This was made by a tool Utsushi no longer has")
	}
	src, err := r.st.media(ctx, m.SourceID)
	if errors.Is(err, errNotFound) || (err == nil && src.Status != mediaReady) {
		return Media{}, failedf("Its source is no longer in the library")
	}
	if err != nil {
		return Media{}, err
	}
	form, err := url.ParseQuery(m.ToolForm)
	if err != nil {
		return Media{}, fmt.Errorf("read its options: %w", err)
	}
	if m.ToolForm == "" && t.formOf != nil {
		form = t.formOf(m.Label)
	}
	o := toolOrder{Source: src, Form: form}
	outs, err := r.toolPlan(t, o)
	if err != nil {
		return Media{}, err
	}
	at := -1
	for i, out := range outs {
		if out.Label == m.Label {
			at = i
		}
	}
	if at < 0 && len(outs) == 1 {
		at = 0
	}
	if at < 0 {
		return Media{}, failedf("Its options are unknown. Make it again from the tool")
	}
	m.Status, m.Error, m.Note, m.Kind = mediaPending, "", "Starting", outs[at].Kind
	if err := r.st.finishMedia(ctx, m); err != nil {
		return Media{}, err
	}
	// Only this item is made; the others are placeholders keep discards.
	held := make([]Media, len(outs))
	held[at] = m
	r.wg.Go(func() {
		r.safely("tool retry "+t.ID+" "+m.ID, func() { r.runTool(t, o, held) })
	})
	return m, nil
}

// kindWords is "a clip or an image".
func kindWords(kinds []string) string {
	words := map[string]string{kindVideo: "a clip", kindImage: "an image", kindAudio: "audio"}
	out := make([]string, len(kinds))
	for i, k := range kinds {
		out[i] = words[k]
	}
	return strings.Join(out, " or ")
}

// runTool runs one order and records every item ready or failed.
func (r *runner) runTool(t *tool, o toolOrder, outs []Media) {
	j := &toolJob{r: r, tool: t, order: o, outs: outs, ctx: r.base}
	defer j.cleanup()
	err := func() error {
		if t.Model {
			if !r.tools.heavy.TryLock() {
				j.note(true, "Waiting for the job ahead")
				r.tools.heavy.Lock()
			}
			defer r.tools.heavy.Unlock()
		}
		err := t.run(j)
		if t.Model && j.r.tools.device.GPU && gpuFull(err) {
			// Another program can hold most of the GPU (a local ComfyUI took
			// 15.5 of 20 GB): the same job runs on the CPU instead.
			who := gpuHolders()
			log.Printf("utsushi: %s ran out of GPU memory, held by %q; running it on the CPU: %v", t.ID, who, err)
			if who != "" {
				j.note(true, "GPU full (%s): running on the CPU", who)
			} else {
				j.note(true, "GPU full: running on the CPU")
			}
			sessionsOnCPU(true)
			defer sessionsOnCPU(false)
			err = t.run(j)
		}
		return err
	}()
	if err == nil {
		err = errors.New("the tool finished without making this")
	}
	for _, m := range j.outs {
		if m.Status == mediaPending {
			m.Status, m.Note, m.Error = mediaFailed, "", describe(err)
			j.finish(m)
		}
	}
}

// toolJob is one order in flight.
type toolJob struct {
	r     *runner
	tool  *tool
	order toolOrder
	outs  []Media
	ctx   context.Context
	last  time.Time
	temp  []string
}

// src is the source file's path.
func (j *toolJob) src() string { return j.r.lib.path(j.order.Source) }

// note shows a progress line on every pending item, at most twice a
// second unless forced. A trailing "· 42%" fills the card's bar.
func (j *toolJob) note(force bool, format string, args ...any) {
	if !force && time.Since(j.last) < 500*time.Millisecond {
		return
	}
	j.last = time.Now()
	for i := range j.outs {
		if j.outs[i].Status == mediaPending {
			j.outs[i].Note = fmt.Sprintf(format, args...)
			j.r.pub.media(j.outs[i])
		}
	}
}

// progress notes "label · frame n of total · p%".
func (j *toolJob) progress(label string, n, total int) {
	j.note(false, "%s · frame %d of %d · %d%%", label, n, max(total, n), min(99, n*100/max(total, 1)))
}

// work is a scratch file in the cache, removed when the job ends.
func (j *toolJob) work(ext string) string {
	p := filepath.Join(j.r.frames.cache, "work-"+newID()+ext)
	j.temp = append(j.temp, p)
	return p
}

func (j *toolJob) cleanup() {
	for _, p := range j.temp {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Printf("utsushi: remove %s: %v", p, err)
		}
	}
}

// keep stores the file at path as output i and marks it ready.
func (j *toolJob) keep(i int, path string) error {
	if j.outs[i].ID == "" {
		// A retry remakes one item; the order's others are not wanted.
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	stored, err := j.r.lib.store(f, originTool, j.outs[i].Label, 0)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if stored.Kind != j.outs[i].Kind {
		return errors.Join(fmt.Errorf("%s made a %s, not a %s", j.tool.Name, stored.Kind, j.outs[i].Kind), j.r.lib.remove(stored))
	}
	done := j.outs[i]
	done.File, done.ContentType, done.Size, done.Seconds = stored.File, stored.ContentType, stored.Size, stored.Seconds
	done.Width, done.Height = stored.Width, stored.Height
	done.Status, done.Note = mediaReady, ""
	j.outs[i] = done
	j.finish(done)
	return nil
}

func (j *toolJob) finish(m Media) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := j.r.st.finishMedia(ctx, m); err != nil {
		log.Printf("utsushi: save %s %s: %v", j.tool.ID, m.ID, err)
	}
	j.r.pub.media(m)
}

// models downloads the runtime for the device and whatever model files
// are missing, then starts ONNX Runtime.
func (j *toolJob) models(files ...modelFile) error {
	kit := j.r.tools
	files = append(runtimeFiles(kit.device), files...)
	if kit.models.missing(files...) > 0 {
		what := "the model"
		if kit.models.missing(runtimeFiles(kit.device)...) > 0 && kit.device.GPU {
			what = "the GPU runtime and the model"
		}
		j.note(true, "Downloading %s", what)
		err := kit.models.ensure(j.ctx, func(done, total int64) {
			j.note(false, "Downloading %s · %d MB of %d · %d%%", what, done>>20, total>>20, done*100/max(total, 1))
		}, files...)
		if err != nil {
			return err
		}
	}
	return startORT(kit.models, kit.device)
}

// video is the source's frame rate and about how many frames it has, for
// progress; an image is one frame at rate 1.
func (j *toolJob) video() (rate string, frames int, err error) {
	if j.order.Source.Kind == kindImage {
		return "1", 1, nil
	}
	if rate, err = j.r.frames.rate(j.ctx, j.src()); err != nil {
		return "", 0, err
	}
	return rate, max(1, int(j.order.Source.Seconds*rateValue(rate)+0.5)), nil
}

// outSize is the source's size, made even for a video, since H.264 needs
// even sides.
func (j *toolJob) outSize() (int, int) {
	w, h := j.order.Source.Width, j.order.Source.Height
	if j.order.Source.Kind == kindVideo {
		return w &^ 1, h &^ 1
	}
	return w, h
}

// ext is the output file's extension for the source's kind.
func (j *toolJob) ext() string {
	if j.order.Source.Kind == kindImage {
		return ".png"
	}
	return ".mp4"
}

// formPick picks the values of key that appear in allowed, in allowed's
// order.
func formPick(form url.Values, key string, allowed []string) []string {
	var out []string
	for _, v := range allowed {
		if slices.Contains(form[key], v) {
			out = append(out, v)
		}
	}
	return out
}

// formChoice is form[key] when it is one of allowed, else the first.
func formChoice(form url.Values, key string, allowed []string) string {
	if v := form.Get(key); slices.Contains(allowed, v) {
		return v
	}
	return allowed[0]
}
