package main

import (
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
)

// Matte looks: the mask, and the people over black in their own colors.
var (
	matteLooks     = []string{"matte", "cutout"}
	matteLookNames = map[string]string{"matte": "Matte", "cutout": "Cutout"}
)

// matteTool separates people from the background with MODNet: a mask,
// white where a person is, and a cutout of them over black.
var matteTool = &tool{
	ID: "matte", Name: "Person matte", Code: "MT", Action: "Make matte", Model: true,
	Blurb: "A mask of the people, white on black, or the people cut out over black",
	Takes: []string{kindVideo, kindImage},
	plan: func(o toolOrder) ([]toolOut, error) {
		looks := formPick(o.Form, "look", matteLooks)
		if len(looks) == 0 {
			return nil, failedf("Pick the matte, the cutout, or both")
		}
		outs := make([]toolOut, len(looks))
		for i, l := range looks {
			outs[i] = toolOut{Kind: o.Source.Kind, Label: o.Source.Label + " · " + matteLookNames[l]}
		}
		return outs, nil
	},
	run: runMatte,
}

// modnetSize is MODNet's input for a frame, as its own ONNX inference
// script sets it (ref_size 512): the short side 512 when the frame is
// smaller than that on every side or larger on every side, the size kept
// otherwise, then both sides down to a multiple of 32.
func modnetSize(w, h int) (int, int) {
	const ref = 512
	if max(w, h) < ref || min(w, h) > ref {
		if w >= h {
			w, h = int(float64(w)/float64(h)*ref), ref
		} else {
			w, h = ref, int(float64(h)/float64(w)*ref)
		}
	}
	return max(32, w-w%32), max(32, h-h%32)
}

func runMatte(j *toolJob) error {
	looks := formPick(j.order.Form, "look", matteLooks)
	if err := j.models(modelFilesFor("matte")...); err != nil {
		return err
	}
	net, err := newONNX("matte", j.r.tools.models.path(modnet), []string{"input"}, []string{"output"})
	if err != nil {
		return err
	}
	defer func() {
		if err := net.close(); err != nil {
			j.note(true, "Closing the matte model: %v", err)
		}
	}()
	rate, expect, err := j.video()
	if err != nil {
		return err
	}
	w, h := modnetSize(j.order.Source.Width, j.order.Source.Height)
	outW, outH := j.outSize()
	image := j.order.Source.Kind == kindImage
	matte, n := j.work(j.ext()), 0
	err = j.r.frames.encodeRaw(j.ctx, matte, rawVideo{
		W: w, H: h, Format: "gray", Rate: rate, OutW: outW, OutH: outH, Image: image, Sound: soundOf(image, j.src()),
	}, func(out io.Writer) error {
		plane := w * h
		px := make([]float32, 3*plane)
		return j.r.frames.rawFrames(j.ctx, j.src(), w, h, func(rgb []byte) error {
			n++
			j.progress("Matte", n, expect)
			for i := range plane {
				for c := range 3 {
					px[c*plane+i] = (float32(rgb[i*3+c]) - 127.5) / 127.5
				}
			}
			res, err := net.run(onnxIn{shape: []int64{1, 3, int64(h), int64(w)}, data: px})
			if err != nil {
				return err
			}
			alpha := make([]byte, plane)
			for i, v := range res[0].data[:plane] {
				alpha[i] = byte(math.Round(float64(min(max(v, 0), 1)) * 255))
			}
			_, err = out.Write(alpha)
			return err
		})
	})
	if err != nil {
		return err
	}
	for i, look := range looks {
		path := matte
		if look == "cutout" {
			j.note(true, "Cutting out")
			path = j.work(j.ext())
			if err := j.r.frames.cutout(j.ctx, j.src(), matte, path, rate, image); err != nil {
				return err
			}
		}
		if err := j.keep(i, path); err != nil {
			return err
		}
	}
	return nil
}

// Upscale choices.
var (
	upscaleFactors = []string{"2", "4"}
	upscaleModels  = []string{"general", "anime"}
	// upscaleLongest is the longest side an upscale may reach: 4K UHD.
	upscaleLongest = 3840
)

// upscaleTool enlarges a clip or image 2 or 4 times with Real-ESRGAN's
// compact models. 2 times is the 4 times model scaled down by half, as
// Real-ESRGAN's own outscale does.
var upscaleTool = &tool{
	ID: "upscale", Name: "Upscale", Code: "UP", Action: "Upscale", Model: true,
	Blurb: "A clip or image at 2 or 4 times the size, details redrawn by Real-ESRGAN",
	Takes: []string{kindVideo, kindImage},
	plan: func(o toolOrder) ([]toolOut, error) {
		f, model := upscaleChoices(o)
		w, h := o.Source.Width*f, o.Source.Height*f
		if max(w, h) > upscaleLongest {
			return nil, failedf("%d× makes %d×%d, past the %d px longest side. Conform it smaller first", f, w, h, upscaleLongest)
		}
		label := fmt.Sprintf("%s · %d× upscale", o.Source.Label, f)
		if model == "anime" {
			label += " anime"
		}
		return []toolOut{{Kind: o.Source.Kind, Label: label}}, nil
	},
	run: runUpscale,
}

func upscaleChoices(o toolOrder) (int, string) {
	f, err := strconv.Atoi(formChoice(o.Form, "factor", upscaleFactors))
	if err != nil {
		f = 2
	}
	return f, formChoice(o.Form, "model", upscaleModels)
}

func runUpscale(j *toolJob) error {
	f, model := upscaleChoices(j.order)
	file := esrganGeneral
	if model == "anime" {
		file = esrganAnime
	}
	if err := j.models(file); err != nil {
		return err
	}
	net, err := newONNX("upscale", j.r.tools.models.path(file), []string{"input"}, []string{"output"})
	if err != nil {
		return err
	}
	defer func() {
		if err := net.close(); err != nil {
			j.note(true, "Closing the upscale model: %v", err)
		}
	}()
	rate, expect, err := j.video()
	if err != nil {
		return err
	}
	w, h := j.order.Source.Width, j.order.Source.Height
	image := j.order.Source.Kind == kindImage
	outW, outH := w*f, h*f
	if !image {
		outW, outH = outW&^1, outH&^1
	}
	dst, n := j.work(j.ext()), 0
	err = j.r.frames.encodeRaw(j.ctx, dst, rawVideo{
		W: w * 4, H: h * 4, Format: "rgb24", Rate: rate, OutW: outW, OutH: outH, Image: image, Sound: soundOf(image, j.src()),
	}, func(out io.Writer) error {
		plane := w * h
		px := make([]float32, 3*plane)
		return j.r.frames.rawFrames(j.ctx, j.src(), w, h, func(rgb []byte) error {
			n++
			j.progress("Upscale", n, expect)
			for i := range plane {
				for c := range 3 {
					px[c*plane+i] = float32(rgb[i*3+c]) / 255
				}
			}
			res, err := net.run(onnxIn{shape: []int64{1, 3, int64(h), int64(w)}, data: px})
			if err != nil {
				return err
			}
			_, err = out.Write(planarToRGB(res[0].data, w*4, h*4))
			return err
		})
	})
	if err != nil {
		return err
	}
	return j.keep(0, dst)
}

// planarToRGB turns a planar 0..1 RGB tensor into packed bytes, clamped.
func planarToRGB(data []float32, w, h int) []byte {
	plane := w * h
	out := make([]byte, plane*3)
	for i := range plane {
		for c := range 3 {
			out[i*3+c] = byte(math.Round(float64(min(max(data[c*plane+i], 0), 1)) * 255))
		}
	}
	return out
}

// Interpolation factors: frames between each pair, plus one.
var interpFactors = []string{"2", "4"}

// interpCut is RIFE's scene cut test: frames this unlike are not blended
// (Practical-RIFE skips under an SSIM of 0.2).
const interpCut = 0.2

// interpTool makes a clip smoother with RIFE 4.9, inserting frames between
// each pair so it plays at 2 or 4 times the frame rate over the same
// length. Across a cut it repeats the frame instead of blending two shots.
var interpTool = &tool{
	ID: "interp", Name: "Frame interpolation", Code: "FI", Action: "Interpolate", Model: true,
	Blurb: "A smoother clip at 2 or 4 times the frame rate, new frames drawn by RIFE",
	Takes: []string{kindVideo},
	plan: func(o toolOrder) ([]toolOut, error) {
		return []toolOut{{Kind: kindVideo, Label: fmt.Sprintf("%s · %s× frames", o.Source.Label, formChoice(o.Form, "factor", interpFactors))}}, nil
	},
	run: runInterp,
}

func runInterp(j *toolJob) error {
	m, err := strconv.Atoi(formChoice(j.order.Form, "factor", interpFactors))
	if err != nil {
		return err
	}
	if err := j.models(modelFilesFor("interp")...); err != nil {
		return err
	}
	net, err := newONNX("interpolation", j.r.tools.models.path(rife49), []string{"input"}, []string{"output"})
	if err != nil {
		return err
	}
	defer func() {
		if err := net.close(); err != nil {
			j.note(true, "Closing the interpolation model: %v", err)
		}
	}()
	rate, expect, err := j.video()
	if err != nil {
		return err
	}
	outRate, err := scaleRate(rate, m)
	if err != nil {
		return err
	}
	w, h := j.outSize()
	// RIFE takes sides in multiples of 32: pad right and bottom with black,
	// then crop.
	pw, ph := (w+31)/32*32, (h+31)/32*32
	dst, n := j.work(".mp4"), 0
	err = j.r.frames.encodeRaw(j.ctx, dst, rawVideo{
		W: w, H: h, Format: "rgb24", Rate: outRate, OutW: w, OutH: h, Sound: j.src(),
	}, func(out io.Writer) error {
		var prev []byte
		in := make([]float32, 7*pw*ph)
		err := j.r.frames.rawFrames(j.ctx, j.src(), w, h, func(rgb []byte) error {
			n++
			j.progress("Interpolate", n, expect)
			cur := append([]byte(nil), rgb...)
			if prev != nil {
				if _, err := out.Write(prev); err != nil {
					return err
				}
				cut := frameSSIM(prev, cur, w, h) < interpCut
				for k := 1; k < m; k++ {
					mid := prev
					if !cut {
						fillRIFE(in, prev, cur, w, h, pw, ph, float32(k)/float32(m))
						res, err := net.run(onnxIn{shape: []int64{1, 7, int64(ph), int64(pw)}, data: in})
						if err != nil {
							return err
						}
						mid = cropPlanar(res[0].data, pw, ph, w, h)
					}
					if _, err := out.Write(mid); err != nil {
						return err
					}
				}
			}
			prev = cur
			return nil
		})
		if err != nil {
			return err
		}
		if prev == nil {
			return failedf("No frames could be read from %s", j.order.Source.Label)
		}
		// The last frame holds for m frames, so the clip keeps its length.
		for range m {
			if _, err := out.Write(prev); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	return j.keep(0, dst)
}

// scaleRate multiplies an ffmpeg rate ("30000/1001") by m.
func scaleRate(rate string, m int) (string, error) {
	num, den, ok := strings.Cut(rate, "/")
	if !ok {
		den = "1"
	}
	n, err := strconv.ParseInt(num, 10, 64)
	if err != nil {
		return "", fmt.Errorf("read the frame rate %q: %w", rate, err)
	}
	if _, err := strconv.ParseInt(den, 10, 64); err != nil {
		return "", fmt.Errorf("read the frame rate %q: %w", rate, err)
	}
	return strconv.FormatInt(n*int64(m), 10) + "/" + den, nil
}

// fillRIFE writes RIFE 4.9's 7-channel input: frame A's RGB, frame B's
// RGB, both 0..1 and padded with black to pw by ph, then the time t.
func fillRIFE(in []float32, a, b []byte, w, h, pw, ph int, t float32) {
	plane := pw * ph
	clear(in[:6*plane])
	for y := range h {
		for x := range w {
			s, d := (y*w+x)*3, y*pw+x
			for c := range 3 {
				in[c*plane+d] = float32(a[s+c]) / 255
				in[(3+c)*plane+d] = float32(b[s+c]) / 255
			}
		}
	}
	for i := 6 * plane; i < 7*plane; i++ {
		in[i] = t
	}
}

// cropPlanar takes the w by h corner of a planar 0..1 RGB tensor pw wide
// as packed bytes.
func cropPlanar(data []float32, pw, ph, w, h int) []byte {
	plane := pw * ph
	out := make([]byte, w*h*3)
	for y := range h {
		for x := range w {
			for c := range 3 {
				out[(y*w+x)*3+c] = byte(math.Round(float64(min(max(data[c*plane+y*pw+x], 0), 1)) * 255))
			}
		}
	}
	return out
}

// frameSSIM is the structural similarity of two frames' 32 by 32 gray
// thumbnails, one window over the whole, as a cut test: about 1 for the
// same shot, near 0 or below across a cut.
func frameSSIM(a, b []byte, w, h int) float64 {
	thumb := func(f []byte) []float64 {
		const n = 32
		out := make([]float64, n*n)
		cnt := make([]float64, n*n)
		for y := range h {
			for x := range w {
				p := (y*w + x) * 3
				i := (y*n/h)*n + x*n/w
				out[i] += 0.299*float64(f[p]) + 0.587*float64(f[p+1]) + 0.114*float64(f[p+2])
				cnt[i]++
			}
		}
		for i := range out {
			if cnt[i] > 0 {
				out[i] /= cnt[i]
			}
		}
		return out
	}
	x, y := thumb(a), thumb(b)
	var mx, my float64
	for i := range x {
		mx += x[i]
		my += y[i]
	}
	k := float64(len(x))
	mx, my = mx/k, my/k
	var vx, vy, cov float64
	for i := range x {
		vx += (x[i] - mx) * (x[i] - mx)
		vy += (y[i] - my) * (y[i] - my)
		cov += (x[i] - mx) * (y[i] - my)
	}
	vx, vy, cov = vx/(k-1), vy/(k-1), cov/(k-1)
	const c1, c2 = (0.01 * 255) * (0.01 * 255), (0.03 * 255) * (0.03 * 255)
	return ((2*mx*my + c1) * (2*cov + c2)) / ((mx*mx + my*my + c1) * (vx + vy + c2))
}
