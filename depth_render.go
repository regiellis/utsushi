package main

import (
	"math"
	"slices"
)

// Depth models, and the looks a depth map is rendered in.
const (
	depthVideo = "video" // Video Depth Anything Small: stable across frames
	depthFrame = "frame" // Depth Anything V2 Small per frame: sharper edges

	lookDepth    = "depth"    // near is white, as ControlNet and LTX read depth
	lookInverted = "inverted" // near is black
	lookNormals  = "normals"  // surface normals as RGB
)

var depthLooks = []string{lookDepth, lookInverted, lookNormals}

// depthRange is the span each frame of a clip is rendered across. A
// frame's own bounds are its 0.5th and 99.5th percentiles, so a stray pixel
// cannot set them. Video Depth Anything aligns its frames itself, so one
// span covers the whole clip and a frame keeps its brightness through it.
// A per-frame model's depth holds only up to a scale and shift per frame,
// so each frame takes its own span, averaged over its neighbors so the
// brightness does not pump.
type depthRange struct {
	lo, hi []float32
	// smooth is how many frames on each side a per-frame span averages
	// over; 0 spans the whole clip.
	smooth int
}

// depthSamples is about how many values a frame's percentiles read.
const depthSamples = 20000

// depthSmooth is the per-frame model's span window, about a third of a
// second at 24 fps.
const depthSmooth = 4

func (r *depthRange) add(d []float32) {
	stride := max(1, len(d)/depthSamples)
	s := make([]float32, 0, len(d)/stride+1)
	for i := 0; i < len(d); i += stride {
		s = append(s, d[i])
	}
	slices.Sort(s)
	r.lo = append(r.lo, s[int(0.005*float64(len(s)-1))])
	r.hi = append(r.hi, s[int(0.995*float64(len(s)-1))])
}

// span is frame i's bounds.
func (r *depthRange) span(i int) (lo, hi float32) {
	if r.smooth == 0 {
		return slices.Min(r.lo), slices.Max(r.hi)
	}
	from, to := max(0, i-r.smooth), min(len(r.lo), i+r.smooth+1)
	for j := from; j < to; j++ {
		lo += r.lo[j]
		hi += r.hi[j]
	}
	n := float32(to - from)
	return lo / n, hi / n
}

// level is frame i's depth across its span, 0 far to 1 near. The models
// give relative disparity, which grows toward the camera.
func (r *depthRange) level(i int, d []float32) []float32 {
	out := make([]float32, len(d))
	lo, hi := r.span(i)
	span := hi - lo
	if span <= 0 {
		return out
	}
	for j, v := range d {
		out[j] = min(max((v-lo)/span, 0), 1)
	}
	return out
}

// grayLook is a frame of levels as 8-bit gray, near white or, inverted,
// near black.
func grayLook(level []float32, inverted bool) []byte {
	out := make([]byte, len(level))
	for i, v := range level {
		if inverted {
			v = 1 - v
		}
		out[i] = byte(math.Round(float64(v) * 255))
	}
	return out
}

// normalStrength scales the depth's slope against the frame's width before
// the normal is taken: higher reads more relief.
const normalStrength = 2.0

// normalLook is the surface normal of a w by h frame of levels as RGB, the
// OpenGL way: red is right, green is up, blue faces the camera, so a flat
// wall facing the camera is (128, 128, 255). The slope comes from a Sobel
// filter, edges repeated.
func normalLook(level []float32, w, h int) []byte {
	out := make([]byte, w*h*3)
	at := func(x, y int) float32 { return level[min(max(y, 0), h-1)*w+min(max(x, 0), w-1)] }
	k := normalStrength * float32(w) / 8 // a Sobel sums 8 pixel steps
	for y := range h {
		for x := range w {
			gx := at(x+1, y-1) + 2*at(x+1, y) + at(x+1, y+1) - at(x-1, y-1) - 2*at(x-1, y) - at(x-1, y+1)
			gy := at(x-1, y+1) + 2*at(x, y+1) + at(x+1, y+1) - at(x-1, y-1) - 2*at(x, y-1) - at(x+1, y-1)
			// The level is height toward the camera; image y runs down.
			nx, ny, nz := -gx*k, gy*k, float32(1)
			l := float32(math.Sqrt(float64(nx*nx + ny*ny + nz*nz)))
			p := (y*w + x) * 3
			out[p] = byte(math.Round(float64((nx/l + 1) * 127.5)))
			out[p+1] = byte(math.Round(float64((ny/l + 1) * 127.5)))
			out[p+2] = byte(math.Round(float64((nz/l + 1) * 127.5)))
		}
	}
	return out
}
