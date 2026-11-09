//go:build linux

package main

import (
	"image"
	"math"

	"github.com/ablescia/tobii4c-linux/internal/bodypose"
	"github.com/ablescia/tobii4c-linux/internal/x11ui"
)

// radioGroup è un gruppo di radio button: una sola voce scelta, clic del
// mouse sulla voce (pallino o etichetta) per sceglierne un'altra.
type radioGroup struct {
	label    string
	items    []string
	selected func(a *App) int
	enabled  func(a *App, i int) bool
	pick     func(a *App, i int)

	rects []image.Rectangle // aree cliccabili, dall'ultimo disegno
	hot   int               // voce sotto il mouse (-1: nessuna)
}

func newRadios() []*radioGroup {
	gs := []*radioGroup{
		{label: "Tipologia", items: []string{"Corpo intero", "Mezzo busto"},
			selected: func(a *App) int { return int(a.snap.Want.Mode) },
			pick: func(a *App, i int) {
				c := a.Eng.Wanted()
				c.Mode = bodypose.Mode(i)
				a.request(c)
			}},
		{label: "Modello", items: bodypose.Variants,
			selected: func(a *App) int {
				for i, v := range bodypose.Variants {
					if v == a.snap.Want.Variant {
						return i
					}
				}
				return 1
			},
			enabled: func(a *App, i int) bool { return a.snap.Want.Mode == bodypose.FullBody },
			pick: func(a *App, i int) {
				c := a.Eng.Wanted()
				c.Variant = bodypose.Variants[i]
				a.request(c)
			}},
		{label: "Calcolo", items: []string{"GPU", "CPU"},
			selected: func(a *App) int {
				if a.snap.Want.GPU {
					return 0
				}
				return 1
			},
			enabled: func(a *App, i int) bool { return i == 1 || a.Eng.GPU != nil },
			pick: func(a *App, i int) {
				c := a.Eng.Wanted()
				c.GPU = i == 0
				a.request(c)
			}},
	}
	for _, g := range gs {
		g.hot = -1
	}
	return gs
}

func (g *radioGroup) isEnabled(a *App, i int) bool { return g.enabled == nil || g.enabled(a, i) }

// draw disegna il gruppo da x sulla linea di base y; restituisce la x
// dove finisce.
func (g *radioGroup) draw(a *App, img *image.RGBA, x, y int) int {
	t := x11ui.Text(img, a.f.small, x, y, g.label, colFaint, x11ui.Left)
	x = t.Max.X + a.px(12)
	sel := g.selected(a)
	g.rects = g.rects[:0]
	r := a.pf(7)
	for i, it := range g.items {
		on := g.isEnabled(a, i)
		cx, cy := float64(x)+r, float64(y)-a.pf(4.5)
		ring, text := colDim, colText
		if !on {
			ring, text = colKey, colFaint
		} else if i == g.hot {
			ring = colText
		}
		x11ui.Circle(img, cx, cy, r, ring)
		x11ui.Circle(img, cx, cy, r-math.Max(1.2, a.pf(1.6)), colPanel)
		if i == sel {
			dot := colAccent
			if !on {
				dot = colFaint
			}
			x11ui.Circle(img, cx, cy, r-a.pf(3.4), dot)
		}
		tr := x11ui.Text(img, a.f.label, int(cx+r)+a.px(6), y, it, text, x11ui.Left)
		g.rects = append(g.rects, image.Rect(x-a.px(3), int(cy-r)-a.px(4), tr.Max.X+a.px(4), int(cy+r)+a.px(4)))
		x = tr.Max.X + a.px(16)
	}
	return x
}

func (g *radioGroup) at(p image.Point) int {
	for i, r := range g.rects {
		if p.In(r) {
			return i
		}
	}
	return -1
}

// hover aggiorna la voce sotto il mouse; true se è cambiata.
func (g *radioGroup) hover(a *App, p image.Point) bool {
	i := g.at(p)
	if i >= 0 && !g.isEnabled(a, i) {
		i = -1
	}
	if i == g.hot {
		return false
	}
	g.hot = i
	return true
}

// click sceglie la voce sotto p; true se il clic era sul gruppo.
func (g *radioGroup) click(a *App, p image.Point) bool {
	i := g.at(p)
	if i < 0 || !g.isEnabled(a, i) {
		return false
	}
	if i != g.selected(a) {
		g.pick(a, i)
	}
	return true
}

// --- riquadro dei comandi -----------------------------------------------------

func (a *App) controlNote() string {
	s := &a.snap
	switch {
	case s.LoadErr != "":
		return "errore: " + s.LoadErr
	case s.Loading != "":
		return s.Loading
	case s.Want.key() != s.Cur.key():
		return "in attesa del cambio…"
	}
	return "clic sulle voci o tasti B, 1–3, G"
}

func (a *App) drawControls(img *image.RGBA, b image.Rectangle) {
	y := b.Min.Y + a.px(20)
	x := b.Min.X
	for k, g := range a.radios {
		if k > 0 {
			x11ui.Fill(img, image.Rect(x, b.Min.Y+a.px(4), x+1, b.Max.Y-a.px(2)), colLine)
			x += a.px(16)
		}
		x = g.draw(a, img, x, y)
	}
}
