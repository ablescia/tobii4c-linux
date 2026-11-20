package x11ui

import (
	"image"
	"image/color"
	"image/draw"
	"math"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gomedium"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
	"golang.org/x/image/vector"
)

// Hex converte 0xRRGGBB in un colore opaco.
func Hex(v uint32) color.RGBA {
	return color.RGBA{uint8(v >> 16), uint8(v >> 8), uint8(v), 0xff}
}

// Alpha restituisce c con opacità a ∈ [0,1] (premoltiplicato).
func Alpha(c color.RGBA, a float64) color.RGBA {
	a = math.Max(0, math.Min(1, a))
	return color.RGBA{uint8(float64(c.R) * a), uint8(float64(c.G) * a), uint8(float64(c.B) * a), uint8(255 * a)}
}

// Fill riempie un rettangolo (opaco: copia diretta).
func Fill(dst *image.RGBA, r image.Rectangle, c color.RGBA) {
	draw.Draw(dst, r, image.NewUniform(c), image.Point{}, draw.Src)
}

const kappa = 0.5522847498 // approssimazione del cerchio con 4 cubiche

func circlePath(z *vector.Rasterizer, cx, cy, r float32, ccw bool) {
	k := r * kappa
	if !ccw {
		z.MoveTo(cx+r, cy)
		z.CubeTo(cx+r, cy+k, cx+k, cy+r, cx, cy+r)
		z.CubeTo(cx-k, cy+r, cx-r, cy+k, cx-r, cy)
		z.CubeTo(cx-r, cy-k, cx-k, cy-r, cx, cy-r)
		z.CubeTo(cx+k, cy-r, cx+r, cy-k, cx+r, cy)
	} else {
		z.MoveTo(cx+r, cy)
		z.CubeTo(cx+r, cy-k, cx+k, cy-r, cx, cy-r)
		z.CubeTo(cx-k, cy-r, cx-r, cy-k, cx-r, cy)
		z.CubeTo(cx-r, cy+k, cx-k, cy+r, cx, cy+r)
		z.CubeTo(cx+k, cy+r, cx+r, cy+k, cx+r, cy)
	}
	z.ClosePath()
}

// fillPath rasterizza un percorso nel riquadro bbox con composizione Over.
func fillPath(dst *image.RGBA, bbox image.Rectangle, c color.RGBA, build func(z *vector.Rasterizer, ox, oy float32)) image.Rectangle {
	bbox = bbox.Intersect(dst.Bounds())
	if bbox.Empty() || c.A == 0 {
		return image.Rectangle{}
	}
	z := vector.NewRasterizer(bbox.Dx(), bbox.Dy())
	z.DrawOp = draw.Over
	build(z, float32(bbox.Min.X), float32(bbox.Min.Y))
	z.Draw(dst, bbox, image.NewUniform(c), image.Point{})
	return bbox
}

func circleBox(cx, cy, r float64) image.Rectangle {
	return image.Rect(int(math.Floor(cx-r))-1, int(math.Floor(cy-r))-1, int(math.Ceil(cx+r))+1, int(math.Ceil(cy+r))+1)
}

// Circle disegna un disco pieno antialiasing; restituisce il riquadro toccato.
func Circle(dst *image.RGBA, cx, cy, r float64, c color.RGBA) image.Rectangle {
	if r <= 0 {
		return image.Rectangle{}
	}
	return fillPath(dst, circleBox(cx, cy, r), c, func(z *vector.Rasterizer, ox, oy float32) {
		circlePath(z, float32(cx)-ox, float32(cy)-oy, float32(r), false)
	})
}

// Ring disegna una corona circolare di spessore width centrata sul raggio r.
func Ring(dst *image.RGBA, cx, cy, r, width float64, c color.RGBA) image.Rectangle {
	ro, ri := r+width/2, math.Max(0, r-width/2)
	return fillPath(dst, circleBox(cx, cy, ro), c, func(z *vector.Rasterizer, ox, oy float32) {
		circlePath(z, float32(cx)-ox, float32(cy)-oy, float32(ro), false)
		if ri > 0 {
			circlePath(z, float32(cx)-ox, float32(cy)-oy, float32(ri), true)
		}
	})
}

// Arc disegna un arco di spessore width da a0 ad a1 (radianti, 0 = destra,
// senso orario sullo schermo).
func Arc(dst *image.RGBA, cx, cy, r, width, a0, a1 float64, c color.RGBA) image.Rectangle {
	ro, ri := r+width/2, math.Max(0, r-width/2)
	steps := max(8, int(math.Abs(a1-a0)*r/3))
	return fillPath(dst, circleBox(cx, cy, ro), c, func(z *vector.Rasterizer, ox, oy float32) {
		px := func(rad, a float64) (float32, float32) {
			return float32(cx+rad*math.Cos(a)) - ox, float32(cy+rad*math.Sin(a)) - oy
		}
		x, y := px(ro, a0)
		z.MoveTo(x, y)
		for i := 1; i <= steps; i++ {
			z.LineTo(px(ro, a0+(a1-a0)*float64(i)/float64(steps)))
		}
		for i := steps; i >= 0; i-- {
			z.LineTo(px(ri, a0+(a1-a0)*float64(i)/float64(steps)))
		}
		z.ClosePath()
	})
}

// Line disegna un segmento spesso.
func Line(dst *image.RGBA, x0, y0, x1, y1, width float64, c color.RGBA) image.Rectangle {
	dx, dy := x1-x0, y1-y0
	l := math.Hypot(dx, dy)
	if l == 0 {
		return Circle(dst, x0, y0, width/2, c)
	}
	nx, ny := -dy/l*width/2, dx/l*width/2
	box := image.Rect(int(math.Min(x0, x1)-width)-1, int(math.Min(y0, y1)-width)-1,
		int(math.Max(x0, x1)+width)+1, int(math.Max(y0, y1)+width)+1)
	return fillPath(dst, box, c, func(z *vector.Rasterizer, ox, oy float32) {
		p := func(x, y float64) (float32, float32) { return float32(x) - ox, float32(y) - oy }
		z.MoveTo(p(x0+nx, y0+ny))
		z.LineTo(p(x1+nx, y1+ny))
		z.LineTo(p(x1-nx, y1-ny))
		z.LineTo(p(x0-nx, y0-ny))
		z.ClosePath()
	})
}

// Triangle riempie un triangolo.
func Triangle(dst *image.RGBA, p [3][2]float64, c color.RGBA) image.Rectangle {
	minX, minY, maxX, maxY := p[0][0], p[0][1], p[0][0], p[0][1]
	for _, q := range p {
		minX, maxX = math.Min(minX, q[0]), math.Max(maxX, q[0])
		minY, maxY = math.Min(minY, q[1]), math.Max(maxY, q[1])
	}
	box := image.Rect(int(minX)-1, int(minY)-1, int(maxX)+2, int(maxY)+2)
	return fillPath(dst, box, c, func(z *vector.Rasterizer, ox, oy float32) {
		z.MoveTo(float32(p[0][0])-ox, float32(p[0][1])-oy)
		z.LineTo(float32(p[1][0])-ox, float32(p[1][1])-oy)
		z.LineTo(float32(p[2][0])-ox, float32(p[2][1])-oy)
		z.ClosePath()
	})
}

// RoundRect riempie un rettangolo con angoli arrotondati.
func RoundRect(dst *image.RGBA, r image.Rectangle, rad float64, c color.RGBA) image.Rectangle {
	x0, y0, x1, y1 := float64(r.Min.X), float64(r.Min.Y), float64(r.Max.X), float64(r.Max.Y)
	rad = math.Min(rad, math.Min(x1-x0, y1-y0)/2)
	k := rad * (1 - kappa)
	return fillPath(dst, r, c, func(z *vector.Rasterizer, ox, oy float32) {
		p := func(x, y float64) (float32, float32) { return float32(x) - ox, float32(y) - oy }
		z.MoveTo(p(x0+rad, y0))
		z.LineTo(p(x1-rad, y0))
		cx1, cy1 := p(x1-k, y0)
		cx2, cy2 := p(x1, y0+k)
		ex, ey := p(x1, y0+rad)
		z.CubeTo(cx1, cy1, cx2, cy2, ex, ey)
		z.LineTo(p(x1, y1-rad))
		cx1, cy1 = p(x1, y1-k)
		cx2, cy2 = p(x1-k, y1)
		ex, ey = p(x1-rad, y1)
		z.CubeTo(cx1, cy1, cx2, cy2, ex, ey)
		z.LineTo(p(x0+rad, y1))
		cx1, cy1 = p(x0+k, y1)
		cx2, cy2 = p(x0, y1-k)
		ex, ey = p(x0, y1-rad)
		z.CubeTo(cx1, cy1, cx2, cy2, ex, ey)
		z.LineTo(p(x0, y0+rad))
		cx1, cy1 = p(x0, y0+k)
		cx2, cy2 = p(x0+k, y0)
		ex, ey = p(x0+rad, y0)
		z.CubeTo(cx1, cy1, cx2, cy2, ex, ey)
		z.ClosePath()
	})
}

// ---------------------------------------------------------------------------
// Testo

// Font è un carattere a una dimensione.
type Font struct {
	face         font.Face
	Ascent, Desc int
	Height       int
}

// Fonts è la famiglia usata dall'interfaccia: Go Regular e Go Medium
// (Bigelow & Holmes), incorporati nel binario.
type Fonts struct {
	Title, Heading, Body, Small, Key, Big *Font
}

// newFont prepara il carattere ttf alla dimensione size (pixel). Le maschere
// dei glifi restano in una cache: un Font va usato da una sola goroutine.
func newFont(ttf []byte, size float64) (*Font, error) {
	f, err := opentype.Parse(ttf)
	if err != nil {
		return nil, err
	}
	face, err := opentype.NewFace(f, &opentype.FaceOptions{Size: size, DPI: 72, Hinting: font.HintingFull})
	if err != nil {
		return nil, err
	}
	m := face.Metrics()
	return &Font{face: newCachedFace(face), Ascent: m.Ascent.Ceil(), Desc: m.Descent.Ceil(), Height: m.Height.Ceil()}, nil
}

// LoadFonts prepara la scala tipografica per uno schermo alto h pixel.
// Scala modulare 1,25 a partire dal corpo del testo (22 px a 1080p).
func LoadFonts(h int) (*Fonts, error) {
	s := float64(h) / 1080
	var fs Fonts
	var err error
	specs := []struct {
		dst  **Font
		ttf  []byte
		size float64
	}{
		{&fs.Big, gomedium.TTF, 68},
		{&fs.Title, gomedium.TTF, 43},
		{&fs.Heading, gomedium.TTF, 27.5},
		{&fs.Body, goregular.TTF, 22},
		{&fs.Small, goregular.TTF, 17.6},
		{&fs.Key, gomedium.TTF, 16},
	}
	for _, sp := range specs {
		if *sp.dst, err = newFont(sp.ttf, sp.size*s); err != nil {
			return nil, err
		}
	}
	return &fs, nil
}

// Measure restituisce la larghezza in pixel di s.
func (f *Font) Measure(s string) int { return font.MeasureString(f.face, s).Ceil() }

// Align del testo rispetto a x.
type Align int

const (
	Left Align = iota
	Center
	Right
)

// Text disegna s con la linea di base a y; restituisce il riquadro occupato.
func Text(dst *image.RGBA, f *Font, x, y int, s string, c color.RGBA, a Align) image.Rectangle {
	w := f.Measure(s)
	switch a {
	case Center:
		x -= w / 2
	case Right:
		x -= w
	}
	d := font.Drawer{Dst: dst, Src: image.NewUniform(c), Face: f.face, Dot: fixed.P(x, y)}
	d.DrawString(s)
	return image.Rect(x-1, y-f.Ascent-1, x+w+2, y+f.Desc+2)
}

// Wrap spezza s in righe larghe al più width pixel.
func Wrap(f *Font, s string, width int) []string {
	var lines []string
	var cur string
	for _, word := range splitWords(s) {
		try := word
		if cur != "" {
			try = cur + " " + word
		}
		if f.Measure(try) > width && cur != "" {
			lines = append(lines, cur)
			cur = word
		} else {
			cur = try
		}
	}
	if cur != "" {
		lines = append(lines, cur)
	}
	return lines
}

func splitWords(s string) []string {
	var out []string
	start := -1
	for i, r := range s {
		if r == ' ' {
			if start >= 0 {
				out = append(out, s[start:i])
				start = -1
			}
		} else if start < 0 {
			start = i
		}
	}
	if start >= 0 {
		out = append(out, s[start:])
	}
	return out
}
