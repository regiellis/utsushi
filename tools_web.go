package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/regiellis/hyperchi/hyperchi"
)

// The Tools page: a tool picker, the studio's own source pickers, the
// chosen tool's options, and a feed of everything made here.

func (w *web) toolsRoutes() {
	w.app.Get("/tools", w.toolsPage)
	w.app.Post("/tools/run", w.runToolHandler)
	w.app.Get("/tools/items/{id}/card", w.toolCard)
	w.app.Post("/tools/items/{id}/retry", w.retryToolItem)
}

// toolView is a tool as the picker shows it.
type toolView struct {
	ID, Name, Code, Blurb, Action string
	Takes                         string // "video image"
	Model, Current                bool
	// FirstUse is what the first run downloads, empty once it has.
	FirstUse string
}

func (w *web) toolViews(current string) []toolView {
	out := make([]toolView, 0, len(toolList))
	for _, t := range toolList {
		v := toolView{ID: t.ID, Name: t.Name, Code: t.Code, Blurb: t.Blurb, Action: t.Action,
			Takes: strings.Join(t.Takes, " "), Model: t.Model, Current: t.ID == current}
		if t.Model {
			kit := w.run.tools
			v.FirstUse = firstUseNote(kit.models.missing(append(runtimeFiles(kit.device), toolFiles(t)...)...))
		}
		out = append(out, v)
	}
	return out
}

// toolFiles are the model files a tool's first run downloads at most.
func toolFiles(t *tool) []modelFile {
	switch t.ID {
	case depthTool.ID:
		return depthFiles(depthFrame)
	}
	return modelFilesFor(t.ID)
}

// toolItemView is one made item's card.
type toolItemView struct {
	ID, URL, Label, Aspect, Meta, Note, Error, Created string
	SourceURL, SourceKind, Kind, ToolName, Shape       string
	Pending, Failed, Ready                             bool
	Percent                                            int
}

// notePercent reads the trailing "· 42%" off a progress note.
var notePercent = regexp.MustCompile(`· (\d+)%$`)

func (w *web) viewToolItem(ctx context.Context, m Media) toolItemView {
	mv := viewMedia(m)
	v := toolItemView{
		ID: m.ID, URL: mv.URL, Label: m.Label, Aspect: mv.Aspect, Meta: mv.Meta, Note: m.Note, Error: m.Error,
		Created: ago(m.CreatedAt), Pending: mv.Pending, Failed: mv.Failed, Ready: mv.Ready, Kind: m.Kind,
	}
	if t, ok := toolByID(m.Tool); ok {
		v.ToolName = t.Name
	}
	if p := notePercent.FindStringSubmatch(m.Note); p != nil {
		if n, err := strconv.Atoi(p[1]); err == nil {
			v.Percent = n
		}
	}
	if m.SourceID != "" {
		src, err := w.st.media(ctx, m.SourceID)
		switch {
		case err == nil && src.Status == mediaReady:
			v.SourceURL, v.SourceKind = "/media/"+src.ID, src.Kind
			if v.Aspect == "" && src.Width > 0 {
				v.Aspect = ratio(src.Width, src.Height)
			}
		case err != nil && !errors.Is(err, errNotFound):
			log.Printf("utsushi: %s source: %v", m.ID, err)
		}
	}
	switch {
	case m.Kind == kindAudio:
		v.Aspect = "1"
	case v.Aspect == "":
		v.Aspect = "0.5625"
	}
	v.Shape = shapeOf(v.Aspect)
	return v
}

func (w *web) toolItemViews(ctx context.Context, list []Media) []toolItemView {
	out := make([]toolItemView, 0, len(list))
	for _, m := range list {
		out = append(out, w.viewToolItem(ctx, m))
	}
	return out
}

func (w *web) toolsPage(c *hyperchi.Context) error {
	ctx := c.Ctx()
	t, ok := toolByID(c.Query("tool"))
	if !ok {
		t = toolList[0]
	}
	// The source is the studio's own clip block, so the clip picked there
	// is picked here too.
	clip, err := w.pageClip(ctx, "")
	if err != nil {
		return err
	}
	images, err := w.st.listMedia(ctx, kindImage, -1)
	if err != nil {
		return err
	}
	made, err := w.st.listOrigin(ctx, originTool, 80)
	if err != nil {
		return err
	}
	var offline string
	switch {
	case !w.frames.available() || w.frames.probe == "":
		offline = "Tools need ffmpeg and ffprobe on the server."
	case t.Model && modelPlatform() != nil:
		offline = describe(modelPlatform())
	}
	views := w.toolViews(t.ID)
	var current toolView
	for _, v := range views {
		if v.Current {
			current = v
		}
	}
	m := w.run.tools.models
	return c.View("tools", w.page(ctx, hyperchi.H{
		"embedded": storyEmbedded(c),
		"title":    t.Name + " · Tools · Utsushi",
		"nav":      "tools",
		"tool":     current,
		"tools":    views,
		"clip":     clip,
		"images":   w.mediaViews(images, false),
		"made":     w.toolItemViews(ctx, made),
		"offline":  offline,
		// Depth's options.
		"videoFirst":  firstUseNote(m.missing(append(runtimeFiles(w.run.tools.device), depthFiles(depthVideo)...)...)),
		"frameFirst":  firstUseNote(m.missing(append(runtimeFiles(w.run.tools.device), depthFiles(depthFrame)...)...)),
		"device":      w.run.tools.device,
		"lookChoices": depthLookChoices,
		// Conform's.
		"rates": conformRates[1:], "sizes": conformSizes[1:],
	}))
}

// firstUseNote says what a model's first run downloads.
func firstUseNote(bytes int64) string {
	if bytes == 0 {
		return ""
	}
	return fmt.Sprintf("downloads %d MB on first use", (bytes+500_000)/1_000_000)
}

type lookChoice struct{ Value, Name, Note string }

var depthLookChoices = []lookChoice{
	{lookDepth, "Depth", "near is white, as ControlNet and LTX read it"},
	{lookInverted, "Inverted", "near is black, for models that read far as white"},
	{lookNormals, "Normal map", "which way each surface faces, in RGB"},
}

// runToolHandler starts one tool order and adds its cards to the feed.
func (w *web) runToolHandler(c *hyperchi.Context) error {
	ctx := c.Ctx()
	if err := c.Request().ParseForm(); err != nil {
		return err
	}
	form := c.Request().Form
	problem := func(msg string) error {
		return c.Status(http.StatusUnprocessableEntity).Fragment("form-errors", hyperchi.H{"errors": []string{msg}})
	}
	t, ok := toolByID(form.Get("tool"))
	if !ok {
		return problem("Pick a tool.")
	}
	kind, id := kindVideo, form.Get("source")
	if form.Get("source_kind") == kindImage {
		kind, id = kindImage, form.Get("image")
	}
	src, err := w.st.media(ctx, id)
	if errors.Is(err, errNotFound) || (err == nil && (src.Status != mediaReady || src.Kind != kind)) {
		if kind == kindImage {
			return problem("Pick an image.")
		}
		return problem("Pick a clip.")
	}
	if err != nil {
		return err
	}
	made, err := w.run.makeTool(ctx, t, toolOrder{Source: src, Form: form})
	var f failure
	var off errOffline
	switch {
	case errors.As(err, &f):
		return problem(sentence(f.msg))
	case errors.As(err, &off):
		return problem(off.reason)
	case err != nil:
		return err
	}
	return c.Fragment("tools-started", hyperchi.H{"made": w.toolItemViews(ctx, made)})
}

// toolCard re-renders a card for a stream that reconnected.
func (w *web) toolCard(c *hyperchi.Context) error {
	m, err := w.st.media(c.Ctx(), c.Param("id"))
	if errors.Is(err, errNotFound) || (err == nil && m.Origin != originTool) {
		return hyperchi.NotFound("No such item.")
	}
	if err != nil {
		return err
	}
	return c.Fragment("tool-card", w.viewToolItem(c.Ctx(), m))
}

// retryToolItem makes a failed item again and answers with its card, now
// pending, in place of the failed one.
func (w *web) retryToolItem(c *hyperchi.Context) error {
	ctx := c.Ctx()
	m, err := w.st.media(ctx, c.Param("id"))
	if errors.Is(err, errNotFound) || (err == nil && m.Origin != originTool) {
		return hyperchi.NotFound("No such item.")
	}
	if err != nil {
		return err
	}
	if m.Status != mediaFailed {
		return c.HX().Trigger("toast", hyperchi.H{"message": "Only a failed item can be made again."}).NoContent()
	}
	again, err := w.run.retryTool(ctx, m)
	var f failure
	var off errOffline
	switch {
	case errors.As(err, &f):
		return c.HX().Trigger("toast", hyperchi.H{"message": sentence(f.msg)}).NoContent()
	case errors.As(err, &off):
		return c.HX().Trigger("toast", hyperchi.H{"message": off.reason}).NoContent()
	case err != nil:
		return err
	}
	return c.Fragment("tool-card", w.viewToolItem(ctx, again))
}

// publishToolItem is the card body pushed to an item's stream.
func (w *web) publishToolItem(m Media) (string, any) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return "tool-card-body", w.viewToolItem(ctx, m)
}
