package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"time"

	"github.com/regiellis/comfy-api-proxy-go/sdk"
)

// imageOrder is what the Create image dialog asks for.
type imageOrder struct {
	Engine     imageEngine
	Where      string // onComfyUI or onCloud
	Prompt     string
	Label      string
	Aspect     string
	Resolution string
	Refs       []Media // library images, in pick order
	Count      int
}

// makeImages adds one pending library image per count and renders each in
// the background, on the local proxy or on Comfy Cloud. The tiles follow
// their progress over SSE, as locally created references always have.
func (r *runner) makeImages(ctx context.Context, o imageOrder) ([]Media, error) {
	if err := o.Engine.readiness(ctx, o.Where, r.cloudKey(), r.refs); err != nil {
		return nil, err
	}
	origin := originLocal
	if o.Where == onCloud {
		origin = originCloud
	}
	cost, _ := o.Engine.cost(o.Aspect, o.Resolution, len(o.Refs))
	made := make([]Media, 0, o.Count)
	for range o.Count {
		m := Media{
			ID: newID(), Kind: kindImage, Origin: origin, Label: o.Label,
			Status: mediaPending, CreatedAt: now(), Note: "Waiting for " + o.Engine.Name,
		}
		if err := r.st.addMedia(ctx, m); err != nil {
			return made, err
		}
		made = append(made, m)
		r.wg.Go(func() {
			r.safely("image "+m.ID, func() { r.renderImage(m, o, cost) })
		})
	}
	return made, nil
}

// cloudKey is the Comfy API key, empty without one.
func (r *runner) cloudKey() string {
	if r.cloud == nil {
		return ""
	}
	return r.cloud.apiKey()
}

// renderImage sends the references to the server that runs the engine,
// renders the image, and records it ready or failed.
func (r *runner) renderImage(m Media, o imageOrder, cost float64) {
	pending := m
	var last time.Time
	progress := func(label string, v float64) {
		// One tile update per half second is plenty for a sampler bar.
		if time.Since(last) < 500*time.Millisecond {
			return
		}
		last = time.Now()
		pending.Note = fmt.Sprintf("%s · %d%%", label, int(v*100))
		r.pub.media(pending)
	}
	write := func(ctx context.Context, w io.Writer) error {
		client, opts := r.refs.client, sdk.SubmitOptions{}
		if o.Where == onCloud {
			client = r.cloud.cloud()
			if client == nil {
				return errOffline{"Add a Comfy API key in Settings to use Comfy Cloud."}
			}
			if o.Engine.Partner {
				opts.PartnerAPIKey = r.cloudKey()
			}
		}
		refs := make([]any, len(o.Refs))
		for i, ref := range o.Refs {
			pending.Note = fmt.Sprintf("Sending reference %d of %d", i+1, len(o.Refs))
			r.pub.media(pending)
			a, err := client.AssetFromFile(r.lib.path(ref)).Reference(ctx)
			if err != nil {
				return fmt.Errorf("send reference %d: %w", i+1, err)
			}
			refs[i] = a
		}
		wf, err := o.Engine.workflow(imageJob{
			Prompt: o.Prompt, Aspect: o.Aspect, Resolution: o.Resolution, Refs: refs, Seed: rand.Int64N(1 << 50),
		})
		if err != nil {
			return err
		}
		if o.Where == onComfyUI {
			if err := r.refs.fit(ctx, wf); err != nil {
				return err
			}
		}
		pending.Note = "Waiting for " + o.Engine.Name
		r.pub.media(pending)
		return renderOn(ctx, client, wf, opts, w, progress)
	}
	stored, err := r.storeStream(r.base, write, m.Origin, m.Label)
	done := m
	done.Note = ""
	if err != nil {
		done.Status = mediaFailed
		done.Error = describe(err)
	} else {
		done.File, done.ContentType, done.Size = stored.File, stored.ContentType, stored.Size
		done.Width, done.Height = stored.Width, stored.Height
		done.Status = mediaReady
		done.CostUSD = cost
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.st.finishMedia(ctx, done); err != nil {
		log.Printf("utsushi: save image %s: %v", done.ID, err)
	}
	r.pub.media(done)
}
