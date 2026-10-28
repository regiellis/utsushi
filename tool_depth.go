package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"strconv"
	"strings"
)

var (
	depthModelNames = map[string]string{depthVideo: "stable", depthFrame: "sharp"}
	depthLookNames  = map[string]string{lookDepth: "Depth", lookInverted: "Depth inverted", lookNormals: "Normals"}
)

// depthTool makes depth maps: one model run over the source, then a
// library item per look rendered from the same depth.
var depthTool = &tool{
	ID: "depth", Name: "Depth maps", Code: "DP", Action: "Make depth", Model: true,
	Blurb: "How far each pixel is, so only motion and shape carry into a run",
	Takes: []string{kindVideo, kindImage},
	plan: func(o toolOrder) ([]toolOut, error) {
		model := depthModel(o)
		looks := formPick(o.Form, "look", depthLooks)
		if len(looks) == 0 {
			return nil, failedf("Pick at least one map to make")
		}
		outs := make([]toolOut, len(looks))
		for i, look := range looks {
			outs[i] = toolOut{Kind: o.Source.Kind, Label: depthLabel(o.Source, model, look)}
		}
		return outs, nil
	},
	run: runDepth,
	formOf: func(label string) url.Values {
		f := url.Values{}
		for _, model := range []string{depthFrame, depthVideo} {
			for _, look := range depthLooks {
				if strings.HasSuffix(label, " · "+depthLookNames[look]+" "+depthModelNames[model]) {
					f.Set("model", model)
					f.Set("look", look)
				}
			}
		}
		return f
	},
}

// depthModel is the order's model. An image always takes the per-frame
// model: the video model reads 32 frames at a time.
func depthModel(o toolOrder) string {
	if o.Source.Kind == kindImage {
		return depthFrame
	}
	return formChoice(o.Form, "model", []string{depthFrame, depthVideo})
}

// depthLabel names a depth item after its source: "walk.mp4 · Depth stable".
func depthLabel(src Media, model, look string) string {
	return fmt.Sprintf("%s · %s %s", src.Label, depthLookNames[look], depthModelNames[model])
}

// depthFiles are the model files a depth model needs.
func depthFiles(model string) []modelFile {
	if model == depthVideo {
		return []modelFile{vdaEncoder, vdaHead}
	}
	return []modelFile{dav2Small}
}

func runDepth(j *toolJob) error {
	o := j.order
	model := depthModel(o)
	looks := formPick(o.Form, "look", depthLooks)
	if err := j.models(depthFiles(model)...); err != nil {
		return err
	}
	rate, expect, err := j.video()
	if err != nil {
		return err
	}
	tmp, err := os.Create(j.work(".depth"))
	if err != nil {
		return err
	}
	var (
		rng   = depthRange{smooth: depthSmooth}
		count int
		bw    = bufio.NewWriterSize(tmp, 1<<20)
	)
	if model == depthVideo {
		rng.smooth = 0
	}
	emit := func(d []float32) error {
		rng.add(d)
		count++
		j.progress("Depth", count, expect)
		return binary.Write(bw, binary.LittleEndian, d)
	}
	w, h, err := j.r.estimate(j.ctx, j.r.tools.models, model, o.Source, j.src(), emit)
	if err == nil {
		err = bw.Flush()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if count == 0 {
		return failedf("No frames could be read from %s", o.Source.Label)
	}
	// The output takes the source's size, so it lines up with it frame for
	// frame and pixel for pixel.
	outW, outH := j.outSize()
	image := o.Source.Kind == kindImage
	for i, look := range looks {
		j.note(true, "Rendering %s", strings.ToLower(depthLookNames[look]))
		dst := j.work(j.ext())
		err := j.r.renderLook(j.ctx, dst, tmp.Name(), &rng, count, look, rawVideo{
			W: w, H: h, Rate: rate, OutW: outW, OutH: outH, Image: image, Sound: soundOf(image, j.src()),
		})
		if err != nil {
			return err
		}
		if err := j.keep(i, dst); err != nil {
			return err
		}
	}
	return nil
}

func soundOf(image bool, src string) string {
	if image {
		return ""
	}
	return src
}

// rateValue is an ffmpeg rate ("30000/1001") as a number, 0 if unreadable.
func rateValue(rate string) float64 {
	num, den, ok := strings.Cut(rate, "/")
	n, err1 := strconv.ParseFloat(num, 64)
	if !ok {
		if err1 != nil {
			return 0
		}
		return n
	}
	d, err2 := strconv.ParseFloat(den, 64)
	if err1 != nil || err2 != nil || d == 0 {
		return 0
	}
	return n / d
}

// estimate decodes the source and runs a depth model over it, sending each
// frame's depth to emit, and returns the depth's frame size.
func (r *runner) estimate(ctx context.Context, m *models, model string, source Media, src string, emit func([]float32) error) (int, int, error) {
	sw, sh := source.Width, source.Height
	if model == depthVideo {
		cw, ch := fitBox(sw, sh, vdaW, vdaH)
		net, err := newORTVDA(m.path(vdaEncoder), m.path(vdaHead))
		if err != nil {
			return 0, 0, err
		}
		ox, oy := (vdaW-cw)/2, (vdaH-ch)/2
		err = r.pullFrames(ctx, src, cw, ch, func(rgb []byte) []float32 {
			px := make([]float32, 3*vdaW*vdaH)
			planar(px, rgb, cw, ch, vdaW, vdaH)
			return px
		}, func(next func() ([]float32, error)) error {
			return videoDepth(net, next, func(d []float32) []float32 { return cropPlane(d, vdaW, ox, oy, cw, ch) }, emit)
		})
		return cw, ch, errors.Join(err, net.close())
	}
	dw, dh := dav2Size(sw, sh)
	net, err := newORTDAV2(m.path(dav2Small))
	if err != nil {
		return 0, 0, err
	}
	err = r.pullFrames(ctx, src, dw, dh, func(rgb []byte) []float32 {
		px := make([]float32, 3*dw*dh)
		planar(px, rgb, dw, dh, dw, dh)
		return px
	}, func(next func() ([]float32, error)) error {
		return frameDepth(net, dw, dh, next, emit)
	})
	return dw, dh, errors.Join(err, net.close())
}

// pullFrames decodes src at w by h in its own goroutine, preparing each
// frame there with prep, and hands run a next function that returns them
// in order, then io.EOF.
func (r *runner) pullFrames(ctx context.Context, src string, w, h int, prep func([]byte) []float32, run func(next func() ([]float32, error)) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	ch := make(chan []float32, 4)
	decoded := make(chan error, 1)
	go func() {
		err := r.frames.rawFrames(ctx, src, w, h, func(rgb []byte) error {
			select {
			case ch <- prep(rgb):
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
		close(ch)
		decoded <- err
	}()
	err := run(func() ([]float32, error) {
		px, ok := <-ch
		if !ok {
			return nil, io.EOF
		}
		return px, nil
	})
	if err != nil {
		cancel()
		for range ch {
		}
		return errors.Join(err, ignoreCanceled(<-decoded))
	}
	return <-decoded
}

func ignoreCanceled(err error) error {
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

// renderLook encodes one look from the depth file to dst.
func (r *runner) renderLook(ctx context.Context, dst, depthFile string, rng *depthRange, count int, look string, v rawVideo) error {
	v.Format = "gray"
	if look == lookNormals {
		v.Format = "rgb24"
	}
	return r.frames.encodeRaw(ctx, dst, v, func(out io.Writer) error {
		in, err := os.Open(depthFile)
		if err != nil {
			return err
		}
		defer func() {
			if err := in.Close(); err != nil {
				log.Printf("utsushi: close %s: %v", depthFile, err)
			}
		}()
		br := bufio.NewReaderSize(in, 1<<20)
		d := make([]float32, v.W*v.H)
		for i := range count {
			if err := binary.Read(br, binary.LittleEndian, d); err != nil {
				return fmt.Errorf("read the depth back: %w", err)
			}
			level := rng.level(i, d)
			frame := grayLook(level, look == lookInverted)
			if look == lookNormals {
				frame = normalLook(level, v.W, v.H)
			}
			if _, err := out.Write(frame); err != nil {
				return err
			}
		}
		return nil
	})
}
