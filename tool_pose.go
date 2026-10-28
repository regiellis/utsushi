package main

import (
	"errors"
	"io"
	"math"
	"slices"
)

// poseTool draws each frame's people as OpenPose skeletons with hands and
// face, the way ControlNet's DWPose annotator does (its ControlNet onnx
// branch, commit 3dca5db, annotator/dwpose), for engines driven by pose.
var poseTool = &tool{
	ID: "pose", Name: "Pose", Code: "PS", Action: "Make pose", Model: true,
	Blurb: "Skeletons of every person, with hands and face, as DWPose draws them",
	Takes: []string{kindVideo, kindImage},
	plan: func(o toolOrder) ([]toolOut, error) {
		return []toolOut{{Kind: o.Source.Kind, Label: o.Source.Label + " · Pose"}}, nil
	},
	run: runPose,
}

// poseDraw is the short side poses are found and drawn at, the annotator's
// usual detect resolution; the result is scaled to the source after, so
// limbs keep the width the control models were trained on.
const poseDraw = 512

const (
	yoloxSize   = 640
	poseW       = 288
	poseH       = 384
	poseSplit   = 2.0 // SimCC split ratio
	poseMinBox  = 0.3 // a person's box score
	poseNMS     = 0.45
	poseVisible = 0.3 // a keypoint's score
)

func runPose(j *toolJob) error {
	m := j.r.tools.models
	if err := j.models(modelFilesFor("pose")...); err != nil {
		return err
	}
	det, err := newONNX("person detector", m.path(yoloxL), []string{"images"}, []string{"output"})
	if err != nil {
		return err
	}
	est, err := newONNX("pose", m.path(dwPose), []string{"input"}, []string{"simcc_x", "simcc_y"})
	if err != nil {
		return errors.Join(err, det.close())
	}
	defer func() {
		if err := errors.Join(det.close(), est.close()); err != nil {
			j.note(true, "Closing the pose models: %v", err)
		}
	}()
	rate, expect, err := j.video()
	if err != nil {
		return err
	}
	w, h := shortSide(j.order.Source.Width, j.order.Source.Height, poseDraw)
	outW, outH := j.outSize()
	image := j.order.Source.Kind == kindImage
	dst, n := j.work(j.ext()), 0
	err = j.r.frames.encodeRaw(j.ctx, dst, rawVideo{
		W: w, H: h, Format: "rgb24", Rate: rate, OutW: outW, OutH: outH, Image: image, Sound: soundOf(image, j.src()),
	}, func(out io.Writer) error {
		return j.r.frames.rawFrames(j.ctx, j.src(), w, h, func(rgb []byte) error {
			n++
			j.progress("Pose", n, expect)
			people, err := findPeople(det, est, rgb, w, h)
			if err != nil {
				return err
			}
			_, err = out.Write(drawPose(people, w, h).px)
			return err
		})
	})
	if err != nil {
		return err
	}
	return j.keep(0, dst)
}

// keypoint is a point in frame pixels with its score.
type keypoint struct{ x, y, score float64 }

// findPeople returns each person's 133 COCO-WholeBody keypoints.
func findPeople(det, est *onnxNet, rgb []byte, w, h int) ([][]keypoint, error) {
	boxes, err := detectPeople(det, rgb, w, h)
	if err != nil {
		return nil, err
	}
	input := make([]float32, 0, len(boxes)*3*poseW*poseH)
	centers := make([][2]float64, len(boxes))
	scales := make([][2]float64, len(boxes))
	for i, b := range boxes {
		px, c, s := poseCrop(rgb, w, h, b)
		input = append(input, px...)
		centers[i], scales[i] = c, s
	}
	res, err := est.run(onnxIn{shape: []int64{int64(len(boxes)), 3, poseH, poseW}, data: input})
	if err != nil {
		return nil, err
	}
	sx, sy := res[0], res[1]
	k, lx, ly := int(sx.shape[1]), int(sx.shape[2]), int(sy.shape[2])
	people := make([][]keypoint, len(boxes))
	for p := range boxes {
		kps := make([]keypoint, k)
		for i := range k {
			rowX := sx.data[(p*k+i)*lx : (p*k+i+1)*lx]
			rowY := sy.data[(p*k+i)*ly : (p*k+i+1)*ly]
			ix, iy := argmax(rowX), argmax(rowY)
			score := float64(min(rowX[ix], rowY[iy]))
			if score <= 0 {
				kps[i] = keypoint{-1, -1, score}
				continue
			}
			x, y := float64(ix)/poseSplit, float64(iy)/poseSplit
			kps[i] = keypoint{
				x:     x/poseW*scales[p][0] + centers[p][0] - scales[p][0]/2,
				y:     y/poseH*scales[p][1] + centers[p][1] - scales[p][1]/2,
				score: score,
			}
		}
		people[p] = kps
	}
	return people, nil
}

func argmax(v []float32) int {
	best := 0
	for i, x := range v {
		if x > v[best] {
			best = i
		}
	}
	return best
}

// detectPeople runs YOLOX as DWPose feeds it, the RGB frame scaled into
// the top left of a 640 square of gray 114, and returns person boxes
// (x1, y1, x2, y2) in frame pixels, or the whole frame when it finds none.
func detectPeople(det *onnxNet, rgb []byte, w, h int) ([][4]float64, error) {
	r := min(float64(yoloxSize)/float64(h), float64(yoloxSize)/float64(w))
	rw, rh := int(float64(w)*r), int(float64(h)*r)
	small := resizeBilinear(rgb, w, h, rw, rh)
	const plane = yoloxSize * yoloxSize
	in := make([]float32, 3*plane)
	for i := range in {
		in[i] = 114
	}
	for y := range rh {
		for x := range rw {
			for c := range 3 {
				in[c*plane+y*yoloxSize+x] = float32(small[(y*rw+x)*3+c])
			}
		}
	}
	res, err := det.run(onnxIn{shape: []int64{1, 3, yoloxSize, yoloxSize}, data: in})
	if err != nil {
		return nil, err
	}
	out := res[0].data
	type scored struct {
		box   [4]float64
		score float64
	}
	var found []scored
	row := 0
	for _, s := range []int{8, 16, 32} {
		g := yoloxSize / s
		for gy := range g {
			for gx := range g {
				o := out[row*85 : (row+1)*85]
				row++
				// Objectness and class scores come out of the graph already
				// through a sigmoid.
				score := float64(o[4]) * float64(o[5])
				if score <= poseMinBox {
					continue
				}
				cx, cy := (float64(o[0])+float64(gx))*float64(s), (float64(o[1])+float64(gy))*float64(s)
				bw, bh := math.Exp(float64(o[2]))*float64(s), math.Exp(float64(o[3]))*float64(s)
				found = append(found, scored{[4]float64{(cx - bw/2) / r, (cy - bh/2) / r, (cx + bw/2) / r, (cy + bh/2) / r}, score})
			}
		}
	}
	slices.SortStableFunc(found, func(a, b scored) int {
		switch {
		case a.score > b.score:
			return -1
		case a.score < b.score:
			return 1
		}
		return 0
	})
	var kept [][4]float64
	for _, f := range found {
		if !slices.ContainsFunc(kept, func(k [4]float64) bool { return iou(k, f.box) > poseNMS }) {
			kept = append(kept, f.box)
		}
	}
	if len(kept) == 0 {
		kept = [][4]float64{{0, 0, float64(w), float64(h)}}
	}
	return kept, nil
}

// iou is two boxes' overlap, with the +1 pixel convention DWPose's NMS
// uses.
func iou(a, b [4]float64) float64 {
	area := func(x [4]float64) float64 { return (x[2] - x[0] + 1) * (x[3] - x[1] + 1) }
	iw := max(0, min(a[2], b[2])-max(a[0], b[0])+1)
	ih := max(0, min(a[3], b[3])-max(a[1], b[1])+1)
	inter := iw * ih
	return inter / (area(a) + area(b) - inter)
}

// poseCrop warps a person's box, padded by a quarter and set to 3:4, into
// the pose model's 288 by 384 input, normalized, and returns the box's
// center and size for mapping keypoints back.
func poseCrop(rgb []byte, w, h int, b [4]float64) ([]float32, [2]float64, [2]float64) {
	center := [2]float64{(b[0] + b[2]) / 2, (b[1] + b[3]) / 2}
	sw, sh := (b[2]-b[0])*1.25, (b[3]-b[1])*1.25
	const aspect = float64(poseW) / poseH
	if sw > sh*aspect {
		sh = sw / aspect
	} else {
		sw = sh * aspect
	}
	k := poseW / sw
	mean := [3]float64{123.675, 116.28, 103.53}
	std := [3]float64{58.395, 57.12, 57.375}
	const plane = poseW * poseH
	out := make([]float32, 3*plane)
	for y := range poseH {
		fy := (float64(y)-poseH/2)/k + center[1]
		for x := range poseW {
			fx := (float64(x)-poseW/2)/k + center[0]
			for c := range 3 {
				out[c*plane+y*poseW+x] = float32((sampleBilinear(rgb, w, h, fx, fy, c) - mean[c]) / std[c])
			}
		}
	}
	return out, center, [2]float64{sw, sh}
}

// OpenPose's body as DWPose draws it: 18 joints, limbs between them
// (1-based), and one color per limb and joint.
var (
	openposeLimbs = [17][2]int{{2, 3}, {2, 6}, {3, 4}, {4, 5}, {6, 7}, {7, 8}, {2, 9}, {9, 10},
		{10, 11}, {2, 12}, {12, 13}, {13, 14}, {2, 1}, {1, 15}, {15, 17}, {1, 16}, {16, 18}}
	openposeColors = [18][3]byte{{255, 0, 0}, {255, 85, 0}, {255, 170, 0}, {255, 255, 0}, {170, 255, 0}, {85, 255, 0},
		{0, 255, 0}, {0, 255, 85}, {0, 255, 170}, {0, 255, 255}, {0, 170, 255}, {0, 85, 255}, {0, 0, 255},
		{85, 0, 255}, {170, 0, 255}, {255, 0, 255}, {255, 0, 170}, {255, 0, 85}}
	handEdges = [20][2]int{{0, 1}, {1, 2}, {2, 3}, {3, 4}, {0, 5}, {5, 6}, {6, 7}, {7, 8}, {0, 9}, {9, 10},
		{10, 11}, {11, 12}, {0, 13}, {13, 14}, {14, 15}, {15, 16}, {0, 17}, {17, 18}, {18, 19}, {19, 20}}
	// COCO-WholeBody body joints in OpenPose's order; -1 is the neck.
	openposeFromCOCO = [18]int{0, -1, 6, 8, 10, 5, 7, 9, 12, 14, 16, 11, 13, 15, 2, 1, 4, 3}
)

// openposeBody is a person's 18 OpenPose joints. The neck is the middle
// of the shoulders, seen only when both are.
func openposeBody(kps []keypoint) [18]keypoint {
	var body [18]keypoint
	for i, c := range openposeFromCOCO {
		if c >= 0 {
			body[i] = kps[c]
			continue
		}
		l, r := kps[5], kps[6]
		score := 0.0
		if l.score > poseVisible && r.score > poseVisible {
			score = 1
		}
		body[i] = keypoint{(l.x + r.x) / 2, (l.y + r.y) / 2, score}
	}
	return body
}

// drawPose draws bodies, then hands, then faces, on black, as DWPose's
// draw_pose does. Keypoints under the score are left out. Feet are not
// drawn.
func drawPose(people [][]keypoint, w, h int) *rgbCanvas {
	c := newCanvas(w, h)
	seen := func(k keypoint) bool { return k.score > poseVisible }
	bodies := make([][18]keypoint, len(people))
	for i, p := range people {
		bodies[i] = openposeBody(p)
	}
	for li, limb := range openposeLimbs {
		for _, b := range bodies {
			p, q := b[limb[0]-1], b[limb[1]-1]
			if !seen(p) || !seen(q) {
				continue
			}
			length := math.Hypot(p.x-q.x, p.y-q.y)
			angle := math.Atan2(p.y-q.y, p.x-q.x) * 180 / math.Pi
			c.ellipse(int((p.x+q.x)/2), int((p.y+q.y)/2), int(length/2), 4, int(angle), openposeColors[li])
		}
	}
	c.dim(0.6)
	for ji := range 18 {
		for _, b := range bodies {
			if seen(b[ji]) {
				c.circle(int(b[ji].x), int(b[ji].y), 4, openposeColors[ji])
			}
		}
	}
	// Left hands, then right hands; a point is drawn only inside the frame.
	inside := func(k keypoint) bool { return seen(k) && int(k.x) > 0 && int(k.y) > 0 }
	for _, from := range []int{91, 112} {
		for _, p := range people {
			hand := p[from : from+21]
			for ei, e := range handEdges {
				a, b := hand[e[0]], hand[e[1]]
				if inside(a) && inside(b) {
					c.line(int(a.x), int(a.y), int(b.x), int(b.y), 2, hsvColor(float64(ei)/float64(len(handEdges))))
				}
			}
			for _, k := range hand {
				if inside(k) {
					c.circle(int(k.x), int(k.y), 4, [3]byte{0, 0, 255})
				}
			}
		}
	}
	for _, p := range people {
		for _, k := range p[23:91] {
			if inside(k) {
				c.circle(int(k.x), int(k.y), 3, [3]byte{255, 255, 255})
			}
		}
	}
	return c
}
