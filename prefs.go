package main

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"

	"github.com/pelletier/go-toml/v2"
)

// Prefs are the choices made in Settings, saved as TOML in
// ~/.config/utsushi/settings.toml (or $XDG_CONFIG_HOME, or UTSUSHI_CONFIG).
// Keys live in the same file, which is written 0600 in a 0700 directory: the
// owner chose a settings file over the system keychain.
type Prefs struct {
	Server  ServerPrefs  `toml:"server"`
	LLM     LLMPrefs     `toml:"llm"`
	Engines EnginePrefs  `toml:"engines"`
	Spend   SpendPrefs   `toml:"spending"`
	Storage StoragePrefs `toml:"storage"`
	About   AboutPrefs   `toml:"about"`
}

// ServerPrefs choose the ComfyUI the managed proxy fronts, and hold the
// Comfy API key for billed engines.
type ServerPrefs struct {
	Mode      string `toml:"mode"` // "local" or "remote"
	LocalURL  string `toml:"local_url"`
	RemoteURL string `toml:"remote_url"`
	Token     string `toml:"token,omitempty"`
	ComfyKey  string `toml:"comfy_api_key,omitempty"`
}

// URL is the ComfyUI address for the chosen mode.
func (s ServerPrefs) URL() string {
	if s.Mode == "remote" {
		return s.RemoteURL
	}
	return s.LocalURL
}

// LLMPrefs configure the optional language model. URLs, keys, and models
// are kept per provider, so switching back finds what was entered.
type LLMPrefs struct {
	On       bool              `toml:"on"`
	Provider string            `toml:"provider"`
	URLs     map[string]string `toml:"urls,omitempty"`
	Keys     map[string]string `toml:"keys,omitempty"`
	Models   map[string]string `toml:"models,omitempty"`
	Improve  bool              `toml:"improve_prompts"`
	Describe bool              `toml:"describe_references"`
	Suggest  bool              `toml:"suggest_from_clip"`
}

// EnginePrefs choose what the picker shows and what new runs start with.
type EnginePrefs struct {
	Off         []string          `toml:"off,omitempty"`     // engine ids hidden from the picker
	Default     map[string]string `toml:"default,omitempty"` // mode -> engine id
	HideUnready bool              `toml:"hide_unready"`
	Resolution  string            `toml:"resolution"` // "480", "720", or "1080"
	Parallel    int               `toml:"parallel"`   // local runs at the same time
	Notify      bool              `toml:"notify"`
	// OpenImages is where open models run, image and template video alike:
	// "comfyui" (the server behind the proxy, free) or "cloud" (Comfy Cloud,
	// billed GPU time). The key keeps its first name.
	OpenImages string `toml:"open_images,omitempty"`
}

// SpendPrefs are the guardrails for billed engines.
type SpendPrefs struct {
	Estimates   bool    `toml:"show_estimates"`
	ConfirmOver string  `toml:"confirm_over"` // "1", "2", "5", or "never"
	CapOn       bool    `toml:"monthly_cap"`
	Cap         float64 `toml:"cap_usd"`
	CapAction   string  `toml:"cap_action"` // "ask" or "block"
}

// StoragePrefs say where finished results are copied and how long they stay.
type StoragePrefs struct {
	OutDir      string `toml:"output_dir"`
	NamePattern string `toml:"name_pattern"`
	KeepSources bool   `toml:"keep_sources"`
	CleanDays   int    `toml:"delete_after_days"` // 0 keeps results
}

// AboutPrefs hold the crash report choice. Reports are always written
// locally; with an endpoint set and sending on, they are also posted there.
type AboutPrefs struct {
	CrashReports  bool   `toml:"send_crash_reports"`
	CrashEndpoint string `toml:"crash_endpoint,omitempty"`
}

func defaultPrefs() Prefs {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return Prefs{
		Server: ServerPrefs{Mode: "local", LocalURL: "http://127.0.0.1:8188"},
		LLM:    LLMPrefs{Provider: "ollama", Improve: true, Describe: true},
		Engines: EnginePrefs{
			Resolution: "720", Parallel: 1, Notify: true,
		},
		Spend:   SpendPrefs{Estimates: true, ConfirmOver: "2", Cap: 25, CapAction: "ask"},
		Storage: StoragePrefs{OutDir: filepath.Join(home, "Videos", "Utsushi"), NamePattern: "{date}_{engine}_{v}", KeepSources: true},
	}
}

// prefsStore loads and saves Prefs, and hands out copies.
type prefsStore struct {
	path string

	mu    sync.RWMutex
	prefs Prefs
}

// prefsPath is where Settings live.
func prefsPath() (string, error) {
	if p := os.Getenv("UTSUSHI_CONFIG"); p != "" {
		return p, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "utsushi", "settings.toml"), nil
}

// openPrefs reads the settings file over the defaults. A missing file is
// the defaults; an unreadable or invalid one is an error, so a typo is never
// silently replaced by defaults on the next save.
func openPrefs(path string) (*prefsStore, error) {
	p := defaultPrefs()
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		if err := toml.NewDecoder(bytes.NewReader(data)).DisallowUnknownFields().Decode(&p); err != nil {
			return nil, fmt.Errorf("settings %s: %w", path, err)
		}
	}
	return &prefsStore{path: path, prefs: p}, nil
}

func (s *prefsStore) get() Prefs {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.prefs.clone()
}

// update applies change to a copy and saves it; the stored prefs change only
// once the file is written.
func (s *prefsStore) update(change func(*Prefs)) (Prefs, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.prefs.clone()
	change(&next)
	if err := writePrefs(s.path, next); err != nil {
		return s.prefs.clone(), err
	}
	s.prefs = next
	return next.clone(), nil
}

func writePrefs(path string, p Prefs) (err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := toml.Marshal(p)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".settings-*.toml")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			if rerr := os.Remove(tmp.Name()); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
				err = errors.Join(err, rerr)
			}
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return errors.Join(err, tmp.Close())
	}
	if _, err := tmp.Write(data); err != nil {
		return errors.Join(err, tmp.Close())
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// clone copies the maps and slices, so a caller's edits never reach the
// stored prefs.
func (p Prefs) clone() Prefs {
	c := p
	c.LLM.URLs = cloneMap(p.LLM.URLs)
	c.LLM.Keys = cloneMap(p.LLM.Keys)
	c.LLM.Models = cloneMap(p.LLM.Models)
	c.Engines.Default = cloneMap(p.Engines.Default)
	c.Engines.Off = append([]string(nil), p.Engines.Off...)
	return c
}

func cloneMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
