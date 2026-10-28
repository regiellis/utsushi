package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// llmProvider is one choice in Settings. Every provider is reached through
// the OpenAI chat/completions API: Anthropic serves a compatibility endpoint,
// and the local servers speak it natively. Model defaults are left empty
// where this app cannot vouch for a current name; List models fills them in.
type llmProvider struct {
	ID, Code, Name, Sub, URL, Model, KeyHint string
	KeyRequired, Local                       bool
}

var llmProviders = []llmProvider{
	{ID: "claude", Code: "CL", Name: "Claude", Sub: "Anthropic · cloud", URL: "https://api.anthropic.com/v1", Model: "claude-sonnet-5-5", KeyHint: "sk-ant-…", KeyRequired: true},
	{ID: "openai", Code: "OA", Name: "OpenAI", Sub: "cloud", URL: "https://api.openai.com/v1", KeyHint: "sk-…", KeyRequired: true},
	{ID: "llamacpp", Code: "LC", Name: "llama.cpp", Sub: "local server", URL: "http://127.0.0.1:8081/v1", Local: true},
	{ID: "ollama", Code: "OL", Name: "Ollama", Sub: "local", URL: "http://127.0.0.1:11434/v1", Local: true},
	{ID: "lmstudio", Code: "LM", Name: "LM Studio", Sub: "local", URL: "http://127.0.0.1:1234/v1", Local: true},
	{ID: "custom", Code: "{}", Name: "Custom", Sub: "OpenAI-compatible", KeyHint: "optional"},
}

func providerByID(id string) llmProvider {
	for _, p := range llmProviders {
		if p.ID == id {
			return p
		}
	}
	return llmProviders[3]
}

// llmClient talks to one provider.
type llmClient struct {
	base, key, model string
	http             *http.Client
}

// newLLM is the client Settings describe, or an offline reason.
func newLLM(p LLMPrefs) (*llmClient, error) {
	if !p.On {
		return nil, errOffline{"The language model is off in Settings."}
	}
	prov := providerByID(p.Provider)
	base := strings.TrimRight(firstNonEmpty(p.URLs[prov.ID], prov.URL), "/")
	key := p.Keys[prov.ID]
	if base == "" {
		return nil, errOffline{"Add a base URL for the language model in Settings."}
	}
	if prov.KeyRequired && key == "" {
		return nil, errOffline{"Add an API key for " + prov.Name + " in Settings."}
	}
	return &llmClient{base: base, key: key, model: firstNonEmpty(p.Models[prov.ID], prov.Model), http: &http.Client{Timeout: 90 * time.Second}}, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// llmImage is one picture sent with a prompt.
type llmImage struct {
	Type  string // MIME type
	Data  []byte
	Label string // sent as text just before the image, when set
}

// chat sends one system and user turn, with images, and returns the reply.
func (c *llmClient) chat(ctx context.Context, system, user string, images []llmImage) (string, error) {
	content := []map[string]any{{"type": "text", "text": user}}
	for _, img := range images {
		if img.Label != "" {
			content = append(content, map[string]any{"type": "text", "text": img.Label})
		}
		content = append(content, map[string]any{"type": "image_url", "image_url": map[string]any{
			"url": "data:" + img.Type + ";base64," + base64.StdEncoding.EncodeToString(img.Data),
		}})
	}
	body := map[string]any{
		"messages": []map[string]any{
			{"role": "system", "content": system},
			{"role": "user", "content": content},
		},
		"max_tokens":  600,
		"temperature": 0.4,
	}
	if c.model != "" {
		body["model"] = c.model
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := c.do(ctx, http.MethodPost, "/chat/completions", body, &out); err != nil {
		return "", err
	}
	if len(out.Choices) == 0 || strings.TrimSpace(out.Choices[0].Message.Content) == "" {
		return "", failedf("The language model sent an empty reply")
	}
	return strings.TrimSpace(out.Choices[0].Message.Content), nil
}

// models lists the model ids the server offers.
func (c *llmClient) models(ctx context.Context) ([]string, error) {
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, "/models", nil, &out); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(out.Data))
	for _, m := range out.Data {
		ids = append(ids, m.ID)
	}
	sort.Strings(ids)
	return ids, nil
}

func (c *llmClient) do(ctx context.Context, method, path string, body, out any) (err error) {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.key != "" {
		req.Header.Set("Authorization", "Bearer "+c.key)
		// Anthropic's compatibility endpoint also takes its own header.
		req.Header.Set("x-api-key", c.key)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		msg := resp.Status
		if json.Unmarshal(data, &e) == nil && e.Error.Message != "" {
			msg = e.Error.Message
		}
		return failedf("The language model refused: %s", msg)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("the language model's reply is not JSON: %w", err)
	}
	return nil
}

// llmTest sends a short prompt, then a tiny image, and reports the latency
// and whether the model reads images.
func (c *llmClient) test(ctx context.Context) (string, error) {
	start := time.Now()
	if _, err := c.chat(ctx, "Answer in one word.", "Reply with OK.", nil); err != nil {
		return "", err
	}
	took := time.Since(start)
	vision := "text only, so the clip and reference features are unavailable"
	probe, err := probePNG()
	if err != nil {
		return "", err
	}
	if _, err := c.chat(ctx, "Answer in one word.", "What color is this image?", []llmImage{{Type: "image/png", Data: probe}}); err == nil {
		vision = "vision supported"
	}
	model := c.model
	if model == "" {
		model = "server default"
	}
	return fmt.Sprintf("Responded in %.1f s · %s · %s", took.Seconds(), model, vision), nil
}

// probePNG is an 8x8 red square, the vision probe.
func probePNG() ([]byte, error) {
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	draw.Draw(img, img.Bounds(), image.NewUniform(color.RGBA{R: 0xff, A: 0xff}), image.Point{}, draw.Src)
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
