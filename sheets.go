package main

import (
	"bytes"
	"errors"
	"image"
	"image/png"
	"os"
	"path/filepath"
)

// frontFigure finds a character sheet's first figure. A sheet is several
// figures side by side on a plain background (the Create image dialog's
// sheets, and most turnaround art); used whole as a reference, a model may
// draw each figure as another person. Anything else reports false and is
// sent whole: the shape alone cannot tell, since sheets are as wide as
// landscape photos.
func frontFigure(img image.Image) (image.Rectangle, bool) {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w < 64 || h < 64 {
		return image.Rectangle{}, false
	}
	// The background is the corners' color, and the corners must agree.
	patch := max(4, min(w, h)/50)
	var corners [4][3]float64
	for i, c := range []image.Point{{b.Min.X, b.Min.Y}, {b.Max.X - patch, b.Min.Y}, {b.Min.X, b.Max.Y - patch}, {b.Max.X - patch, b.Max.Y - patch}} {
		var sum [3]float64
		for y := c.Y; y < c.Y+patch; y++ {
			for x := c.X; x < c.X+patch; x++ {
				r, g, bl, _ := img.At(x, y).RGBA()
				sum[0], sum[1], sum[2] = sum[0]+float64(r>>8), sum[1]+float64(g>>8), sum[2]+float64(bl>>8)
			}
		}
		n := float64(patch * patch)
		corners[i] = [3]float64{sum[0] / n, sum[1] / n, sum[2] / n}
	}
	bg := corners[0]
	for _, c := range corners[1:] {
		if colorDistance(c, bg) > 24 {
			return image.Rectangle{}, false
		}
	}
	// A column holds a figure when enough of it differs from the background.
	figure := make([]bool, w)
	for x := range w {
		differ := 0
		for y := b.Min.Y; y < b.Max.Y; y += 2 {
			r, g, bl, _ := img.At(b.Min.X+x, y).RGBA()
			if colorDistance([3]float64{float64(r >> 8), float64(g >> 8), float64(bl >> 8)}, bg) > 40 {
				differ++
			}
		}
		figure[x] = differ*2 > h/50
	}
	// Runs of figure columns, joining small gaps and dropping slivers.
	type run struct{ start, end int }
	var runs []run
	for x := 0; x < w; x++ {
		if !figure[x] {
			continue
		}
		start := x
		for x < w && figure[x] {
			x++
		}
		if len(runs) > 0 && start-runs[len(runs)-1].end < w/100 {
			runs[len(runs)-1].end = x
			continue
		}
		runs = append(runs, run{start, x})
	}
	var figures []run
	for _, r := range runs {
		if r.end-r.start >= w/50 {
			figures = append(figures, r)
		}
	}
	if len(figures) < 3 {
		return image.Rectangle{}, false
	}
	first, next := figures[0], figures[1]
	margin := min(w/50, (next.start-first.end)/2)
	return image.Rect(b.Min.X+max(0, first.start-margin), b.Min.Y, b.Min.X+min(w, first.end+margin), b.Max.Y), true
}

func colorDistance(a, b [3]float64) float64 {
	d := 0.0
	for i := range a {
		if v := a[i] - b[i]; v > 0 {
			d += v
		} else {
			d -= v
		}
	}
	return d / 3
}

// frontView is the file to send for a cast image: a sheet's first figure,
// cut once into the cache, or the image itself.
func frontView(lib *library, cache string, m Media) (string, error) {
	path := lib.path(m)
	out := filepath.Join(cache, "front-"+m.ID+".png")
	if _, err := os.Stat(out); err == nil {
		return out, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	img, _, err := image.Decode(f)
	if cerr := f.Close(); cerr != nil && err == nil {
		err = cerr
	}
	if err != nil {
		return "", err
	}
	rect, ok := frontFigure(img)
	if !ok {
		return path, nil
	}
	sub, ok := img.(interface {
		SubImage(image.Rectangle) image.Image
	})
	if !ok {
		return path, nil
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, sub.SubImage(rect)); err != nil {
		return "", err
	}
	if err := os.WriteFile(out, buf.Bytes(), 0o640); err != nil {
		return "", err
	}
	return out, nil
}
