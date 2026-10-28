// Utsushi is an example studio built on this repo's Go SDK and proxy, in the
// shape of Higgsfield's Genjutsu: give it a source clip and references, and
// it recasts the people (Motion Transfer) or replaces an object (Object
// Swap). Billed models run on Comfy Router and a Wan 3.0 partner node; Wan
// Animate 2 and reference images run free on the local GPU through
// comfy-api-proxy.
package main

import (
	"context"
	"embed"
	"errors"
	"html/template"
	"io/fs"
	"log"
	"path/filepath"
	"time"

	"github.com/regiellis/hyperchi/hyperchi"
	"github.com/regiellis/hyperchi/hyperchi/config"
)

//go:embed templates
var templateFiles embed.FS

//go:embed static
var staticFiles embed.FS

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	s, err := loadSettings()
	if err != nil {
		return err
	}
	cfg := config.FromEnv()
	cfg.Server.PortFallback = true
	cfg.Database.Name = filepath.Join(s.DataDir, "utsushi.db")
	app, err := hyperchi.NewWithConfigE(cfg)
	if err != nil {
		return err
	}
	defer func() {
		if err := app.Close(); err != nil {
			log.Printf("close: %v", err)
		}
	}()
	if app.DB == nil {
		return errors.New("the database did not open")
	}
	app.AutoMiddleware()
	// chi takes middleware only before the first route, so the crash
	// recorder goes in now; Settings are attached once they load.
	crashes, err := newCrashes(filepath.Join(s.DataDir, "crash"), nil)
	if err != nil {
		return err
	}
	app.Use(crashes.middleware)
	app.Extensions().EnableExtensionAssets()
	app.Extensions().EnableSSE("/sse")
	// ffmpeg, when the server has it, makes poster and scrub previews.
	fr, err := newFrames(filepath.Join(s.DataDir, "cache"))
	if err != nil {
		return err
	}
	app.RegisterFilters(template.FuncMap{"posters": fr.available})
	if err := app.LoadTemplatesFS(templateFiles, "templates/**/*.tmpl"); err != nil {
		return err
	}
	static, err := fs.Sub(staticFiles, "static")
	if err != nil {
		return err
	}
	if err := app.StaticFS("/static/*", static, hyperchi.StaticOptions{}); err != nil {
		return err
	}

	st, err := newStore(app.DB)
	if err != nil {
		return err
	}
	lib, err := openLibrary(s.mediaDir())
	if err != nil {
		return err
	}
	defer func() {
		if err := lib.Close(); err != nil {
			log.Printf("close library: %v", err)
		}
	}()

	// Settings (~/.config/utsushi/settings.toml) choose the ComfyUI server
	// and hold the Comfy API key. A key saved there wins; COMFY_API_KEY is
	// the fallback until one is.
	path, err := prefsPath()
	if err != nil {
		return err
	}
	prefs, err := openPrefs(path)
	if err != nil {
		return err
	}
	key := prefs.get().Server.ComfyKey
	if key == "" {
		key = s.APIKey
	}
	// Comfy Cloud carries uploads for the Router models, and its client
	// also reaches Comfy Router, so one key serves every billed engine.
	cloud, err := newCloudLink(s.CloudURL, key)
	if err != nil {
		return err
	}
	if key == "" {
		log.Print("utsushi: no Comfy API key yet; billed engines are hidden until one is added in Settings")
	}

	// Utsushi runs the proxy itself, fronting the ComfyUI chosen in
	// Settings, unless UTSUSHI_PROXY_URL points somewhere else.
	var pm *proxyManager
	if s.ProxyURL == proxyAddr {
		bin, err := findProxyBinary()
		if err != nil {
			return err
		}
		pm = newProxyManager(bin)
		sp := prefs.get().Server
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		err = pm.start(ctx, sp.URL(), sp.Token)
		cancel()
		if err != nil {
			log.Printf("utsushi: start the proxy for %s: %v", sp.URL(), err)
		}
		defer func() {
			if err := pm.shutdown(); err != nil {
				log.Printf("utsushi: stop the proxy: %v", err)
			}
		}()
	}

	refs, err := newLocalRefs(s.ProxyURL, s.ProxyToken)
	if err != nil {
		return err
	}
	gpu := newGPUNames(s.ProxyURL)
	refs.models = newModelIndex(gpu)
	assets := &cloudAssets{client: cloud.cloud, lib: lib}
	genjutsu := newGenjutsuEngine(cloud.models, assets)
	wan3 := newWan3Engine(refs, lib, cloud.apiKey)
	aleph := newAlephEngine(cloud.models, assets)
	wanEdit := newWanEditEngine(cloud.models, assets)
	seedance, seedanceEdit := newSeedanceEngines(cloud, lib)
	// Open models run where Settings, Engines say: the ComfyUI behind the
	// proxy, or Comfy Cloud.
	openWhere := func() string { return openImages(prefs.get()) }
	eng := &engines{list: append([]engine{
		genjutsu,
		newAnimateEngine(s.AnimateWorkflow, refs, lib).onCloud(cloud, openWhere),
		wan3,
		seedance,
		newMiniMaxH3Engine(cloud, lib, false),
		newMiniMaxH3Engine(cloud, lib, true),
		aleph,
		wanEdit,
		seedanceEdit,
		newMiniMaxH3LocalEngine(refs, lib).onCloud(cloud, openWhere),
		newMiniMaxH3ControlEngine(refs, lib).onCloud(cloud, openWhere),
		newLTXControlEngine(refs, lib).onCloud(cloud, openWhere),
		newLongEngine(refs, lib, cloud, openWhere, fr),
	}, styleEngines(genjutsu, wan3, wanEdit, aleph, seedanceEdit, newLTXStyleBase(refs, lib).onCloud(cloud, openWhere))...)}

	crashes.prefs = prefs

	pub := &webPublisher{}
	rn := newRunner(st, lib, eng, refs, pub)
	rn.guard = crashes.guard
	rn.cloud = cloud
	rn.frames = fr
	rn.tools = &toolKit{models: newModels(filepath.Join(s.DataDir, "models")), device: findDevice()}
	log.Printf("utsushi: model tools run on %s", rn.tools.device.Name)
	rn.local.setLimit(prefs.get().Engines.Parallel)
	defer rn.shutdown()
	w := &web{app: app, st: st, lib: lib, eng: eng, run: rn, sse: app.Extensions().GetSSEHub(),
		gpu: gpu, cloud: cloud, prefs: prefs, proxy: pm, envKey: s.APIKey, frames: fr, crash: crashes}
	rn.succeeded = w.exportResult
	pub.w = w
	w.routes()

	app.SetGlobal("proxyURL", s.ProxyURL)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := rn.recover(ctx); err != nil {
		return err
	}
	if err := backfillSizes(ctx, st, lib); err != nil {
		return err
	}
	janitor, stopJanitor := context.WithCancel(context.Background())
	defer stopJanitor()
	go crashes.guard("storage cleanup", func() { w.cleanup(janitor) })
	return app.Serve("")
}

// backfillSizes measures media stored before sizes were recorded, once.
func backfillSizes(ctx context.Context, st *store, lib *library) error {
	list, err := st.unsized(ctx)
	if err != nil {
		return err
	}
	for _, m := range list {
		if w, h := lib.size(m); w > 0 {
			if err := st.setSize(ctx, m.ID, w, h); err != nil {
				return err
			}
		}
	}
	return nil
}
