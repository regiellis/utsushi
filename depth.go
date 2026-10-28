package main

import (
	"errors"
	"fmt"
	"io"
	"math"

	ort "github.com/yalue/onnxruntime_go"
)

// Video Depth Anything's export and its reference inference settings
// (video_depth.py, infer_video_depth: "do not change").
const (
	vdaW, vdaH = 910, 518
	vdaTokens  = 2405 // 37 by 65 patches of 14 px
	vdaDim     = 384
	vdaLen     = 32 // INFER_LEN, frames per window
	vdaOverlap = 10 // OVERLAP, inputs a window takes from the one before
	vdaInterp  = 8  // INTERP_LEN, frames blended where windows meet
	vdaAlign   = vdaOverlap - vdaInterp
	// dav2Short is Depth Anything V2's input short side.
	dav2Short = 518
)

// vdaKeyframes are the inputs of one window that open the next.
var vdaKeyframes = [vdaOverlap]int{0, 12, 24, 25, 26, 27, 28, 29, 30, 31}

var (
	imagenetMean = [3]float32{0.485, 0.456, 0.406}
	imagenetStd  = [3]float32{0.229, 0.224, 0.225}
)

// videoNet is Video Depth Anything: an encoder run once per frame and a
// temporal head run once per window. The features are opaque here.
type videoNet interface {
	encode(pixels []float32) ([]float32, error)
	head(window [][]float32) ([][]float32, error)
	close() error
}

// frameNet is a single-image depth model.
type frameNet interface {
	depth(pixels []float32, w, h int) ([]float32, error)
	close() error
}

// videoDepth runs Video Depth Anything over a clip the way its reference
// does: 32-frame windows stepping 22 frames, each window opening on the
// one before's keyframes, scaled and shifted onto it by least squares over
// two of them, the 8 frames they share blended linearly. next returns
// io.EOF after the last frame; the clip is padded with its last frame to
// fill the final window. crop turns the model's depth into the frame's,
// and emit gets every frame's depth in order, once it is final.
func videoDepth(net videoNet, next func() ([]float32, error), crop func([]float32) []float32, emit func([]float32) error) error {
	var (
		feats    = map[int][]float32{} // encoded frames a window still needs
		read     int                   // frames read
		eof      bool
		lastFeat []float32
	)
	feat := func(i int) ([]float32, error) {
		for !eof && read <= i {
			px, err := next()
			if errors.Is(err, io.EOF) {
				eof = true
				break
			}
			if err != nil {
				return nil, err
			}
			f, err := net.encode(px)
			if err != nil {
				return nil, err
			}
			feats[read], lastFeat = f, f
			read++
		}
		if i < read {
			return feats[i], nil
		}
		return lastFeat, nil
	}
	var (
		prevIn  [][]float32
		pending [][]float32 // aligned frames not yet emitted; the last vdaInterp can still change
		ref     [vdaAlign][]float32
		emitted int
	)
	flush := func(keep int) error {
		for len(pending) > keep && (!eof || emitted < read) {
			if err := emit(pending[0]); err != nil {
				return err
			}
			pending = pending[1:]
			emitted++
		}
		return nil
	}
	for k := 0; ; k++ {
		start := k * (vdaLen - vdaOverlap)
		if _, err := feat(start); err != nil {
			return err
		}
		if start >= read {
			break
		}
		in := make([][]float32, vdaLen)
		for i := range in {
			if prevIn != nil && i < vdaOverlap {
				in[i] = prevIn[vdaKeyframes[i]]
				continue
			}
			f, err := feat(start + i)
			if err != nil {
				return err
			}
			in[i] = f
		}
		out, err := net.head(in)
		if err != nil {
			return err
		}
		if len(out) != vdaLen {
			return fmt.Errorf("the depth head returned %d frames, not %d", len(out), vdaLen)
		}
		for i := range out {
			out[i] = crop(out[i])
		}
		if prevIn == nil {
			pending = append(pending, out...)
			ref = [vdaAlign][]float32{out[0], out[vdaKeyframes[1]]}
		} else {
			scale, shift := scaleShift(concat(out[:vdaAlign]...), concat(ref[:]...))
			post := make([][]float32, 0, vdaInterp)
			for _, d := range out[vdaAlign:vdaOverlap] {
				post = append(post, affine(d, scale, shift))
			}
			blend(pending[len(pending)-vdaInterp:], post)
			for _, d := range out[vdaOverlap:] {
				pending = append(pending, affine(d, scale, shift))
			}
			ref = [vdaAlign][]float32{ref[0], affine(out[vdaKeyframes[1]], scale, shift)}
		}
		prevIn = in
		// The next window's new frames start after its keyframe inputs.
		for i := range feats {
			if i < start+(vdaLen-vdaOverlap)+vdaOverlap {
				delete(feats, i)
			}
		}
		if err := flush(vdaInterp); err != nil {
			return err
		}
	}
	return flush(0)
}

// frameDepth runs a single-image model on every frame. Each frame's depth
// keeps its own scale; depthRange evens them out when they are rendered.
func frameDepth(net frameNet, w, h int, next func() ([]float32, error), emit func([]float32) error) error {
	for {
		px, err := next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		d, err := net.depth(px, w, h)
		if err != nil {
			return err
		}
		if err := emit(d); err != nil {
			return err
		}
	}
}

// scaleShift is the least-squares scale and shift taking pred onto target
// (utils/util.py, compute_scale_and_shift_full), 1 and 0 when singular.
func scaleShift(pred, target []float32) (float32, float32) {
	var a00, a01, a11, b0, b1 float64
	for i, p := range pred {
		p, t := float64(p), float64(target[i])
		a00 += p * p
		a01 += p
		a11++
		b0 += p * t
		b1 += t
	}
	det := a00*a11 - a01*a01
	if det == 0 {
		return 1, 0
	}
	return float32((a11*b0 - a01*b1) / det), float32((-a01*b0 + a00*b1) / det)
}

// affine is d*scale+shift, negatives clamped to 0, as a new frame.
func affine(d []float32, scale, shift float32) []float32 {
	out := make([]float32, len(d))
	for i, v := range d {
		out[i] = max(v*scale+shift, 0)
	}
	return out
}

// blend moves pre toward post in place, weight 0 at the first frame to 1
// at the last (get_interpolate_frames).
func blend(pre, post [][]float32) {
	step := 1 / float32(len(pre)-1)
	for i := range pre {
		w := float32(i) * step
		mixed := make([]float32, len(pre[i]))
		for j := range mixed {
			mixed[j] = pre[i][j]*(1-w) + post[i][j]*w
		}
		pre[i] = mixed
	}
}

func concat(frames ...[]float32) []float32 {
	n := 0
	for _, f := range frames {
		n += len(f)
	}
	out := make([]float32, 0, n)
	for _, f := range frames {
		out = append(out, f...)
	}
	return out
}

// fitBox is the size a w by h frame takes inside bw by bh at its own shape.
func fitBox(w, h, bw, bh int) (int, int) {
	s := min(float64(bw)/float64(w), float64(bh)/float64(h))
	fw, fh := int(math.Round(float64(w)*s)), int(math.Round(float64(h)*s))
	return min(max(fw, 1), bw), min(max(fh, 1), bh)
}

// dav2Size is Depth Anything V2's input for a w by h frame: its shape kept,
// both sides at least 518 and multiples of 14 (Resize, lower_bound,
// ensure_multiple_of 14, in the model's own transform).
func dav2Size(w, h int) (int, int) {
	s := max(dav2Short/float64(w), dav2Short/float64(h))
	return multipleOf14(float64(w)*s, dav2Short), multipleOf14(float64(h)*s, dav2Short)
}

func multipleOf14(x float64, least int) int {
	y := int(math.Round(x/14)) * 14
	if y < least {
		y = int(math.Ceil(x/14)) * 14
	}
	return y
}

// planar writes a w by h RGB frame into a bw by bh ImageNet-normalized
// planar tensor, centered, its edge pixels repeated into the border, and
// returns where the frame sits.
func planar(dst []float32, rgb []byte, w, h, bw, bh int) (ox, oy int) {
	ox, oy = (bw-w)/2, (bh-h)/2
	plane := bw * bh
	for y := range bh {
		sy := min(max(y-oy, 0), h-1)
		for x := range bw {
			sx := min(max(x-ox, 0), w-1)
			p, i := (sy*w+sx)*3, y*bw+x
			for c := range 3 {
				dst[c*plane+i] = (float32(rgb[p+c])/255 - imagenetMean[c]) / imagenetStd[c]
			}
		}
	}
	return ox, oy
}

// cropPlane cuts the w by h frame at ox, oy out of a plane bw wide.
func cropPlane(src []float32, bw, ox, oy, w, h int) []float32 {
	out := make([]float32, w*h)
	for y := range h {
		copy(out[y*w:(y+1)*w], src[(oy+y)*bw+ox:])
	}
	return out
}

// ortVDA runs the two halves of the Video Depth Anything export on tensors
// made once and reused for every frame and window.
type ortVDA struct {
	enc, dec *ort.AdvancedSession
	px       *ort.Tensor[float32]
	feats    [4]*ort.Tensor[float32] // 1 by 2405 by 384 each
	window   [4]*ort.Tensor[float32] // 32 by 2405 by 384 each
	out      *ort.Tensor[float32]    // 32 by 1 by 518 by 910
}

var vdaFeatNames = []string{"feat1", "feat2", "feat3", "feat4"}

func newORTVDA(encoder, head string) (_ *ortVDA, err error) {
	n := &ortVDA{}
	defer func() {
		if err != nil {
			err = errors.Join(err, n.close())
		}
	}()
	if n.px, err = ort.NewEmptyTensor[float32](ort.NewShape(1, 3, vdaH, vdaW)); err != nil {
		return nil, err
	}
	encOut := make([]ort.Value, 4)
	headIn := make([]ort.Value, 4)
	for i := range 4 {
		if n.feats[i], err = ort.NewEmptyTensor[float32](ort.NewShape(1, vdaTokens, vdaDim)); err != nil {
			return nil, err
		}
		if n.window[i], err = ort.NewEmptyTensor[float32](ort.NewShape(vdaLen, vdaTokens, vdaDim)); err != nil {
			return nil, err
		}
		encOut[i], headIn[i] = n.feats[i], n.window[i]
	}
	if n.out, err = ort.NewEmptyTensor[float32](ort.NewShape(vdaLen, 1, vdaH, vdaW)); err != nil {
		return nil, err
	}
	if n.enc, err = withOptions(func(o *ort.SessionOptions) (*ort.AdvancedSession, error) {
		return ort.NewAdvancedSession(encoder, []string{"pixels"}, vdaFeatNames, []ort.Value{n.px}, encOut, o)
	}); err != nil {
		return nil, fmt.Errorf("load the depth encoder: %w", err)
	}
	if n.dec, err = withOptions(func(o *ort.SessionOptions) (*ort.AdvancedSession, error) {
		return ort.NewAdvancedSession(head, vdaFeatNames, []string{"depth"}, headIn, []ort.Value{n.out}, o)
	}); err != nil {
		return nil, fmt.Errorf("load the depth head: %w", err)
	}
	return n, nil
}

// encode returns one frame's four feature maps end to end.
func (n *ortVDA) encode(pixels []float32) ([]float32, error) {
	copy(n.px.GetData(), pixels)
	if err := n.enc.Run(); err != nil {
		return nil, fmt.Errorf("depth encoder: %w", err)
	}
	out := make([]float32, 0, 4*vdaTokens*vdaDim)
	for _, f := range n.feats {
		out = append(out, f.GetData()...)
	}
	return out, nil
}

func (n *ortVDA) head(window [][]float32) ([][]float32, error) {
	const size = vdaTokens * vdaDim
	for j, t := range n.window {
		data := t.GetData()
		for i, f := range window {
			copy(data[i*size:(i+1)*size], f[j*size:(j+1)*size])
		}
	}
	if err := n.dec.Run(); err != nil {
		return nil, fmt.Errorf("depth head: %w", err)
	}
	const plane = vdaW * vdaH
	data := n.out.GetData()
	out := make([][]float32, vdaLen)
	for i := range out {
		out[i] = append([]float32(nil), data[i*plane:(i+1)*plane]...)
	}
	return out, nil
}

func (n *ortVDA) close() error {
	var errs []error
	for _, s := range []*ort.AdvancedSession{n.enc, n.dec} {
		if s != nil {
			errs = append(errs, s.Destroy())
		}
	}
	for _, t := range append([]*ort.Tensor[float32]{n.px, n.out}, append(n.feats[:], n.window[:]...)...) {
		if t != nil {
			errs = append(errs, t.Destroy())
		}
	}
	return errors.Join(errs...)
}

// ortDAV2 runs Depth Anything V2, whose input size follows the frame.
type ortDAV2 struct {
	s *ort.DynamicAdvancedSession
}

func newORTDAV2(model string) (*ortDAV2, error) {
	s, err := withOptions(func(o *ort.SessionOptions) (*ort.DynamicAdvancedSession, error) {
		return ort.NewDynamicAdvancedSession(model, []string{"pixel_values"}, []string{"predicted_depth"}, o)
	})
	if err != nil {
		return nil, fmt.Errorf("load the depth model: %w", err)
	}
	return &ortDAV2{s: s}, nil
}

func (n *ortDAV2) depth(pixels []float32, w, h int) (_ []float32, err error) {
	in, err := ort.NewTensor(ort.NewShape(1, 3, int64(h), int64(w)), pixels)
	if err != nil {
		return nil, err
	}
	outs := []ort.Value{nil}
	defer func() {
		err = errors.Join(err, in.Destroy())
		if outs[0] != nil {
			err = errors.Join(err, outs[0].Destroy())
		}
	}()
	if err := n.s.Run([]ort.Value{in}, outs); err != nil {
		return nil, fmt.Errorf("depth model: %w", err)
	}
	t, ok := outs[0].(*ort.Tensor[float32])
	if !ok {
		return nil, fmt.Errorf("the depth model returned %T, not float32", outs[0])
	}
	if got := t.GetShape().FlattenedSize(); got != int64(w*h) {
		return nil, fmt.Errorf("the depth model returned %d values for a %dx%d frame", got, w, h)
	}
	return append([]float32(nil), t.GetData()...), nil
}

func (n *ortDAV2) close() error { return n.s.Destroy() }
