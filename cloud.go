package main

import (
	"strings"
	"sync"

	"github.com/regiellis/comfy-api-proxy-go/sdk"
)

// cloudLink holds the Comfy API key and the client built from it, so
// Settings can add, replace, or remove the key while the app runs. Engines
// read it through its getters on every call, never keeping a client.
type cloudLink struct {
	baseURL string

	mu     sync.RWMutex
	key    string
	client *sdk.Client // nil without a key
}

func newCloudLink(baseURL, key string) (*cloudLink, error) {
	c := &cloudLink{baseURL: baseURL}
	return c, c.set(key)
}

// set swaps the key, building a client for it. An empty key removes it.
func (c *cloudLink) set(key string) error {
	key = strings.TrimSpace(key)
	var client *sdk.Client
	if key != "" {
		var err error
		client, err = sdk.New(sdk.WithBaseURL(c.baseURL), sdk.WithAPIKey(key), sdk.WithClientInfo("utsushi"))
		if err != nil {
			return err
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.key, c.client = key, client
	return nil
}

func (c *cloudLink) cloud() *sdk.Client {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.client
}

func (c *cloudLink) models() *sdk.Models {
	if cl := c.cloud(); cl != nil {
		return cl.Models
	}
	return nil
}

func (c *cloudLink) apiKey() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.key
}

// masked shows a key's shape for Settings: its prefix and last four.
func masked(key string) string {
	if len(key) <= 8 {
		return strings.Repeat("•", len(key))
	}
	prefix := ""
	if i := strings.IndexAny(key, "_-"); i > 0 && i < 8 {
		prefix = key[:i+1]
	}
	return prefix + "••••••••" + key[len(key)-4:]
}

// fixedModels and fixedKey adapt a value that never changes to the getters
// engines take. Tests use them.
func fixedModels(m *sdk.Models) func() *sdk.Models { return func() *sdk.Models { return m } }
func fixedKey(k string) func() string              { return func() string { return k } }
