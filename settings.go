package main

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/regiellis/comfy-api-proxy-go/sdk"
)

// settings is what the app reads from its environment. HOST, PORT, and
// ENVIRONMENT belong to hyperchi's config.FromEnv and are not repeated here.
type settings struct {
	// DataDir holds the SQLite file and every stored media file.
	DataDir string
	// APIKey is the Comfy API key for Comfy Cloud assets and Comfy Router
	// models. Empty leaves every billed engine offline.
	APIKey string
	// CloudURL is the Comfy Cloud v2 deployment uploads go to.
	CloudURL string
	// ProxyURL is the local comfy-api-proxy that fronts ComfyUI, which runs
	// Wan Animate 2 and reference images on this machine.
	ProxyURL string
	// ProxyToken is the proxy's bearer token, when it was started with one.
	ProxyToken string
	// AnimateWorkflow is the API-format export of ComfyUI's Wan Animate 2
	// template. Its binding file sits beside it (see workflowEngine).
	AnimateWorkflow string
}

// loadSettings reads the environment, then removes COMFY_API_KEY and
// COMFY_BASE_URL from it. The SDK falls back to both when an option is blank,
// so leaving them set would send the Cloud key to the local proxy. Every
// client here passes its target and key explicitly instead.
func loadSettings() (settings, error) {
	s := settings{
		DataDir:         envOr("UTSUSHI_DATA", "data"),
		APIKey:          strings.TrimSpace(os.Getenv(sdk.APIKeyEnv)),
		CloudURL:        envOr("COMFY_CLOUD_URL", sdk.CloudBaseURL),
		ProxyURL:        envOr("UTSUSHI_PROXY_URL", "http://127.0.0.1:8189"),
		ProxyToken:      strings.TrimSpace(os.Getenv("UTSUSHI_PROXY_TOKEN")),
		AnimateWorkflow: envOr("UTSUSHI_ANIMATE_WORKFLOW", filepath.Join("workflows", "wan-animate2.api.json")),
	}
	for _, k := range []string{sdk.APIKeyEnv, sdk.BaseURLEnv} {
		if err := os.Unsetenv(k); err != nil {
			return settings{}, err
		}
	}
	return s, nil
}

func (s settings) mediaDir() string { return filepath.Join(s.DataDir, "media") }

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}
