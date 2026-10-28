package main

import (
	"fmt"
	"io"
	"math"
	"testing"
)

// windowNet stands in for Video Depth Anything. A frame's feature is its
// index, and window k answers every frame with its true depth scaled and
// shifted by k's own amounts, as the real head's relative depth drifts
// window to window. It also checks each window is built as the reference
// builds it.
type windowNet struct {
	t      *testing.T
	frames int
	k      int
}

func trueDepth(f int) []float32 {
	return []float32{1 + float32(f), 3 + 2*float32(f), 0.5 + 0.01*float32(f*f)}
}

func (n *windowNet) encode(px []float32) ([]float32, error) { return []float32{px[0]}, nil }

func (n *windowNet) head(window [][]float32) ([][]float32, error) {
	step := vdaLen - vdaOverlap
	last := n.frames - 1
	for i, f := range window {
		var want int
		switch {
		case n.k == 0:
			want = min(i, last)
		case i == 0:
			want = 0
		case i == 1:
			want = min(step*(n.k-1)+12, last)
		default:
			want = min(step*n.k+i, last)
		}
		if int(f[0]) != want {
			n.t.Fatalf("window %d input %d is frame %d, want %d", n.k, i, int(f[0]), want)
		}
	}
	scale, shift := 1+0.5*float32(n.k), 0.25*float32(n.k)
	out := make([][]float32, len(window))
	for i, f := range window {
		out[i] = affine(trueDepth(int(f[0])), scale, shift)
	}
	n.k++
	return out, nil
}

func (n *windowNet) close() error { return nil }

func TestVideoDepthAlignsEveryWindowOntoTheFirst(t *testing.T) {
	for _, frames := range []int{1, 5, 22, 32, 33, 54, 55, 100} {
		t.Run(fmt.Sprintf("%d frames", frames), func(t *testing.T) {
			net := &windowNet{t: t, frames: frames}
			read := 0
			next := func() ([]float32, error) {
				if read == frames {
					return nil, io.EOF
				}
				read++
				return []float32{float32(read - 1)}, nil
			}
			var got [][]float32
			err := videoDepth(net, next, func(d []float32) []float32 { return d }, func(d []float32) error {
				got = append(got, d)
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != frames {
				t.Fatalf("emitted %d frames, want %d", len(got), frames)
			}
			for f, d := range got {
				for j, v := range trueDepth(f) {
					if math.Abs(float64(d[j]-v)) > 1e-3*float64(v) {
						t.Fatalf("frame %d value %d = %v, want %v", f, j, d[j], v)
					}
				}
			}
			if want := (frames + vdaLen - vdaOverlap - 1) / (vdaLen - vdaOverlap); net.k != want {
				t.Fatalf("ran %d windows, want %d", net.k, want)
			}
		})
	}
}

func TestScaleShiftRecoversAnAffineMap(t *testing.T) {
	pred := []float32{1, 2, 5, 9}
	target := affine(pred, 2.5, 0.75)
	s, sh := scaleShift(pred, target)
	if math.Abs(float64(s-2.5)) > 1e-5 || math.Abs(float64(sh-0.75)) > 1e-5 {
		t.Fatalf("scale, shift = %v, %v; want 2.5, 0.75", s, sh)
	}
	if s, sh := scaleShift([]float32{3, 3}, []float32{1, 2}); s != 1 || sh != 0 {
		t.Fatalf("singular case = %v, %v; want 1, 0", s, sh)
	}
}

func TestBlendMovesFromTheEarlierWindowToTheLater(t *testing.T) {
	pre := make([][]float32, vdaInterp)
	post := make([][]float32, vdaInterp)
	for i := range pre {
		pre[i], post[i] = []float32{0}, []float32{7}
	}
	blend(pre, post)
	for i, f := range pre {
		if want := float32(i); math.Abs(float64(f[0]-want)) > 1e-5 {
			t.Fatalf("blended frame %d = %v, want %v", i, f[0], want)
		}
	}
}

func TestDepthFrameSizes(t *testing.T) {
	for _, c := range []struct{ w, h, fw, fh, dw, dh int }{
		{480, 854, 291, 518, 518, 924},
		{1080, 1920, 291, 518, 518, 924},
		{1920, 1080, 910, 512, 924, 518},
		{1024, 1024, 518, 518, 518, 518},
	} {
		if fw, fh := fitBox(c.w, c.h, vdaW, vdaH); fw != c.fw || fh != c.fh {
			t.Errorf("fitBox(%d, %d) = %d, %d; want %d, %d", c.w, c.h, fw, fh, c.fw, c.fh)
		}
		if dw, dh := dav2Size(c.w, c.h); dw != c.dw || dh != c.dh {
			t.Errorf("dav2Size(%d, %d) = %d, %d; want %d, %d", c.w, c.h, dw, dh, c.dw, c.dh)
		}
	}
}

func TestPlanarCentersTheFrameAndRepeatsItsEdges(t *testing.T) {
	// A 2 by 1 frame, black then white, in a 4 by 3 box.
	rgb := []byte{0, 0, 0, 255, 255, 255}
	dst := make([]float32, 3*4*3)
	ox, oy := planar(dst, rgb, 2, 1, 4, 3)
	if ox != 1 || oy != 1 {
		t.Fatalf("frame at %d, %d; want 1, 1", ox, oy)
	}
	black, white := -imagenetMean[0]/imagenetStd[0], (1-imagenetMean[0])/imagenetStd[0]
	for y := range 3 {
		for x := range 4 {
			want := black
			if x >= 2 {
				want = white
			}
			if got := dst[y*4+x]; math.Abs(float64(got-want)) > 1e-5 {
				t.Fatalf("red at %d, %d = %v, want %v", x, y, got, want)
			}
		}
	}
	if got := cropPlane([]float32{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11}, 4, ox, oy, 2, 1); got[0] != 5 || got[1] != 6 {
		t.Fatalf("crop = %v, want [5 6]", got)
	}
}

func TestDepthRangeIgnoresAStrayPixel(t *testing.T) {
	d := make([]float32, 1000)
	for i := range d {
		d[i] = float32(i % 100)
	}
	d[500] = 1e6
	var r depthRange
	r.add(d)
	lo, hi := r.span(0)
	if hi > 100 {
		t.Fatalf("range top %v follows the stray pixel", hi)
	}
	level := r.level(0, []float32{lo, hi, 1e6, -5})
	if level[0] != 0 || level[1] != 1 || level[2] != 1 || level[3] != 0 {
		t.Fatalf("levels = %v, want [0 1 1 0]", level)
	}
	if g := grayLook([]float32{0, 1}, false); g[0] != 0 || g[1] != 255 {
		t.Fatalf("near is not white: %v", g)
	}
	if g := grayLook([]float32{0, 1}, true); g[0] != 255 || g[1] != 0 {
		t.Fatalf("inverted near is not black: %v", g)
	}
}

func TestDepthRangeSpansTheClipOrEachFramesNeighbors(t *testing.T) {
	// Five frames whose depth runs over 0..10, then 0..20, ... 0..50.
	var whole, smooth depthRange
	smooth.smooth = 1
	for f := 1; f <= 5; f++ {
		d := make([]float32, 101)
		for i := range d {
			d[i] = float32(i) / 10 * float32(f)
		}
		whole.add(d)
		smooth.add(d)
	}
	if lo, hi := whole.span(0); lo != 0 || hi < 49 {
		t.Fatalf("whole clip span = %v..%v, want 0..about 50", lo, hi)
	}
	// Frame 0 averages frames 0 and 1 (tops about 10 and 20).
	if _, hi := smooth.span(0); hi < 14 || hi > 16 {
		t.Fatalf("frame 0 span top = %v, want about 15", hi)
	}
	// Frame 2 averages frames 1, 2, and 3 (about 20, 30, 40).
	if _, hi := smooth.span(2); hi < 29 || hi > 31 {
		t.Fatalf("frame 2 span top = %v, want about 30", hi)
	}
}

func TestNormalLookFacesTheCameraOnAFlatWallAndTiltsWithASlope(t *testing.T) {
	const w, h = 6, 4
	flat := make([]float32, w*h)
	for i := range flat {
		flat[i] = 0.5
	}
	n := normalLook(flat, w, h)
	if n[0] != 128 || n[1] != 128 || n[2] != 255 {
		t.Fatalf("flat wall = %v, want 128 128 255", n[:3])
	}
	// Nearer to the right: the surface turns to face left.
	ramp := make([]float32, w*h)
	for y := range h {
		for x := range w {
			ramp[y*w+x] = float32(x) / w
		}
	}
	n = normalLook(ramp, w, h)
	if p := (1*w + 2) * 3; n[p] >= 128 || n[p+1] != 128 {
		t.Fatalf("slope rising right = %v, want red under 128 and green 128", n[p:p+3])
	}
}

func TestPickRatePrefersTheAverageWhateverTheOrder(t *testing.T) {
	// ffprobe's own order, read from drive2s.mp4: 48 frames over 2 s.
	got, err := pickRate("r_frame_rate=60/1\navg_frame_rate=24/1\n")
	if err != nil || got != "24/1" {
		t.Fatalf("pickRate = %q, %v; want 24/1", got, err)
	}
	if got, err := pickRate("r_frame_rate=30/1\navg_frame_rate=0/0\n"); err != nil || got != "30/1" {
		t.Fatalf("no average: pickRate = %q, %v; want 30/1", got, err)
	}
	if _, err := pickRate("r_frame_rate=0/0\navg_frame_rate=0/0\n"); err == nil {
		t.Fatal("no rate at all read as one")
	}
}

func TestRateValue(t *testing.T) {
	for rate, want := range map[string]float64{"30000/1001": 29.97, "24/1": 24, "25": 25, "0/0": 0, "x": 0} {
		if got := rateValue(rate); math.Abs(got-want) > 0.01 {
			t.Errorf("rateValue(%q) = %v, want %v", rate, got, want)
		}
	}
}
