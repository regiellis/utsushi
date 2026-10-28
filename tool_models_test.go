package main

import (
	"math"
	"testing"
)

func TestModnetSizeFollowsItsInferenceScript(t *testing.T) {
	for _, c := range []struct{ w, h, ww, wh int }{
		{480, 854, 480, 832},   // one side either way of 512: kept, then floored to 32
		{1080, 1920, 512, 896}, // larger on every side: short side to 512 (910), floored
		{300, 200, 768, 512},   // smaller on every side: short side to 512
		{512, 512, 512, 512},
	} {
		if w, h := modnetSize(c.w, c.h); w != c.ww || h != c.wh {
			t.Errorf("modnetSize(%d, %d) = %d, %d; want %d, %d", c.w, c.h, w, h, c.ww, c.wh)
		}
	}
}

func TestScaleRate(t *testing.T) {
	for rate, want := range map[string]string{"24/1": "48/1", "30000/1001": "60000/1001", "25": "50/1"} {
		if got, err := scaleRate(rate, 2); err != nil || got != want {
			t.Errorf("scaleRate(%q, 2) = %q, %v; want %q", rate, got, err, want)
		}
	}
	if _, err := scaleRate("x/1", 2); err == nil {
		t.Error("an unreadable rate scaled")
	}
}

func TestFrameSSIMTellsACutFromTheSameShot(t *testing.T) {
	const w, h = 64, 48
	a, b, moved := make([]byte, w*h*3), make([]byte, w*h*3), make([]byte, w*h*3)
	for y := range h {
		for x := range w {
			p := (y*w + x) * 3
			a[p], a[p+1], a[p+2] = byte(x*4), byte(y*5), 90
			// The same picture a pixel to the right.
			moved[p], moved[p+1], moved[p+2] = byte(max(x-1, 0)*4), byte(y*5), 90
			// Another shot: the gradient the other way.
			b[p], b[p+1], b[p+2] = byte(255-x*4), byte(255-y*5), 20
		}
	}
	if s := frameSSIM(a, a, w, h); math.Abs(s-1) > 1e-9 {
		t.Errorf("a frame against itself = %v, want 1", s)
	}
	if s := frameSSIM(a, moved, w, h); s < 0.9 {
		t.Errorf("a small move = %v, want near 1", s)
	}
	if s := frameSSIM(a, b, w, h); s >= interpCut {
		t.Errorf("a cut = %v, want under %v", s, interpCut)
	}
}

func TestFillRIFEPadsAndCropPlanarTakesTheCornerBack(t *testing.T) {
	const w, h, pw, ph = 3, 2, 32, 32
	a := []byte{255, 0, 0, 0, 255, 0, 0, 0, 255, 255, 255, 255, 0, 0, 0, 128, 128, 128}
	b := make([]byte, len(a))
	in := make([]float32, 7*pw*ph)
	fillRIFE(in, a, b, w, h, pw, ph, 0.25)
	plane := pw * ph
	if in[0] != 1 || in[plane+1] != 1 || in[2*plane+2] != 1 {
		t.Fatalf("frame A's channels are not in planes 0 to 2: %v %v %v", in[0], in[plane+1], in[2*plane+2])
	}
	if in[w] != 0 || in[pw*h] != 0 {
		t.Fatal("the padding is not black")
	}
	if in[6*plane] != 0.25 || in[7*plane-1] != 0.25 {
		t.Fatal("the time plane is not filled")
	}
	got := cropPlanar(in[:3*plane], pw, ph, w, h)
	for i := range a {
		if got[i] != a[i] {
			t.Fatalf("crop byte %d = %d, want %d", i, got[i], a[i])
		}
	}
}

func TestIOUUsesThePlusOneConvention(t *testing.T) {
	if v := iou([4]float64{0, 0, 9, 9}, [4]float64{0, 0, 9, 9}); v != 1 {
		t.Errorf("same box = %v", v)
	}
	// Two 10 by 10 boxes sharing a 5 by 10 strip: 50 / (100 + 100 - 50).
	if v := iou([4]float64{0, 0, 9, 9}, [4]float64{5, 0, 14, 9}); math.Abs(v-1.0/3) > 1e-9 {
		t.Errorf("half overlap = %v, want 1/3", v)
	}
}

func TestOpenposeBodyMapsCOCOAndAddsTheNeck(t *testing.T) {
	kps := make([]keypoint, 133)
	for i := range kps {
		kps[i] = keypoint{float64(i), float64(i), 0.9}
	}
	body := openposeBody(kps)
	// OpenPose: 0 nose, 1 neck, 2 right shoulder (COCO 6), 5 left shoulder
	// (COCO 5), 14 right eye (COCO 2), 17 left ear (COCO 3).
	if body[0].x != 0 || body[2].x != 6 || body[5].x != 5 || body[14].x != 2 || body[17].x != 3 {
		t.Fatalf("mapping = %v", body)
	}
	if body[1].x != 5.5 || body[1].score != 1 {
		t.Fatalf("neck = %+v, want the shoulders' middle, seen", body[1])
	}
	kps[6].score = 0.1
	if openposeBody(kps)[1].score != 0 {
		t.Fatal("a neck with one shoulder unseen was drawn")
	}
}

func TestDrawPoseDimsLimbsAndKeepsJointsBright(t *testing.T) {
	const w, h = 200, 200
	kps := make([]keypoint, 133)
	for i := range kps {
		kps[i] = keypoint{-1, -1, 0}
	}
	// A right shoulder and elbow: limb 3 to 4 in OpenPose (index 2),
	// colored [255, 170, 0], its joints colored by joint.
	kps[6] = keypoint{50, 100, 0.9}
	kps[8] = keypoint{150, 100, 0.9}
	c := drawPose([][]keypoint{kps}, w, h)
	at := func(x, y int) [3]byte {
		p := (y*w + x) * 3
		return [3]byte{c.px[p], c.px[p+1], c.px[p+2]}
	}
	if got, want := at(100, 100), [3]byte{153, 102, 0}; got != want {
		t.Errorf("the limb's middle = %v, want %v (255, 170, 0 at 0.6)", got, want)
	}
	if got, want := at(50, 100), openposeColors[2]; got != want {
		t.Errorf("the shoulder joint = %v, want %v at full brightness", got, want)
	}
	if got := at(100, 120); got != [3]byte{} {
		t.Errorf("off the limb = %v, want black", got)
	}
}
