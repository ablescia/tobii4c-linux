package headpose

import (
	"math"

	"github.com/ablescia/tobii4c-linux/internal/irimg"
)

// Crop è un ritaglio quadrato e ruotato dell'immagine: centro e lato in pixel,
// Angle (rad) è l'angolo dell'asse x del ritaglio rispetto a quello
// dell'immagine (positivo in senso orario sullo schermo, perché y scende).
type Crop struct {
	CX, CY, Side, Angle float64
}

// ToImage mappa il punto (u, v) di un ritaglio di n×n pixel nell'immagine.
func (c Crop) ToImage(u, v float64, n int) (x, y float64) {
	s := c.Side / float64(n)
	du, dv := (u-float64(n)/2)*s, (v-float64(n)/2)*s
	sin, cos := math.Sincos(c.Angle)
	return c.CX + du*cos - dv*sin, c.CY + du*sin + dv*cos
}

// FromImage è l'inverso di ToImage.
func (c Crop) FromImage(x, y float64, n int) (u, v float64) {
	s := float64(n) / c.Side
	dx, dy := x-c.CX, y-c.CY
	sin, cos := math.Sincos(c.Angle)
	return (dx*cos+dy*sin)*s + float64(n)/2, (-dx*sin+dy*cos)*s + float64(n)/2
}

// sampler campiona un ritaglio in un tensore NHWC n×n×3 in [0,1], con
// interpolazione bilineare. Il contrasto viene normalizzato tra il 1° e il
// 99° percentile del ritaglio: le immagini IR del tracker sono scure e
// variano con l'esposizione automatica, mentre il modello è addestrato su
// foto a colori ben esposte.
type sampler struct {
	n    int
	gray []float32
}

func newSampler(n int) *sampler { return &sampler{n: n, gray: make([]float32, n*n)} }

func (s *sampler) sample(img *irimg.Frame, c Crop, dst []float32) {
	n := s.n
	var hist [256]int
	sc := c.Side / float64(n)
	sin, cos := math.Sincos(c.Angle)
	// passo nell'immagine per un pixel del ritaglio lungo u e lungo v
	ux, uy := cos*sc, sin*sc
	vx, vy := -sin*sc, cos*sc
	x0, y0 := c.ToImage(0.5, 0.5, n) // centro del primo pixel
	w, h := img.W, img.H
	for v := 0; v < n; v++ {
		x, y := x0+float64(v)*vx, y0+float64(v)*vy
		row := s.gray[v*n : (v+1)*n]
		for u := range row {
			// bilineare con bordo nero
			fx, fy := x-0.5, y-0.5
			ix, iy := int(math.Floor(fx)), int(math.Floor(fy))
			ax, ay := float32(fx-float64(ix)), float32(fy-float64(iy))
			var p00, p10, p01, p11 float32
			if ix >= 0 && iy >= 0 && ix+1 < w && iy+1 < h {
				o := iy*w + ix
				p00, p10 = float32(img.Pix[o]), float32(img.Pix[o+1])
				p01, p11 = float32(img.Pix[o+w]), float32(img.Pix[o+w+1])
			} else {
				p00, p10 = float32(img.At(ix, iy)), float32(img.At(ix+1, iy))
				p01, p11 = float32(img.At(ix, iy+1)), float32(img.At(ix+1, iy+1))
			}
			val := (p00*(1-ax)+p10*ax)*(1-ay) + (p01*(1-ax)+p11*ax)*ay
			row[u] = val
			hist[int(val)]++
			x += ux
			y += uy
		}
	}
	lo, hi := percentile(&hist, n*n, 0.01), percentile(&hist, n*n, 0.99)
	if hi-lo < 8 {
		hi = lo + 8
	}
	scale := 1 / float32(hi-lo)
	for i, g := range s.gray {
		t := (g - float32(lo)) * scale
		t = min(max(t, 0), 1)
		dst[3*i], dst[3*i+1], dst[3*i+2] = t, t, t
	}
}

func percentile(h *[256]int, total int, q float64) int {
	target := int(q * float64(total))
	acc := 0
	for i, c := range h {
		acc += c
		if acc > target {
			return i
		}
	}
	return 255
}
