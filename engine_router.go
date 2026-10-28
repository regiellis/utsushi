package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/regiellis/comfy-api-proxy-go/sdk"
)

// urlSource turns a library file into a public URL a partner can fetch.
type urlSource interface {
	publicURL(ctx context.Context, m Media) (string, error)
}

// cloudAssets uploads library files to Comfy Cloud. A commit hashes the file
// and asks the server for that hash first, so a clip used by four versions is
// sent once.
type cloudAssets struct {
	client func() *sdk.Client // nil without an API key
	lib    *library
}

func (a *cloudAssets) asset(m Media) (*sdk.Asset, error) {
	client := a.client()
	if client == nil {
		return nil, errOffline{"Set COMFY_API_KEY to upload to Comfy Cloud."}
	}
	if m.File == "" {
		return nil, fmt.Errorf("media %s has no file", m.ID)
	}
	return client.AssetFromFile(a.lib.path(m)), nil
}

// publicURL is a signed link to the uploaded bytes, valid for hours.
func (a *cloudAssets) publicURL(ctx context.Context, m Media) (string, error) {
	as, err := a.asset(m)
	if err != nil {
		return "", err
	}
	d, err := as.DownloadURL(ctx)
	if err != nil {
		return "", err
	}
	return d.URL, nil
}

// routerEngine runs one Comfy Router model through its queue: submit, follow
// the status, collect. Queueing rather than a synchronous run keeps the
// request id, so a restart collects a billed generation instead of losing it.
type routerEngine struct {
	about  engineInfo
	model  string
	models func() *sdk.Models // nil without an API key
	urls   urlSource
	// args builds the model's request body from public URLs.
	args func(t task, source string, refs []string) map[string]any
	// result reads the video URL from the model's native output.
	result func(out map[string]any) (string, error)
}

func (e *routerEngine) info() engineInfo { return e.about }

func (e *routerEngine) ready(context.Context) error {
	if e.models() == nil {
		return errOffline{"Set COMFY_API_KEY to run Comfy Router models."}
	}
	return nil
}

func (e *routerEngine) generate(ctx context.Context, t task, tr tracker) (outcome, error) {
	if err := e.ready(ctx); err != nil {
		return outcome{}, err
	}
	tr.running("Uploading the source clip", 0)
	source, err := e.urls.publicURL(ctx, t.Source)
	if err != nil {
		return outcome{}, fmt.Errorf("upload source: %w", err)
	}
	images := t.ofKind(kindImage)
	refs := make([]string, 0, len(images))
	for i, r := range images {
		tr.running(fmt.Sprintf("Uploading reference %d of %d", i+1, len(images)), 0)
		u, err := e.urls.publicURL(ctx, r)
		if err != nil {
			return outcome{}, fmt.Errorf("upload reference %d: %w", i+1, err)
		}
		refs = append(refs, u)
	}
	tr.running("Submitting to "+e.model, 0)
	h, err := e.models().Submit(ctx, e.model, e.args(t, source, refs), sdk.RunOptions{IdempotencyKey: t.Key})
	if err != nil {
		return outcome{}, apiFailure(err)
	}
	tr.queued(h.RequestID(), nil)
	return e.follow(ctx, h, tr)
}

func (e *routerEngine) resume(ctx context.Context, requestID string, tr tracker) (outcome, error) {
	if err := e.ready(ctx); err != nil {
		return outcome{}, err
	}
	h, err := e.models().Handle(e.model, requestID)
	if err != nil {
		return outcome{}, err
	}
	return e.follow(ctx, h, tr)
}

func (e *routerEngine) follow(ctx context.Context, h *sdk.RequestHandle, tr tracker) (outcome, error) {
	for u, err := range h.Updates(ctx) {
		if err != nil {
			return outcome{}, apiFailure(err)
		}
		switch {
		case u.IsCompleted():
			tr.running("Collecting the result", 1)
		case u.QueuePosition != nil:
			tr.queued(h.RequestID(), u.QueuePosition)
		default:
			tr.running(queueLabel(u.Status), 0)
		}
	}
	out, err := h.Get(ctx)
	if err != nil {
		return outcome{}, apiFailure(err)
	}
	if out.JSON == nil {
		return outcome{}, failedf("%s answered with %s bytes, not a result document", e.model, out.Binary.ContentType)
	}
	u, err := e.result(out.JSON)
	if err != nil {
		return outcome{}, err
	}
	return outcome{URL: u}, nil
}

func (e *routerEngine) cancel(ctx context.Context, requestID string) error {
	if err := e.ready(ctx); err != nil {
		return err
	}
	h, err := e.models().Handle(e.model, requestID)
	if err != nil {
		return err
	}
	_, err = h.Cancel(ctx)
	return err
}

// queueLabel turns a queue status such as IN_PROGRESS into "In progress".
func queueLabel(status string) string {
	s := strings.ToLower(strings.ReplaceAll(status, "_", " "))
	if s == "" {
		return "Waiting on the provider"
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// apiFailure keeps an API refusal's own message for the run card. A
// transport or context error passes through unchanged, so the runner can tell
// a shutdown from a refusal.
func apiFailure(err error) error {
	var api *sdk.APIError
	// A 5xx or 429 that outlived the SDK's retries is lost contact, not a
	// verdict: while the provider holds the run, the runner tries again.
	if errors.As(err, &api) && api.HTTPStatus < 500 && api.HTTPStatus != http.StatusTooManyRequests {
		msg := api.Message
		if msg == "" {
			msg = api.Code
		}
		return failure{fmt.Sprintf("%s (%s)", msg, api.Code)}
	}
	return err
}

// Model bindings. Each request body follows the model's published schema
// (GET /v2/models/{id}/schema), read 2026-10-17.

func newGenjutsuEngine(models func() *sdk.Models, urls urlSource) *routerEngine {
	return &routerEngine{
		about: engineInfo{
			ID: "genjutsu", Mode: modeMotion, Name: "Genjutsu Motion Transfer", Via: "Comfy Router", Code: "GJ", Billed: true,
			Blurb: "Recasts the people in a clip from your references and keeps its motion, camera, and timing.",
			Limits: limits{
				// The schema: "Minimum 4 seconds; a longer video is trimmed to
				// 30 seconds." So 30 is a trim point, not a limit.
				MinRefs: 1, MaxRefs: 8, MinSeconds: 4, TrimSeconds: 30, PromptMax: 10000,
				RefsFor:     "The cast, the location, and the style.",
				Resolutions: []string{"480p", "720p", "1080p"}, DefaultResolution: "720p",
			},
		},
		model:  "higgsfield/higgsfield-genjutsu-motion-transfer-v1.0",
		models: models,
		urls:   urls,
		args: func(t task, source string, refs []string) map[string]any {
			args := map[string]any{"video_url": source, "image_urls": refs, "resolution": t.Resolution}
			if t.Prompt != "" {
				args["prompt"] = t.Prompt
			}
			return args
		},
		result: func(out map[string]any) (string, error) {
			status, _ := out["status"].(string)
			if status != "completed" {
				detail, _ := out["error"].(string)
				return "", failedf("Genjutsu ended %s. %s", status, detail)
			}
			video, _ := out["video"].(map[string]any)
			u, _ := video["url"].(string)
			if u == "" {
				return "", failedf("Genjutsu completed without a video URL")
			}
			return u, nil
		},
	}
}

func newAlephEngine(models func() *sdk.Models, urls urlSource) *routerEngine {
	return &routerEngine{
		about: engineInfo{
			ID: "aleph2", Mode: modeSwap, Name: "Runway Aleph 2", Via: "Comfy Router", Code: "A2", Billed: true,
			Blurb: "Edits the clip from your prompt. An optional reference image guides the first frame.",
			Limits: limits{MaxRefs: 1, MaxSeconds: 30, PromptRequired: true, PromptMax: 1000,
				RefsFor: "How the first frame should look after the edit."},
		},
		model:  "runway/aleph2",
		models: models,
		urls:   urls,
		args: func(t task, source string, refs []string) map[string]any {
			args := map[string]any{"promptText": t.Prompt, "videoUri": source}
			if len(refs) > 0 {
				args["promptImage"] = []map[string]any{{"uri": refs[0], "position": "first"}}
			}
			return args
		},
		result: func(out map[string]any) (string, error) {
			if status, _ := out["status"].(string); status != "SUCCEEDED" {
				return "", failedf("Runway ended %s", status)
			}
			list, _ := out["output"].([]any)
			if len(list) == 0 {
				return "", failedf("Runway succeeded without an output URL")
			}
			u, _ := list[0].(string)
			if u == "" {
				return "", failedf("Runway's output is not a URL")
			}
			return u, nil
		},
	}
}

func newWanEditEngine(models func() *sdk.Models, urls urlSource) *routerEngine {
	return &routerEngine{
		about: engineInfo{
			ID: "wan27-edit", Mode: modeSwap, Name: "Wan 2.7 Video Edit", Via: "Comfy Router", Code: "W7", Billed: true,
			Blurb: "Edits a 2 to 10 second clip from your prompt, with up to four reference images.",
			Limits: limits{
				MaxRefs: 4, MinSeconds: 2, MaxSeconds: 10, PromptRequired: true, PromptMax: 800,
				RefsFor:     "What the edit brings in: a person, an object, or a look.",
				Resolutions: []string{"720P", "1080P"}, DefaultResolution: "720P",
			},
		},
		model:  "wan/wan2.7-videoedit",
		models: models,
		urls:   urls,
		args: func(t task, source string, refs []string) map[string]any {
			media := []map[string]any{{"type": "video", "url": source}}
			for _, r := range refs {
				media = append(media, map[string]any{"type": "reference_image", "url": r})
			}
			return map[string]any{
				"input":      map[string]any{"prompt": t.Prompt, "media": media},
				"parameters": map[string]any{"resolution": t.Resolution, "audio_setting": "origin"},
			}
		},
		result: func(out map[string]any) (string, error) {
			if msg, _ := out["message"].(string); msg != "" {
				code, _ := out["code"].(string)
				return "", failedf("Wan refused the edit: %s (%s)", msg, code)
			}
			o, _ := out["output"].(map[string]any)
			if status, _ := o["task_status"].(string); status != "SUCCEEDED" {
				msg, _ := o["message"].(string)
				return "", failedf("Wan ended %s. %s", status, msg)
			}
			u, _ := o["video_url"].(string)
			if u == "" {
				return "", failedf("Wan succeeded without a video URL")
			}
			return u, nil
		},
	}
}
