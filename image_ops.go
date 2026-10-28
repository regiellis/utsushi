package main

import "math"

// Image helpers for the model tools, on packed RGB bytes. They follow
// OpenCV's conventions where a model's reference code calls OpenCV, since
// the models were trained on what those calls produce.

// resizeBilinear scales an RGB image as cv2.resize with INTER_LINEAR does:
// pixel centers at half steps, edges repeated.
func resizeBilinear(src []byte, w, h, nw, nh int) []byte {
	out := make([]byte, nw*nh*3)
	sx, sy := float64(w)/float64(nw), float64(h)/float64(nh)
	for y := range nh {
		fy := min(max((float64(y)+0.5)*sy-0.5, 0), float64(h-1))
		y0 := int(fy)
		y1, wy := min(y0+1, h-1), fy-float64(y0)
		for x := range nw {
			fx := min(max((float64(x)+0.5)*sx-0.5, 0), float64(w-1))
			x0 := int(fx)
			x1, wx := min(x0+1, w-1), fx-float64(x0)
			for c := range 3 {
				a := float64(src[(y0*w+x0)*3+c])*(1-wx) + float64(src[(y0*w+x1)*3+c])*wx
				b := float64(src[(y1*w+x0)*3+c])*(1-wx) + float64(src[(y1*w+x1)*3+c])*wx
				out[(y*nw+x)*3+c] = byte(math.Round(a*(1-wy) + b*wy))
			}
		}
	}
	return out
}

// sampleBilinear reads channel c at a fractional point, 0 outside the
// image, as cv2.warpAffine's constant border does.
func sampleBilinear(src []byte, w, h int, fx, fy float64, c int) float64 {
	x0, y0 := int(math.Floor(fx)), int(math.Floor(fy))
	wx, wy := fx-float64(x0), fy-float64(y0)
	at := func(x, y int) float64 {
		if x < 0 || y < 0 || x >= w || y >= h {
			return 0
		}
		return float64(src[(y*w+x)*3+c])
	}
	a := at(x0, y0)*(1-wx) + at(x0+1, y0)*wx
	b := at(x0, y0+1)*(1-wx) + at(x0+1, y0+1)*wx
	return a*(1-wy) + b*wy
}

// shortSide scales w by h so its short side is n, the long side rounded
// to even.
func shortSide(w, h, n int) (int, int) {
	if w <= h {
		return n, max(2, int(math.Round(float64(h)*float64(n)/float64(w)/2))*2)
	}
	return max(2, int(math.Round(float64(w)*float64(n)/float64(h)/2))*2), n
}

// rgbCanvas is an RGB drawing surface.
type rgbCanvas struct {
	w, h int
	px   []byte
}

func newCanvas(w, h int) *rgbCanvas { return &rgbCanvas{w: w, h: h, px: make([]byte, w*h*3)} }

func (c *rgbCanvas) set(x, y int, col [3]byte) {
	if x < 0 || y < 0 || x >= c.w || y >= c.h {
		return
	}
	copy(c.px[(y*c.w+x)*3:], col[:])
}

// circle fills a disc, as cv2.circle with thickness -1.
func (c *rgbCanvas) circle(cx, cy, r int, col [3]byte) {
	for y := cy - r; y <= cy+r; y++ {
		for x := cx - r; x <= cx+r; x++ {
			if (x-cx)*(x-cx)+(y-cy)*(y-cy) <= r*r {
				c.set(x, y, col)
			}
		}
	}
}

// ellipse fills a rotated ellipse the way cv2.ellipse2Poly (one-degree
// steps) and cv2.fillConvexPoly draw it.
func (c *rgbCanvas) ellipse(cx, cy, a, b, angle int, col [3]byte) {
	ca, sa := math.Cos(float64(angle)*math.Pi/180), math.Sin(float64(angle)*math.Pi/180)
	poly := make([][2]float64, 0, 361)
	for i := 0; i <= 360; i++ {
		t := float64(i) * math.Pi / 180
		x, y := float64(a)*math.Cos(t), float64(b)*math.Sin(t)
		poly = append(poly, [2]float64{math.Round(float64(cx) + x*ca - y*sa), math.Round(float64(cy) + x*sa + y*ca)})
	}
	c.convex(poly, col)
}

// convex fills a convex polygon, its edge pixels included.
func (c *rgbCanvas) convex(poly [][2]float64, col [3]byte) {
	minX, minY, maxX, maxY := math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)
	for _, p := range poly {
		minX, maxX = min(minX, p[0]), max(maxX, p[0])
		minY, maxY = min(minY, p[1]), max(maxY, p[1])
	}
	for y := int(minY); y <= int(maxY); y++ {
		// The polygon's span on this row: where its edges cross y.
		lo, hi := math.Inf(1), math.Inf(-1)
		fy := float64(y)
		for i := range poly {
			p, q := poly[i], poly[(i+1)%len(poly)]
			if (p[1] <= fy && q[1] >= fy) || (q[1] <= fy && p[1] >= fy) {
				var x float64
				if p[1] == q[1] {
					lo, hi = min(lo, p[0], q[0]), max(hi, p[0], q[0])
					continue
				}
				x = p[0] + (fy-p[1])*(q[0]-p[0])/(q[1]-p[1])
				lo, hi = min(lo, x), max(hi, x)
			}
		}
		for x := int(math.Ceil(lo - 0.5)); x <= int(math.Floor(hi+0.5)); x++ {
			c.set(x, y, col)
		}
	}
}

// line draws a segment thickness px wide with round ends, as cv2.line
// does for a thickness over 1.
func (c *rgbCanvas) line(x0, y0, x1, y1, thickness int, col [3]byte) {
	r := float64(thickness) / 2
	dx, dy := float64(x1-x0), float64(y1-y0)
	l2 := dx*dx + dy*dy
	pad := int(math.Ceil(r))
	for y := min(y0, y1) - pad; y <= max(y0, y1)+pad; y++ {
		for x := min(x0, x1) - pad; x <= max(x0, x1)+pad; x++ {
			t := 0.0
			if l2 > 0 {
				t = min(max((float64(x-x0)*dx+float64(y-y0)*dy)/l2, 0), 1)
			}
			ex, ey := float64(x0)+t*dx-float64(x), float64(y0)+t*dy-float64(y)
			if ex*ex+ey*ey <= r*r {
				c.set(x, y, col)
			}
		}
	}
}

// dim scales every pixel by f, truncating, as numpy's astype(uint8) does.
func (c *rgbCanvas) dim(f float64) {
	for i, v := range c.px {
		c.px[i] = byte(float64(v) * f)
	}
}

// hsvColor is matplotlib's hsv_to_rgb for full saturation and value, as
// 0..255, rounded as OpenCV rounds a float color.
func hsvColor(hue float64) [3]byte {
	h := hue * 6
	i := int(math.Floor(h)) % 6
	f := h - math.Floor(h)
	var r, g, b float64
	switch i {
	case 0:
		r, g, b = 1, f, 0
	case 1:
		r, g, b = 1-f, 1, 0
	case 2:
		r, g, b = 0, 1, f
	case 3:
		r, g, b = 0, 1-f, 1
	case 4:
		r, g, b = f, 0, 1
	default:
		r, g, b = 1, 0, 1-f
	}
	return [3]byte{byte(math.Round(r * 255)), byte(math.Round(g * 255)), byte(math.Round(b * 255))}
}
