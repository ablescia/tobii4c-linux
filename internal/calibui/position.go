package calibui

import (
	"fmt"
	"image"
	"math"
	"time"

	"github.com/ablescia/tobii4c-linux/internal/x11ui"
)

// Distanze dal tracker (mm). Il 4C traccia tra ~45 e ~95 cm; la zona comoda
// e più precisa è 50–80 cm.
const (
	distTooClose = 450.0
	distGoodMin  = 500.0
	distGoodMax  = 800.0
	distTooFar   = 900.0
	scaleMin     = 400.0
	scaleMax     = 1000.0
)

type position struct {
	eyes liveEyes
	bar  image.Rectangle
}

func newPosition() *position { return &position{} }

func (p *position) Static(a *App, img *image.RGBA) {
	a.ShowCursor(false)
	cx := a.W / 2
	x11ui.Text(img, a.F.Title, cx, a.Px(150), "Mettiti comodo davanti allo schermo", colText, x11ui.Center)
	x11ui.Text(img, a.F.Body, cx, a.Px(198), "Siediti a 50–80 cm, con gli occhi al centro del riquadro.", colDim, x11ui.Center)
	fw, fh := a.Px(720), a.Px(470)
	p.eyes.frame = image.Rect(cx-fw/2, a.Px(250), cx+fw/2, a.Px(250)+fh)
	p.eyes.static(a, img, true)

	// Scala della distanza, con la zona buona evidenziata.
	bw := a.Px(720)
	by := p.eyes.frame.Max.Y + a.Px(70)
	p.bar = image.Rect(cx-bw/2, by, cx+bw/2, by+a.Px(6))
	x11ui.RoundRect(img, p.bar, a.Pf(3), colPanel)
	g0, g1 := p.xAt(distGoodMin), p.xAt(distGoodMax)
	x11ui.RoundRect(img, image.Rect(int(g0), p.bar.Min.Y, int(g1), p.bar.Max.Y), a.Pf(3), x11ui.Alpha(colOK, 0.55))
	for _, cm := range []int{40, 50, 60, 70, 80, 90, 100} {
		x := p.xAt(float64(cm) * 10)
		label := fmt.Sprintf("%d", cm)
		if cm == 100 {
			label += " cm"
		}
		x11ui.Text(img, a.F.Small, int(x), p.bar.Max.Y+a.Px(30), label, colFaint, x11ui.Center)
	}
	a.drawKeys(img, cx, a.H-a.Px(90), []keyHint{{"Invio", "continua"}, {"Esc", "indietro"}}, x11ui.Center)
}

func (p *position) xAt(mm float64) float64 {
	t := (mm - scaleMin) / (scaleMax - scaleMin)
	return float64(p.bar.Min.X) + math.Max(0, math.Min(1, t))*float64(p.bar.Dx())
}

func (p *position) Frame(a *App, img *image.RGBA, now time.Time) {
	s, seen := p.eyes.frameEyes(a, img, now)
	msg, c := "Non ti vedo: guarda lo schermo", colWarn
	// Distanza e scostamento laterale si misurano dal tracker, non dallo schermo.
	if mid, ok := s.EyesTrackerMM(); seen && ok {
		d := norm(mid)
		x := float64(mid[0])
		switch {
		case d < distTooClose:
			msg, c = fmt.Sprintf("Troppo vicino: allontanati di circa %.0f cm", (distGoodMin+50-d)/10), colWarn
		case d < distGoodMin:
			msg, c = "Allontanati un po'", colWarn
		case d > distTooFar:
			msg, c = fmt.Sprintf("Troppo lontano: avvicinati di circa %.0f cm", (d-distGoodMax+50)/10), colWarn
		case d > distGoodMax:
			msg, c = "Avvicinati un po'", colWarn
		case x > 110:
			msg, c = "Spostati un po' verso sinistra", colWarn
		case x < -110:
			msg, c = "Spostati un po' verso destra", colWarn
		default:
			msg, c = "Perfetto, resta così", colOK
		}
		mx := p.xAt(d)
		my := float64(p.bar.Min.Y) + float64(p.bar.Dy())/2
		a.Dyn(x11ui.Circle(img, mx, my, a.Pf(11), colText))
		a.Dyn(x11ui.Text(img, a.F.Body, int(mx), p.bar.Min.Y-a.Px(22), it(d/10, 0)+" cm", colText, x11ui.Center))
	} else if seen {
		msg = "Vedo un solo occhio: guarda lo schermo"
	}
	a.Dyn(x11ui.Text(img, a.F.Heading, a.W/2, p.bar.Max.Y+a.Px(110), msg, c, x11ui.Center))
}

func (p *position) Event(a *App, ev x11ui.Event) {
	if ev.Kind != x11ui.KeyDown {
		return
	}
	switch ev.Sym {
	case x11ui.KeyEscape:
		a.Home()
	case x11ui.KeyReturn, x11ui.KeyKPEnter, x11ui.KeySpace:
		a.Next()
	}
}
