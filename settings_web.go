package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/regiellis/comfy-api-proxy-go/sdk"
	"github.com/regiellis/hyperchi/hyperchi"
)

// settingsSections are the Settings tabs, in order.
var settingsSections = []struct{ ID, Label, Desc string }{
	{"server", "ComfyUI server", "Where your workflows run. Utsushi runs the proxy and points it at any ComfyUI you can reach."},
	{"llm", "Language model", "Optional. A language model can write prompts and cast descriptions. Any OpenAI-compatible endpoint works, and local servers need no key."},
	{"engines", "Engines & queue", "Choose which engines appear in the picker and what new runs start with."},
	{"spending", "Spending", "Guardrails for billed engines. Local runs are never counted."},
	{"storage", "Storage", "Where results are copied on disk and how long they stay."},
	{"about", "About", "Utsushi is the example app for the ComfyUI Go SDK."},
}

func (w *web) settingsRoutes() {
	app := w.app
	app.Get("/settings", w.settingsPage)
	app.Post("/settings/server/test", w.settingsServerTest)
	app.Post("/settings/server", w.settingsServerSave)
	app.Post("/settings/key", w.settingsKeySave)
	app.Post("/settings/key/remove", w.settingsKeyRemove)
	app.Post("/settings/llm/test", w.settingsLLMTest)
	app.Post("/settings/llm/models", w.settingsLLMModels)
	app.Post("/settings/llm", w.settingsLLMSave)
	app.Post("/settings/engines", w.settingsEnginesSave)
	app.Post("/settings/spending", w.settingsSpendSave)
	app.Post("/settings/storage", w.settingsStorageSave)
	app.Post("/settings/storage/purge", w.settingsStoragePurge)
	app.Post("/settings/cache/clear", w.settingsCacheClear)
	app.Post("/settings/about", w.settingsAboutSave)
	app.Get("/diagnostics", w.diagnostics)
}

// settingsPage renders the Settings modal at one tab.
func (w *web) settingsPage(c *hyperchi.Context) error {
	data, err := w.settingsData(c.Ctx(), c.Query("sec"), c.Query("provider"))
	if err != nil {
		return err
	}
	return c.Fragment("settings", data)
}

func (w *web) renderSettings(c *hyperchi.Context, sec, flash string) error {
	data, err := w.settingsData(c.Ctx(), sec, "")
	if err != nil {
		return err
	}
	data["flash"] = flash
	return c.Fragment("settings", data)
}

type navItem struct {
	ID, Label, Status string
	OK, Current       bool
}

// settingsData is everything the modal shows for one tab.
// provider, when set, previews another language-model provider's fields
// before it is saved.
func (w *web) settingsData(ctx context.Context, sec, provider string) (hyperchi.H, error) {
	if !slices.ContainsFunc(settingsSections, func(s struct{ ID, Label, Desc string }) bool { return s.ID == sec }) {
		sec = "server"
	}
	p := w.prefs.get()
	spent, err := w.monthSpend(ctx)
	if err != nil {
		return nil, err
	}
	librarySize, err := dirSize(w.lib.dir)
	if err != nil {
		librarySize = 0
	}
	proxyUp := w.run.refs.ready(ctx) == nil
	nav := make([]navItem, 0, len(settingsSections))
	var title, desc string
	for _, s := range settingsSections {
		n := navItem{ID: s.ID, Label: s.Label, Current: s.ID == sec}
		switch s.ID {
		case "server":
			n.Status, n.OK = "Offline", proxyUp
			if proxyUp {
				n.Status = "Connected"
			}
		case "llm":
			n.Status = "Off"
			if p.LLM.On {
				n.Status = providerByID(p.LLM.Provider).Name
			}
		case "engines":
			on := 0
			for _, en := range w.eng.choices() {
				if !slices.Contains(p.Engines.Off, en.info().ID) && !slices.Contains(p.Engines.Off, engineModelID(en.info().ID)) {
					on++
				}
			}
			n.Status = fmt.Sprintf("%d of %d", on, len(w.eng.choices()))
		case "spending":
			n.Status = "No cap"
			if p.Spend.CapOn {
				n.Status = fmt.Sprintf("$%.2f / $%.0f", spent, p.Spend.Cap)
			}
		case "storage":
			n.Status = humanBytes(librarySize)
		case "about":
			n.Status = buildVersion()
		}
		if n.Current {
			title, desc = s.Label, s.Desc
		}
		nav = append(nav, n)
	}
	data := hyperchi.H{
		"sec": sec, "nav": nav, "title": title, "desc": desc, "prefsPath": w.prefs.path,
		"version": buildVersion(), "sdk": sdkVersion(),
	}
	switch sec {
	case "server":
		data["server"] = w.serverSection(ctx, p)
	case "llm":
		if provider != "" {
			p.LLM.Provider = providerByID(provider).ID
		}
		data["llm"] = llmSection(p)
	case "engines":
		data["engines"] = w.enginesSection(p)
	case "spending":
		data["spend"] = spendSection(p, spent)
	case "storage":
		data["storage"] = w.storageSection(ctx, p, librarySize)
	case "about":
		data["about"] = w.aboutSection(ctx, p)
	}
	return data, nil
}

// --- Server ---

type serverSection struct {
	Mode, LocalURL, RemoteURL string
	TokenSet, Managed         bool
	ProxyNote                 string
	Report                    serverReport
	KeySet, KeyFromEnv        bool
	KeyMasked                 string
}

func (w *web) serverSection(ctx context.Context, p Prefs) serverSection {
	s := serverSection{Mode: p.Server.Mode, LocalURL: p.Server.LocalURL, RemoteURL: p.Server.RemoteURL, TokenSet: p.Server.Token != ""}
	if w.proxy == nil {
		s.ProxyNote = "UTSUSHI_PROXY_URL points at a proxy run elsewhere, so this tab can test servers but not switch them."
	} else if managed, _, _ := w.proxy.status(); !managed {
		s.ProxyNote = "A proxy you started yourself is on 127.0.0.1:8189. Stop it and restart Utsushi to switch servers here."
	} else {
		s.Managed = true
	}
	s.Report = w.probeServer(ctx, p.Server.URL(), p.Server.Token)
	key := w.cloud.apiKey()
	s.KeySet, s.KeyMasked = key != "", masked(key)
	s.KeyFromEnv = key != "" && p.Server.ComfyKey == "" && key == w.envKey
	return s
}

// probeServer tests a ComfyUI and checks what each feature needs there.
func (w *web) probeServer(ctx context.Context, base, token string) serverReport {
	probe, err := newServerProbe(base, token)
	if err != nil {
		return serverReport{Err: err.Error()}
	}
	features := []probeFeature{
		{Name: "Genjutsu, Aleph 2, Wan 2.7 Edit", Need: "Comfy Router · needs a Comfy API key", NeedsKey: true},
	}
	if wf, err := sdk.WorkflowFromFile(w.animatePath()); err == nil {
		features = append(features, probeFeature{Name: "Wan Animate 2", Need: "Wan Animate 2 models and loop nodes", Graph: wf})
	}
	if wf, err := sdk.WorkflowFromJSON([]byte(wan3NodeGraph)); err == nil {
		features = append(features, probeFeature{Name: "Wan 3.0 Reference to Video", Need: "partner node · needs a Comfy API key", Graph: wf, NeedsKey: true})
	}
	if wf, err := sdk.WorkflowFromJSON([]byte(restyleGraph)); err == nil {
		features = append(features, probeFeature{Name: "Style frames", Need: "Qwen Image 2.1 Edit", Graph: wf})
	}
	if wf, err := sdk.WorkflowFromJSON([]byte(zimageGraph)); err == nil {
		features = append(features, probeFeature{Name: "Create tile and style swatches", Need: "Z-Image Turbo", Graph: wf})
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	return probe.probe(ctx, features, w.cloud.apiKey() != "")
}

// wan3NodeGraph is the partner node alone, for checking a server has it.
const wan3NodeGraph = `{"1": {"class_type": "Wan3ReferenceToVideoApi", "inputs": {}}}`

func (w *web) animatePath() string {
	for _, en := range w.eng.choices() {
		if wf, ok := en.(*workflowEngine); ok {
			return wf.path
		}
	}
	return ""
}

type serverForm struct {
	Mode       string `form:"mode"`
	LocalURL   string `form:"local_url"`
	RemoteURL  string `form:"remote_url"`
	Token      string `form:"token"`
	ClearToken bool   `form:"clear_token"`
}

func (f serverForm) apply(p ServerPrefs) ServerPrefs {
	if f.Mode == "remote" || f.Mode == "local" {
		p.Mode = f.Mode
	}
	p.LocalURL = strings.TrimSpace(f.LocalURL)
	p.RemoteURL = strings.TrimSpace(f.RemoteURL)
	switch {
	case f.ClearToken:
		p.Token = ""
	case strings.TrimSpace(f.Token) != "":
		p.Token = strings.TrimSpace(f.Token)
	}
	return p
}

func (w *web) settingsServerTest(c *hyperchi.Context) error {
	in, err := hyperchi.Bind[serverForm](c)
	if err != nil {
		return err
	}
	sp := in.apply(w.prefs.get().Server)
	return c.Fragment("server-report", w.probeServer(c.Ctx(), sp.URL(), sp.Token))
}

// settingsServerSave saves the server only after it answers, then moves
// the managed proxy to it.
func (w *web) settingsServerSave(c *hyperchi.Context) error {
	in, err := hyperchi.Bind[serverForm](c)
	if err != nil {
		return err
	}
	ctx := c.Ctx()
	sp := in.apply(w.prefs.get().Server)
	if rep := w.probeServer(ctx, sp.URL(), sp.Token); !rep.OK {
		return w.renderSettings(c, "server", "Not saved. "+rep.Err)
	}
	if _, err := w.prefs.update(func(p *Prefs) { p.Server = sp }); err != nil {
		return err
	}
	if w.proxy != nil {
		if managed, _, _ := w.proxy.status(); managed {
			if err := w.proxy.switchTo(ctx, sp.URL(), sp.Token); err != nil {
				return w.renderSettings(c, "server", "Saved, but the proxy did not restart: "+describe(err))
			}
		}
	}
	return w.renderSettings(c, "server", "Saved. Workflows now run on "+sp.URL()+".")
}

type keyForm struct {
	Key string `form:"key"`
}

func (w *web) settingsKeySave(c *hyperchi.Context) error {
	in, err := hyperchi.Bind[keyForm](c)
	if err != nil {
		return err
	}
	key := strings.TrimSpace(in.Key)
	if key == "" {
		return w.renderSettings(c, "server", "Paste a Comfy API key first.")
	}
	if err := w.cloud.set(key); err != nil {
		return err
	}
	if _, err := w.prefs.update(func(p *Prefs) { p.Server.ComfyKey = key }); err != nil {
		return err
	}
	return w.renderSettings(c, "server", "Comfy API key saved. Billed engines are back in the picker.")
}

func (w *web) settingsKeyRemove(c *hyperchi.Context) error {
	if _, err := w.prefs.update(func(p *Prefs) { p.Server.ComfyKey = "" }); err != nil {
		return err
	}
	// The environment key, when there is one, takes over again.
	if err := w.cloud.set(w.envKey); err != nil {
		return err
	}
	msg := "Comfy API key removed. Billed engines are hidden."
	if w.envKey != "" {
		msg = "Saved key removed. The key from COMFY_API_KEY is in use again."
	}
	return w.renderSettings(c, "server", msg)
}

// --- Language model ---

type provView struct {
	ID, Code, Name, Sub, Tag string
	Free, Selected           bool
}

type llmSectionView struct {
	On, Improve, Describe, Suggest bool
	Providers                      []provView
	URL, URLHint, Model            string
	KeyLabel, KeyHint              string
	KeySet, KeyRequired            bool
	Privacy                        string
}

func llmSection(p Prefs) llmSectionView {
	prov := providerByID(p.LLM.Provider)
	v := llmSectionView{
		On: p.LLM.On, Improve: p.LLM.Improve, Describe: p.LLM.Describe, Suggest: p.LLM.Suggest,
		URL: firstNonEmpty(p.LLM.URLs[prov.ID], prov.URL), URLHint: firstNonEmpty(prov.URL, "https://your-server/v1"),
		Model: firstNonEmpty(p.LLM.Models[prov.ID], prov.Model), KeySet: p.LLM.Keys[prov.ID] != "",
		KeyRequired: prov.KeyRequired, KeyHint: firstNonEmpty(prov.KeyHint, "not needed"),
	}
	v.KeyLabel = "API key · optional"
	if prov.KeyRequired {
		v.KeyLabel = "API key"
	}
	if prov.Local {
		v.Privacy = "Runs on your own server. Nothing leaves your network."
	} else {
		v.Privacy = "Prompts and reference images are sent to " + prov.Name + ". Footage never is, only single frames when you ask for a suggestion."
	}
	for _, pr := range llmProviders {
		tag := "no key needed"
		if pr.KeyRequired {
			tag = "key required"
		}
		v.Providers = append(v.Providers, provView{ID: pr.ID, Code: pr.Code, Name: pr.Name, Sub: pr.Sub, Tag: tag, Free: !pr.KeyRequired, Selected: pr.ID == prov.ID})
	}
	return v
}

type llmForm struct {
	On       bool   `form:"on"`
	Provider string `form:"provider"`
	URL      string `form:"url"`
	Key      string `form:"key"`
	ClearKey bool   `form:"clear_key"`
	Model    string `form:"model"`
	Improve  bool   `form:"improve"`
	Describe bool   `form:"describe"`
	Suggest  bool   `form:"suggest"`
}

func (f llmForm) apply(p LLMPrefs) LLMPrefs {
	prov := providerByID(f.Provider)
	p.On, p.Provider = f.On, prov.ID
	p.Improve, p.Describe, p.Suggest = f.Improve, f.Describe, f.Suggest
	set := func(m map[string]string, v string) map[string]string {
		if m == nil {
			m = map[string]string{}
		}
		if v = strings.TrimSpace(v); v == "" {
			delete(m, prov.ID)
		} else {
			m[prov.ID] = v
		}
		return m
	}
	p.URLs = set(p.URLs, f.URL)
	p.Models = set(p.Models, f.Model)
	switch {
	case f.ClearKey:
		p.Keys = set(p.Keys, "")
	case strings.TrimSpace(f.Key) != "":
		p.Keys = set(p.Keys, f.Key)
	}
	return p
}

func (w *web) settingsLLMTest(c *hyperchi.Context) error {
	in, err := hyperchi.Bind[llmForm](c)
	if err != nil {
		return err
	}
	lp := in.apply(w.prefs.get().LLM)
	lp.On = true
	client, err := newLLM(lp)
	if err != nil {
		return c.Fragment("llm-result", hyperchi.H{"ok": false, "text": describe(err)})
	}
	ctx, cancel := context.WithTimeout(c.Ctx(), 60*time.Second)
	defer cancel()
	txt, err := client.test(ctx)
	if err != nil {
		return c.Fragment("llm-result", hyperchi.H{"ok": false, "text": describe(err)})
	}
	return c.Fragment("llm-result", hyperchi.H{"ok": true, "text": txt})
}

func (w *web) settingsLLMModels(c *hyperchi.Context) error {
	in, err := hyperchi.Bind[llmForm](c)
	if err != nil {
		return err
	}
	lp := in.apply(w.prefs.get().LLM)
	lp.On = true
	client, err := newLLM(lp)
	if err != nil {
		return c.Fragment("llm-result", hyperchi.H{"ok": false, "text": describe(err)})
	}
	ctx, cancel := context.WithTimeout(c.Ctx(), 20*time.Second)
	defer cancel()
	ids, err := client.models(ctx)
	if err != nil {
		return c.Fragment("llm-result", hyperchi.H{"ok": false, "text": describe(err)})
	}
	return c.Fragment("llm-models", hyperchi.H{"models": ids})
}

func (w *web) settingsLLMSave(c *hyperchi.Context) error {
	in, err := hyperchi.Bind[llmForm](c)
	if err != nil {
		return err
	}
	if _, err := w.prefs.update(func(p *Prefs) { p.LLM = in.apply(p.LLM) }); err != nil {
		return err
	}
	return w.renderSettings(c, "llm", "Saved.")
}

// --- Engines ---

type engRow struct {
	ID, Code, Name, Via, Cost string
	Free, On, Default         bool
}

type engGroup struct {
	Mode, Label string
	Rows        []engRow
}

type enginesSectionView struct {
	Groups      []engGroup
	HideUnready bool
	Resolution  string
	Parallel    int
	Notify      bool
	OpenImages  string
}

func (w *web) enginesSection(p Prefs) enginesSectionView {
	v := enginesSectionView{HideUnready: p.Engines.HideUnready, Resolution: p.Engines.Resolution, Parallel: p.Engines.Parallel,
		Notify: p.Engines.Notify, OpenImages: openImages(p)}
	for _, mode := range []string{modeMotion, modeSwap, modeStyle} {
		g := engGroup{Mode: mode, Label: modeLabels[mode]}
		list := w.eng.forMode(mode)
		def := w.eng.preferredID(p.Engines.Default[mode], openImages(p))
		if def == "" && len(list) > 0 {
			def = list[0].info().ID
		}
		for _, en := range list {
			in := en.info()
			cost, free := costLabel(in)
			g.Rows = append(g.Rows, engRow{ID: in.ID, Code: in.Code, Name: in.Name, Via: in.Via, Cost: cost, Free: free,
				On: !slices.Contains(p.Engines.Off, in.ID) && !slices.Contains(p.Engines.Off, engineModelID(in.ID)), Default: in.ID == def})
		}
		v.Groups = append(v.Groups, g)
	}
	return v
}

// openImages is where open image models run, "comfyui" unless Settings say
// Comfy Cloud.
func openImages(p Prefs) string {
	if p.Engines.OpenImages == onCloud {
		return onCloud
	}
	return onComfyUI
}

func (w *web) settingsEnginesSave(c *hyperchi.Context) error {
	req := c.Request()
	if err := req.ParseForm(); err != nil {
		return err
	}
	var off []string
	defaults := map[string]string{}
	for _, en := range w.eng.choices() {
		in := en.info()
		if req.PostForm.Get("on."+in.ID) == "" {
			off = append(off, in.ID)
		}
	}
	for _, mode := range []string{modeMotion, modeSwap, modeStyle} {
		if id := req.PostForm.Get("default." + mode); id != "" {
			if en, ok := w.eng.get(id); ok && en.info().Mode == mode {
				defaults[mode] = id
			}
		}
	}
	parallel, err := strconv.Atoi(req.PostForm.Get("parallel"))
	if err != nil || !slices.Contains([]int{1, 2, 4}, parallel) {
		parallel = 1
	}
	res := req.PostForm.Get("resolution")
	if !slices.Contains([]string{"480", "720", "1080"}, res) {
		res = "720"
	}
	open := req.PostForm.Get("open_images")
	if open != onCloud {
		open = onComfyUI
	}
	if _, err := w.prefs.update(func(p *Prefs) {
		p.Engines.Off, p.Engines.Default = off, defaults
		p.Engines.OpenImages = open
		p.Engines.HideUnready = req.PostForm.Get("hide_unready") != ""
		p.Engines.Notify = req.PostForm.Get("notify") != ""
		p.Engines.Resolution, p.Engines.Parallel = res, parallel
	}); err != nil {
		return err
	}
	w.run.local.setLimit(parallel)
	return w.renderSettings(c, "engines", "Saved.")
}

// --- Spending ---

type spendSectionView struct {
	Estimates, CapOn       bool
	ConfirmOver, CapAction string
	Cap                    string
	Spent                  string
	Percent                int
	Over                   bool
}

func spendSection(p Prefs, spent float64) spendSectionView {
	v := spendSectionView{Estimates: p.Spend.Estimates, CapOn: p.Spend.CapOn, ConfirmOver: p.Spend.ConfirmOver,
		CapAction: p.Spend.CapAction, Cap: strconv.FormatFloat(p.Spend.Cap, 'f', -1, 64), Spent: fmt.Sprintf("$%.2f", spent)}
	if p.Spend.Cap > 0 {
		v.Percent = min(100, int(spent/p.Spend.Cap*100))
		v.Over = spent >= p.Spend.Cap
	}
	return v
}

// monthSpend is this month's estimated spend.
func (w *web) monthSpend(ctx context.Context) (float64, error) {
	now := time.Now()
	return w.st.spentSince(ctx, time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location()))
}

func (w *web) settingsSpendSave(c *hyperchi.Context) error {
	req := c.Request()
	if err := req.ParseForm(); err != nil {
		return err
	}
	confirm := req.PostForm.Get("confirm_over")
	if !slices.Contains([]string{"1", "2", "5", "never"}, confirm) {
		confirm = "2"
	}
	capUSD, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimPrefix(req.PostForm.Get("cap"), "$")), 64)
	if err != nil || capUSD < 0 {
		return w.renderSettings(c, "spending", "Not saved. The monthly cap must be a dollar amount.")
	}
	action := req.PostForm.Get("cap_action")
	if action != "block" {
		action = "ask"
	}
	if _, err := w.prefs.update(func(p *Prefs) {
		p.Spend.Estimates = req.PostForm.Get("estimates") != ""
		p.Spend.CapOn = req.PostForm.Get("cap_on") != ""
		p.Spend.ConfirmOver, p.Spend.Cap, p.Spend.CapAction = confirm, capUSD, action
	}); err != nil {
		return err
	}
	return w.renderSettings(c, "spending", "Saved.")
}

// --- Storage ---

type storageSectionView struct {
	OutDir, NamePattern, NameExample, Tokens string
	KeepSources                              bool
	CleanDays                                int
	LibrarySize, CacheSize, FFmpeg           string
	HasFFmpeg                                bool
}

func (w *web) storageSection(ctx context.Context, p Prefs, librarySize int64) storageSectionView {
	cache, err := w.frames.size()
	if err != nil {
		cache = 0
	}
	sample := Run{ID: "a1b2c3d4e5", Mode: modeMotion, Engine: "wan-animate2"}
	return storageSectionView{
		OutDir: p.Storage.OutDir, NamePattern: p.Storage.NamePattern, Tokens: nameTokens,
		NameExample: exportName(p.Storage.NamePattern, sample, "v1", time.Now()),
		KeepSources: p.Storage.KeepSources, CleanDays: p.Storage.CleanDays,
		LibrarySize: humanBytes(librarySize), CacheSize: humanBytes(cache),
		FFmpeg: w.frames.version(ctx), HasFFmpeg: w.frames.available(),
	}
}

func (w *web) settingsStorageSave(c *hyperchi.Context) error {
	req := c.Request()
	if err := req.ParseForm(); err != nil {
		return err
	}
	days, err := strconv.Atoi(req.PostForm.Get("clean_days"))
	if err != nil || !slices.Contains([]int{0, 30, 90}, days) {
		days = 0
	}
	out := strings.TrimSpace(req.PostForm.Get("out_dir"))
	if out != "" {
		dir, err := expandHome(out)
		if err != nil {
			return err
		}
		if err := checkWritable(dir); err != nil {
			return w.renderSettings(c, "storage", "Not saved. "+err.Error())
		}
	}
	if _, err := w.prefs.update(func(p *Prefs) {
		p.Storage.OutDir = out
		p.Storage.NamePattern = strings.TrimSpace(req.PostForm.Get("name_pattern"))
		p.Storage.KeepSources = req.PostForm.Get("keep_sources") != ""
		p.Storage.CleanDays = days
	}); err != nil {
		return err
	}
	if days > 0 {
		if err := w.cleanOnce(c.Ctx()); err != nil {
			log.Printf("utsushi: storage cleanup: %v", err)
		}
	}
	return w.renderSettings(c, "storage", "Saved.")
}

func (w *web) settingsCacheClear(c *hyperchi.Context) error {
	if err := w.frames.clear(); err != nil {
		return err
	}
	return w.renderSettings(c, "storage", "Preview cache cleared. Previews are made again as they are needed.")
}

// --- About ---

type aboutSectionView struct {
	Version, SDK, Go, ComfyUI, FFmpeg string
	CrashReports                      bool
	CrashEndpoint                     string
	CrashCount                        int
}

func (w *web) aboutSection(ctx context.Context, p Prefs) aboutSectionView {
	v := aboutSectionView{Version: buildVersion(), SDK: sdkVersion(), Go: runtime.Version(), FFmpeg: w.frames.version(ctx),
		CrashReports: p.About.CrashReports, CrashEndpoint: p.About.CrashEndpoint, CrashCount: w.crash.count()}
	v.ComfyUI = w.comfyVersion(ctx)
	return v
}

func (w *web) comfyVersion(ctx context.Context) string {
	up, err := w.gpu.upstream(ctx)
	if err != nil {
		return "unreachable"
	}
	var stats struct {
		System struct {
			Version string `json:"comfyui_version"`
		} `json:"system"`
	}
	if err := w.gpu.fetch(ctx, up+"/system_stats", &stats); err != nil || stats.System.Version == "" {
		return "unknown, at " + up
	}
	return stats.System.Version + " at " + up
}

func (w *web) settingsAboutSave(c *hyperchi.Context) error {
	req := c.Request()
	if err := req.ParseForm(); err != nil {
		return err
	}
	endpoint := strings.TrimSpace(req.PostForm.Get("crash_endpoint"))
	if endpoint != "" && !strings.HasPrefix(endpoint, "https://") && !strings.HasPrefix(endpoint, "http://") {
		return w.renderSettings(c, "about", "Not saved. The crash report endpoint must be an http or https URL.")
	}
	if _, err := w.prefs.update(func(p *Prefs) {
		p.About.CrashReports = req.PostForm.Get("crash_reports") != ""
		p.About.CrashEndpoint = endpoint
	}); err != nil {
		return err
	}
	return w.renderSettings(c, "about", "Saved.")
}

// diagnostics is a plain-text report for a bug report: versions, the server,
// engine readiness, and settings without keys.
func (w *web) diagnostics(c *hyperchi.Context) error {
	ctx := c.Ctx()
	p := w.prefs.get()
	p.Server.Token, p.Server.ComfyKey = redact(p.Server.Token), redact(p.Server.ComfyKey)
	for k, v := range p.LLM.Keys {
		p.LLM.Keys[k] = redact(v)
	}
	engines := map[string]string{}
	for _, en := range w.eng.choices() {
		state := "ready"
		if err := en.ready(ctx); err != nil {
			state = describe(err)
		}
		engines[en.info().ID] = state
	}
	report := map[string]any{
		"utsushi": buildVersion(), "sdk": sdkVersion(), "go": runtime.Version(), "os": runtime.GOOS + "/" + runtime.GOARCH,
		"comfyui": w.comfyVersion(ctx), "ffmpeg": w.frames.version(ctx), "comfy_api_key": w.cloud.apiKey() != "",
		"engines": engines, "settings": p, "crash_reports_on_disk": w.crash.count(),
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	c.Response().Header().Set("Content-Type", "text/plain; charset=utf-8")
	c.Response().WriteHeader(http.StatusOK)
	if _, err := c.Response().Write(data); err != nil {
		return nil // the client left; nothing more to send
	}
	return nil
}

func redact(s string) string {
	if s == "" {
		return ""
	}
	return "set"
}
