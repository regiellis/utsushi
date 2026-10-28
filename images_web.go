package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/regiellis/hyperchi/hyperchi"
)

func (w *web) imageRoutes() {
	w.app.Get("/images/new", w.imageDialog)
	w.app.Post("/images/preview", w.imagePreview)
	w.app.Post("/images", w.createImages)
}

// imageForm is the Create image dialog's fields.
type imageForm struct {
	Purpose    string   `form:"purpose"`
	Subject    string   `form:"subject"`
	Engine     string   `form:"engine"`
	Aspect     string   `form:"aspect"`
	Resolution string   `form:"resolution"`
	Count      string   `form:"count"`
	Refs       []string `form:"refs"`
	Prompt     string   `form:"prompt"`
	// Touched is set once the visitor edits the prompt, so the dialog stops
	// rewriting it from their choices.
	Touched string `form:"touched"`
}

type imageEngineView struct {
	Featured                                             string
	Advice                                               modelAdvice
	ID, Name, Code, Blurb, Where, Cost, RefRule, Offline string
	Free, Partner, Selected                              bool
	// Tier, Meter, and Note are the engine's tested rating for the purpose.
	Tier, Meter, Note string
}

type imageDialogView struct {
	Featured   []imageEngineView
	Hardware   string
	Purposes   []choiceView
	Aspects    []choiceView
	Counts     []choiceView
	Res        []choiceView
	Open       []imageEngineView
	Partners   []imageEngineView
	OpenWhere  string
	Refs       []mediaView
	RefsShown  bool
	RefRule    string
	RefCount   int
	Subject    string
	Hint       string
	Prompt     string
	Touched    bool
	Meta       string
	Blocked    string
	Confirm    string
	ConfirmOK  string
	Engine     string
	EngineName string
	// Best names the engines rated best for the purpose, from test runs.
	Best        []string
	PurposeNoun string
}

type choiceView struct {
	Value, Label string
	Selected     bool
	Hint         string
}

func (w *web) imageDialog(c *hyperchi.Context) error {
	v, err := w.imageView(c.Ctx(), imageForm{Purpose: c.Query("purpose")})
	if err != nil {
		return err
	}
	return c.Fragment("image-dialog", v)
}

// imagePreview re-renders what depends on the choices: the prompt (unless
// the visitor has edited it), the references, the resolution, and the
// Create button. The subject box is never swapped, so typing keeps focus.
func (w *web) imagePreview(c *hyperchi.Context) error {
	in, err := hyperchi.Bind[imageForm](c)
	if err != nil {
		return err
	}
	v, err := w.imageView(c.Ctx(), in)
	if err != nil {
		return err
	}
	return c.Fragment("image-preview", v)
}

// imageView works out the dialog for the current choices.
func (w *web) imageView(ctx context.Context, in imageForm) (imageDialogView, error) {
	p := w.prefs.get()
	open := openImages(p)
	hardware := w.gpu.hardwareSnapshot(ctx)
	v := imageDialogView{Hardware: hardware.summary(), Subject: in.Subject, OpenWhere: map[string]string{onComfyUI: "your ComfyUI", onCloud: "Comfy Cloud"}[open]}

	purpose := in.Purpose
	if !slices.ContainsFunc(imagePurposes, func(x struct{ Key, Name, Hint, Aspect string }) bool { return x.Key == purpose }) {
		purpose = imagePurposes[0].Key
	}
	aspect := in.Aspect
	for _, x := range imagePurposes {
		if x.Key == purpose {
			v.Hint = x.Hint
			if !knownAspect(aspect) {
				aspect = x.Aspect
			}
		}
		v.Purposes = append(v.Purposes, choiceView{Value: x.Key, Label: x.Name, Selected: x.Key == purpose, Hint: x.Hint})
	}
	for _, a := range imageAspects {
		v.Aspects = append(v.Aspects, choiceView{Value: a.Key, Label: a.Name + " " + a.Ratio, Selected: a.Key == aspect})
	}
	count, err := strconv.Atoi(in.Count)
	if err != nil || !slices.Contains([]int{1, 2, 4}, count) {
		count = 1
	}
	for _, n := range []int{1, 2, 4} {
		v.Counts = append(v.Counts, choiceView{Value: strconv.Itoa(n), Label: strconv.Itoa(n), Selected: n == count})
	}

	// Engines, each with where it runs, its price, and why it cannot run.
	chosen, ok := imageEngineByID(in.Engine)
	if ok && !chosen.Partner && chosen.target == "" {
		chosen, ok = imageEngineByID(chosen.ID + "@" + open)
	}
	key := w.cloud.apiKey()
	offline := map[string]string{}
	withRefs := len(in.Refs) > 0
	v.PurposeNoun = purposeNoun(purpose, false)
	ranked := imageChoices()
	byRating(ranked, func(e imageEngine) string { return imageRating(purpose, withRefs && e.MaxRefs > 0, e.ID).Tier })
	slices.SortStableFunc(ranked, func(a, b imageEngine) int {
		if (featuredModel(a.ID) != "") == (featuredModel(b.ID) != "") {
			return 0
		}
		if featuredModel(a.ID) != "" {
			return -1
		}
		return 1
	})
	firstReady := ""
	for _, e := range ranked {
		where := e.where(open)
		ev := imageEngineView{Featured: featuredModel(e.ID), ID: e.ID, Name: e.Name, Code: e.Code, Blurb: e.Blurb, Partner: e.Partner,
			Where: map[string]string{onComfyUI: "Your ComfyUI", onCloud: "Comfy Cloud"}[where], RefRule: imageRefRule(e)}
		ev.Cost, ev.Free = e.costLabel(where, "square", "1K", 0)
		r := imageRating(purpose, withRefs && e.MaxRefs > 0, e.ID)
		ev.Tier, ev.Meter, ev.Note = r.Tier, tierMeter(r.Tier), r.Note
		if where == onComfyUI {
			ev.Advice = localAdvice(e.ID, hardware)
		} else {
			var price float64
			if e.price != nil {
				res := in.Resolution
				if !slices.Contains(e.Resolutions, res) && len(e.Resolutions) > 0 {
					res = e.Resolutions[0]
				}
				_, price = e.price(aspect, res, len(in.Refs))
			}
			ev.Advice = cloudAdvice(price, r.Tier, "image")
		}
		if r.Tier == tierBest && !slices.Contains(v.Best, e.Name) {
			v.Best = append(v.Best, e.Name)
		}
		if err := e.readiness(ctx, where, key, w.run.refs); err != nil {
			ev.Offline = describe(err)
			offline[e.ID] = ev.Offline
		} else if firstReady == "" {
			firstReady = e.ID
		}
		if ev.Featured != "" {
			v.Featured = append(v.Featured, ev)
		} else if e.Partner {
			v.Partners = append(v.Partners, ev)
		} else {
			v.Open = append(v.Open, ev)
		}
	}
	if !ok {
		if chosen, ok = imageEngineByID(firstReady); !ok {
			chosen = ranked[0]
		}
	}
	for _, list := range [][]imageEngineView{v.Featured, v.Open, v.Partners} {
		for i := range list {
			list[i].Selected = list[i].ID == chosen.ID
		}
	}
	v.Engine, v.EngineName = chosen.ID, chosen.Name

	res := in.Resolution
	if !slices.Contains(chosen.Resolutions, res) && len(chosen.Resolutions) > 0 {
		res = chosen.Resolutions[0]
	}
	for _, r := range chosen.Resolutions {
		v.Res = append(v.Res, choiceView{Value: r, Label: r, Selected: r == res})
	}

	// References: library images, picked ones kept in pick order.
	v.RefsShown = chosen.MaxRefs > 0
	v.RefRule = imageRefRule(chosen)
	images, err := w.st.listMedia(ctx, kindImage, 120)
	if err != nil {
		return v, err
	}
	picked := []string{}
	if v.RefsShown {
		picked = in.Refs
	}
	for _, m := range images {
		mv := viewMedia(m)
		if !mv.Ready {
			continue
		}
		if i := slices.Index(picked, m.ID); i >= 0 {
			mv.Selected, mv.Tag = true, fmt.Sprintf("%d", i+1)
		}
		v.Refs = append(v.Refs, mv)
	}
	refs := 0
	for _, id := range picked {
		if slices.ContainsFunc(v.Refs, func(m mediaView) bool { return m.ID == id }) {
			refs++
		}
	}
	v.RefCount = refs

	// The prompt is written from the choices until the visitor edits it.
	v.Touched = in.Touched != "" && strings.TrimSpace(in.Prompt) != ""
	v.Prompt = in.Prompt
	if !v.Touched {
		v.Prompt = imagePrompt(chosen, purpose, in.Subject, refs)
	}

	// The button: count, price, or why it is blocked.
	where := chosen.where(open)
	lo, hi := chosen.cost(aspect, res, refs)
	n := map[bool]string{true: "image", false: "images"}[count == 1]
	switch {
	case offline[chosen.ID] != "":
		v.Blocked = "Engine not ready"
	case refs < chosen.MinRefs:
		v.Blocked = fmt.Sprintf("Pick %d reference%s", chosen.MinRefs, plural(chosen.MinRefs))
	case refs > chosen.MaxRefs && chosen.MaxRefs > 0:
		v.Blocked = fmt.Sprintf("Use at most %d references", chosen.MaxRefs)
	case strings.TrimSpace(v.Prompt) == "":
		v.Blocked = "Describe it first"
	case chosen.price != nil && hi > lo:
		v.Meta = fmt.Sprintf("%d %s · ≈ %s–%s", count, n, usd(lo*float64(count)), usd(hi*float64(count)))
	case chosen.price != nil:
		v.Meta = fmt.Sprintf("%d %s · ≈ %s", count, n, usd(lo*float64(count)))
	case where == onCloud:
		v.Meta = fmt.Sprintf("%d %s · billed by Comfy Cloud", count, n)
	default:
		v.Meta = fmt.Sprintf("%d %s · free", count, n)
	}
	// Ask first, as the studio does, over the threshold or with no
	// published price.
	if v.Blocked == "" && (chosen.Partner || where == onCloud) {
		threshold, err := strconv.ParseFloat(p.Spend.ConfirmOver, 64)
		over := err == nil && hi*float64(count) > threshold
		if chosen.price == nil || over {
			v.Confirm = fmt.Sprintf("Make %d %s with %s (%s)?", count, n, chosen.Name, strings.TrimPrefix(v.Meta, fmt.Sprintf("%d %s · ", count, n)))
			v.ConfirmOK = "Create"
		}
	}
	return v, nil
}

// usd shows a price to the cent, or to a tenth of a cent under ten cents,
// where two decimals would round a $0.045 image down to $0.04.
func usd(v float64) string {
	if v < 0.1 {
		return fmt.Sprintf("$%.3f", v)
	}
	return fmt.Sprintf("$%.2f", v)
}

// imageRefRule says how many references an image engine takes.
func imageRefRule(e imageEngine) string {
	switch {
	case e.MaxRefs == 0:
		return "Text only"
	case e.MinRefs > 0:
		return fmt.Sprintf("%d–%d references", e.MinRefs, e.MaxRefs)
	default:
		return fmt.Sprintf("Up to %d references · optional", e.MaxRefs)
	}
}

// createImages checks the dialog and starts the images. The new tiles go
// to the front of the reference grid.
func (w *web) createImages(c *hyperchi.Context) error {
	ctx := c.Ctx()
	in, err := hyperchi.Bind[imageForm](c)
	if err != nil {
		return err
	}
	fail := func(msg string) error {
		return c.Status(http.StatusUnprocessableEntity).HX().Retarget("#image-errors").Reswap("innerHTML").
			Fragment("notice", hyperchi.H{"message": msg})
	}
	v, err := w.imageView(ctx, in)
	if err != nil {
		return err
	}
	if v.Blocked != "" {
		return fail(v.Blocked + ".")
	}
	e, ok := imageEngineByID(v.Engine)
	if !ok {
		return fail("Pick an engine.")
	}
	p := w.prefs.get()
	order := imageOrder{Engine: e, Where: e.where(openImages(p)), Prompt: strings.TrimSpace(v.Prompt)}
	for _, a := range v.Aspects {
		if a.Selected {
			order.Aspect = a.Value
		}
	}
	for _, r := range v.Res {
		if r.Selected {
			order.Resolution = r.Value
		}
	}
	for _, n := range v.Counts {
		if n.Selected {
			order.Count, err = strconv.Atoi(n.Value)
			if err != nil {
				return err
			}
		}
	}
	if e.MaxRefs > 0 && len(in.Refs) > 0 {
		if order.Refs, err = w.st.mediaByIDs(ctx, in.Refs); errors.Is(err, errNotFound) {
			return fail("A picked reference is gone. Pick it again.")
		} else if err != nil {
			return err
		}
	}
	if e.ID == "flux3" {
		for _, ref := range order.Refs {
			if ref.Width < 256 || ref.Height < 256 {
				return fail("FLUX 3 references must be at least 256 × 256 pixels.")
			}
		}
	}
	for _, x := range imagePurposes {
		if x.Key == in.Purpose {
			order.Label = x.Name
		}
	}
	if s := strings.TrimSpace(in.Subject); s != "" {
		order.Label = strings.TrimSpace(order.Label + ": " + s)
	}
	order.Label = clip(cmp.Or(order.Label, order.Prompt), 120)

	if p.Spend.CapOn && p.Spend.CapAction == "block" && e.price != nil {
		_, hi := e.cost(order.Aspect, order.Resolution, len(order.Refs))
		spent, err := w.monthSpend(ctx)
		if err != nil {
			return err
		}
		if est := hi * float64(order.Count); spent+est > p.Spend.Cap {
			return fail(fmt.Sprintf("These images (≈ $%.2f) would pass your $%.0f monthly cap ($%.2f spent). Raise the cap in Settings, Spending.",
				est, p.Spend.Cap, spent))
		}
	}
	made, err := w.run.makeImages(ctx, order)
	var off errOffline
	if errors.As(err, &off) {
		return fail(off.reason)
	}
	if err != nil {
		return err
	}
	tiles := make([]mediaView, len(made))
	for i, m := range made {
		tiles[i] = viewMedia(m)
	}
	c.HX().Trigger("image-made")
	return c.Fragment("image-tiles", hyperchi.H{"tiles": tiles})
}
