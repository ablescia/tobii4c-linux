package x11ui

import (
	"image"
	"image/color"
	"math"
)

// Primitive per disegni fitti (mesh del viso, grafici): scrivono i pixel
// direttamente, senza rasterizzatore e senza allocazioni. c è un colore
// opaco; a è l'opacità.

// Blend compone c sul pixel (x, y) con opacità a ∈ [0,1], se (x, y) sta in clip.
func Blend(dst *image.RGBA, clip image.Rectangle, x, y int, c color.RGBA, a float64) {
	if a <= 0 || !(image.Point{x, y}).In(clip) || !(image.Point{x, y}).In(dst.Rect) {
		return
	}
	if a > 1 {
		a = 1
	}
	i := dst.PixOffset(x, y)
	p := dst.Pix[i : i+4 : i+4]
	ia := 1 - a
	p[0] = uint8(float64(c.R)*a + float64(p[0])*ia)
	p[1] = uint8(float64(c.G)*a + float64(p[1])*ia)
	p[2] = uint8(float64(c.B)*a + float64(p[2])*ia)
	p[3] = 0xff
}

// ThinLine disegna una linea antialias larga un pixel (algoritmo di Wu),
// limitata a clip.
func ThinLine(dst *image.RGBA, clip image.Rectangle, x0, y0, x1, y1 float64, c color.RGBA, a float64) {
	steep := math.Abs(y1-y0) > math.Abs(x1-x0)
	if steep {
		x0, y0, x1, y1 = y0, x0, y1, x1
	}
	if x0 > x1 {
		x0, x1, y0, y1 = x1, x0, y1, y0
	}
	dx, dy := x1-x0, y1-y0
	grad := 1.0
	if dx > 1e-9 {
		grad = dy / dx
	}
	plot := func(x, y int, cov float64) {
		if steep {
			Blend(dst, clip, y, x, c, a*cov)
		} else {
			Blend(dst, clip, x, y, c, a*cov)
		}
	}
	xs, xe := int(math.Round(x0)), int(math.Round(x1))
	// limita il ciclo alla zona visibile
	lo, hi := clip.Min.X, clip.Max.X
	if steep {
		lo, hi = clip.Min.Y, clip.Max.Y
	}
	if xs < lo-1 {
		xs = lo - 1
	}
	if xe > hi {
		xe = hi
	}
	for x := xs; x <= xe; x++ {
		y := y0 + grad*(float64(x)-x0)
		fy := math.Floor(y)
		f := y - fy
		plot(x, int(fy), 1-f)
		plot(x, int(fy)+1, f)
	}
}

// Dot disegna un disco antialias di raggio r (pensato per raggi di pochi
// pixel), limitato a clip.
func Dot(dst *image.RGBA, clip image.Rectangle, cx, cy, r float64, c color.RGBA, a float64) {
	x0, x1 := int(math.Floor(cx-r-1)), int(math.Ceil(cx+r+1))
	y0, y1 := int(math.Floor(cy-r-1)), int(math.Ceil(cy+r+1))
	for y := y0; y <= y1; y++ {
		for x := x0; x <= x1; x++ {
			d := math.Hypot(float64(x)+0.5-cx, float64(y)+0.5-cy)
			if cov := r + 0.5 - d; cov > 0 {
				Blend(dst, clip, x, y, c, a*math.Min(1, cov))
			}
		}
	}
}

// Frame disegna il bordo di un rettangolo largo w pixel (opaco, senza
// antialias).
func Frame(dst *image.RGBA, r image.Rectangle, w int, c color.RGBA) {
	Fill(dst, image.Rect(r.Min.X, r.Min.Y, r.Max.X, r.Min.Y+w), c)
	Fill(dst, image.Rect(r.Min.X, r.Max.Y-w, r.Max.X, r.Max.Y), c)
	Fill(dst, image.Rect(r.Min.X, r.Min.Y, r.Min.X+w, r.Max.Y), c)
	Fill(dst, image.Rect(r.Max.X-w, r.Min.Y, r.Max.X, r.Max.Y), c)
}
