package main

import (
	"image"
	"image/color"
	"testing"
)

// canvas is a w by h image of one color.
func canvas(w, h int, c color.Color) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, c)
		}
	}
	return img
}

func fill(img *image.RGBA, r image.Rectangle, c color.Color) {
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			img.Set(x, y, c)
		}
	}
}

func TestFrontFigureCutsASheetsFirstFigureAndLeavesOtherImages(t *testing.T) {
	gray, dark := color.RGBA{230, 230, 230, 255}, color.RGBA{40, 50, 60, 255}
	sheet := canvas(1248, 832, gray)
	for i := range 4 {
		x := 80 + i*300
		fill(sheet, image.Rect(x, 100, x+140, 780), dark)
	}
	r, ok := frontFigure(sheet)
	if !ok {
		t.Fatal("a four-figure sheet was not recognized")
	}
	if r.Min.X > 80 || r.Max.X < 220 || r.Max.X > 380 || r.Dy() != 832 {
		t.Errorf("cut %v, want the first figure (x 80 to 220) with a margin, full height", r)
	}

	portrait := canvas(832, 1248, gray)
	fill(portrait, image.Rect(300, 200, 540, 1200), dark)
	if _, ok := frontFigure(portrait); ok {
		t.Error("one figure is not a sheet")
	}

	photo := image.NewRGBA(image.Rect(0, 0, 1280, 720))
	for y := range 720 {
		for x := range 1280 {
			photo.Set(x, y, color.RGBA{uint8(x / 6), uint8(y / 3), uint8((x + y) / 8), 255})
		}
	}
	if _, ok := frontFigure(photo); ok {
		t.Error("a photo whose corners differ is not a sheet")
	}
}
