package main

import (
	"io"
)

// Edge detail: Canny's low and high thresholds. "medium" is ControlNet's
// default, 100 and 200.
var (
	edgeDetails    = []string{"medium", "more", "less"}
	edgeThresholds = map[string][2]int32{"medium": {100, 200}, "more": {50, 100}, "less": {150, 300}}
	edgeNames      = map[string]string{"medium": "", "more": " fine", "less": " bold"}
)

// edgesTool draws every frame's outlines white on black, as ControlNet's
// Canny annotator does, for engines that take edges as control.
var edgesTool = &tool{
	ID: "edges", Name: "Edges", Code: "ED", Action: "Make edges",
	Blurb: "Outlines of every shape, white on black, as ControlNet's Canny reads them",
	Takes: []string{kindVideo, kindImage},
	plan: func(o toolOrder) ([]toolOut, error) {
		d := formChoice(o.Form, "detail", edgeDetails)
		return []toolOut{{Kind: o.Source.Kind, Label: o.Source.Label + " · Edges" + edgeNames[d]}}, nil
	},
	run: func(j *toolJob) error {
		th := edgeThresholds[formChoice(j.order.Form, "detail", edgeDetails)]
		rate, expect, err := j.video()
		if err != nil {
			return err
		}
		w, h := j.outSize()
		image := j.order.Source.Kind == kindImage
		dst, n := j.work(j.ext()), 0
		err = j.r.frames.encodeRaw(j.ctx, dst, rawVideo{
			W: w, H: h, Format: "gray", Rate: rate, OutW: w, OutH: h, Image: image, Sound: soundOf(image, j.src()),
		}, func(out io.Writer) error {
			return j.r.frames.rawFrames(j.ctx, j.src(), w, h, func(rgb []byte) error {
				n++
				j.progress("Edges", n, expect)
				_, err := out.Write(canny(rgb, w, h, th[0], th[1]))
				return err
			})
		})
		if err != nil {
			return err
		}
		return j.keep(0, dst)
	},
}

// canny is OpenCV's Canny on an RGB frame, as ControlNet calls it
// (cv2.Canny(img, low, high)): a 3 by 3 Sobel on each channel with edges
// repeated, the channel with the largest L1 gradient at each pixel,
// non-maximum suppression in four directions, and hysteresis over the
// eight neighbors. Edges are 255 on 0.
func canny(rgb []byte, w, h int, low, high int32) []byte {
	dx := make([]int32, w*h)
	dy := make([]int32, w*h)
	mag := make([]int32, (w+2)*(h+2)) // a zero border, as OpenCV keeps
	at := func(x, y, c int) int32 {
		return int32(rgb[(min(max(y, 0), h-1)*w+min(max(x, 0), w-1))*3+c])
	}
	for y := range h {
		for x := range w {
			var bx, by, bm int32
			for c := range 3 {
				gx := at(x+1, y-1, c) + 2*at(x+1, y, c) + at(x+1, y+1, c) - at(x-1, y-1, c) - 2*at(x-1, y, c) - at(x-1, y+1, c)
				gy := at(x-1, y+1, c) + 2*at(x, y+1, c) + at(x+1, y+1, c) - at(x-1, y-1, c) - 2*at(x, y-1, c) - at(x+1, y-1, c)
				m := abs32(gx) + abs32(gy)
				if c == 0 || m > bm {
					bx, by, bm = gx, gy, m
				}
			}
			dx[y*w+x], dy[y*w+x], mag[(y+1)*(w+2)+x+1] = bx, by, bm
		}
	}
	// tan(22.5°) in 15-bit fixed point, as OpenCV compares directions.
	const tg22 = 13573 // round(0.41421356 * (1 << 15))
	const (
		none   = 0
		weak   = 1
		strong = 2
	)
	state := make([]byte, w*h)
	stack := make([]int, 0, 1024)
	for y := range h {
		for x := range w {
			m := mag[(y+1)*(w+2)+x+1]
			if m <= low {
				continue
			}
			gx, gy := int64(dx[y*w+x]), int64(dy[y*w+x])
			xs, ys := abs64(gx), abs64(gy)<<15
			tg22x := xs * tg22
			row := (y + 1) * (w + 2)
			up, down := row-(w+2), row+(w+2)
			var peak bool
			switch {
			case ys < tg22x:
				peak = m > mag[row+x] && m >= mag[row+x+2]
			case ys > tg22x+(xs<<16):
				peak = m > mag[up+x+1] && m >= mag[down+x+1]
			default:
				s := 1
				if (gx < 0) != (gy < 0) {
					s = -1
				}
				peak = m > mag[up+x+1-s] && m > mag[down+x+1+s]
			}
			if !peak {
				continue
			}
			if m > high {
				state[y*w+x] = strong
				stack = append(stack, y*w+x)
			} else {
				state[y*w+x] = weak
			}
		}
	}
	out := make([]byte, w*h)
	for len(stack) > 0 {
		i := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if out[i] != 0 {
			continue
		}
		out[i] = 255
		x, y := i%w, i/w
		for ny := max(y-1, 0); ny <= min(y+1, h-1); ny++ {
			for nx := max(x-1, 0); nx <= min(x+1, w-1); nx++ {
				if j := ny*w + nx; state[j] != none && out[j] == 0 {
					stack = append(stack, j)
				}
			}
		}
	}
	return out
}

func abs32(v int32) int32 {
	if v < 0 {
		return -v
	}
	return v
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
