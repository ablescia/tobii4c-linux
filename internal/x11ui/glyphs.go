package x11ui

import (
	"image"
	"image/draw"

	"golang.org/x/image/font"
	"golang.org/x/image/math/fixed"
)

// cachedFace tiene in memoria le maschere dei glifi già rasterizzati: il
// rasterizzatore di opentype ridisegna ogni glifo a ogni chiamata, e
// un'interfaccia che riscrive centinaia di valori più volte al secondo
// passerebbe lì gran parte del tempo. La chiave comprende la parte
// frazionaria della posizione, quindi il risultato è identico a quello senza
// cache. Non è sicura per l'uso concorrente: come il resto del disegno,
// appartiene alla goroutine dell'interfaccia.
type cachedFace struct {
	font.Face
	glyphs map[glyphKey]glyphVal
	adv    map[rune]advVal
	kern   map[[2]rune]fixed.Int26_6
}

type glyphKey struct {
	r      rune
	fx, fy fixed.Int26_6 // parte frazionaria del punto di partenza
}

type glyphVal struct {
	dr   image.Rectangle // relativo alla parte intera del punto di partenza
	mask *image.Alpha
	adv  fixed.Int26_6
	ok   bool
}

type advVal struct {
	adv fixed.Int26_6
	ok  bool
}

// maxCachedGlyphs limita la memoria: oltre, la cache riparte da vuota.
const maxCachedGlyphs = 8192

func newCachedFace(f font.Face) *cachedFace {
	return &cachedFace{Face: f, glyphs: map[glyphKey]glyphVal{}, adv: map[rune]advVal{}, kern: map[[2]rune]fixed.Int26_6{}}
}

func (f *cachedFace) Glyph(dot fixed.Point26_6, r rune) (image.Rectangle, image.Image, image.Point, fixed.Int26_6, bool) {
	k := glyphKey{r: r, fx: dot.X & 63, fy: dot.Y & 63}
	g, hit := f.glyphs[k]
	if !hit {
		dr, mask, mp, adv, ok := f.Face.Glyph(fixed.Point26_6{X: k.fx, Y: k.fy}, r)
		g = glyphVal{adv: adv, ok: ok}
		if ok && !dr.Empty() && mask != nil {
			a := image.NewAlpha(image.Rect(0, 0, dr.Dx(), dr.Dy()))
			draw.Draw(a, a.Bounds(), mask, mp, draw.Src)
			g.dr, g.mask = dr, a
		}
		if len(f.glyphs) >= maxCachedGlyphs {
			clear(f.glyphs)
		}
		f.glyphs[k] = g
	}
	if g.mask == nil {
		return image.Rectangle{}, nil, image.Point{}, g.adv, g.ok
	}
	return g.dr.Add(image.Pt(dot.X.Floor(), dot.Y.Floor())), g.mask, image.Point{}, g.adv, g.ok
}

func (f *cachedFace) GlyphAdvance(r rune) (fixed.Int26_6, bool) {
	if a, ok := f.adv[r]; ok {
		return a.adv, a.ok
	}
	adv, ok := f.Face.GlyphAdvance(r)
	if len(f.adv) >= maxCachedGlyphs {
		clear(f.adv)
	}
	f.adv[r] = advVal{adv, ok}
	return adv, ok
}

func (f *cachedFace) Kern(r0, r1 rune) fixed.Int26_6 {
	k := [2]rune{r0, r1}
	if v, ok := f.kern[k]; ok {
		return v
	}
	v := f.Face.Kern(r0, r1)
	if len(f.kern) >= maxCachedGlyphs {
		clear(f.kern)
	}
	f.kern[k] = v
	return v
}
