package calibui

import (
	"context"
	"fmt"
	"image"
	"math"
	"time"

	"github.com/ablescia/tobii4c-linux/internal/ttp"
	"github.com/ablescia/tobii4c-linux/internal/x11ui"
)

// displaySetup: come nell'app ufficiale, due frecce sul bordo inferiore
// dello schermo vanno allineate ai due segni guida sul bordo superiore del
// tracker (a Mounting.WidthMM l'uno dall'altro, 184 mm sul 4C). Il loro punto
// medio è il centro del tracker, da cui l'offset orizzontale.
//
// Se l'EDID del monitor è credibile (proporzioni uguali a quelle della
// finestra), la dimensione viene da lì: le frecce restano alla distanza dei
// segni e si spostano insieme. Allinearle "a occhio" sarebbe peggio, perché i
// segni stanno davanti allo schermo e sembrano più distanti di quanto sono.
// Altrimenti (o premendo M) si misura a mano: la distanza tra le frecce in
// pixel dà il passo del pixel, e quindi la dimensione dello schermo.
type displaySetup struct {
	guideMM  float64
	guides   int
	edidMM   [2]float64 // 0 = non utilizzabile
	manual   bool
	mid      float64 // centro delle frecce, px
	spacing  float64 // distanza tra le frecce, px
	current  *ttp.DisplayArea
	drag     int // 0 nessuno, 1 coppia, 2 freccia sinistra, 3 destra
	dragOff  float64
	applying bool
	message  string
	msgCol   colorName
	err      string
}

func newDisplaySetup() *displaySetup { return &displaySetup{} }

const (
	minMonitorOverGuide = 1.5
	maxMonitorMM        = 3000
	aspectTolerance     = 0.02
)

func (d *displaySetup) init(a *App) {
	dev, err := a.Ctrl.Device()
	if err != nil {
		d.err = "Tracker non connesso."
		return
	}
	area, m := dev.DisplayInfo()
	if m == nil || m.Guides < 1 {
		d.err = "Il tracker non ha fornito la geometria dei segni guida."
		return
	}
	d.guideMM, d.guides, d.current = m.WidthMM, m.Guides, area
	mon := a.Monitor
	wide := math.Max(mon.WidthMM, mon.HeightMM)
	aspectPx := float64(a.W) / float64(a.H)
	if mon.WidthMM > 0 && mon.HeightMM > 0 && wide >= d.guideMM*minMonitorOverGuide && wide <= maxMonitorMM &&
		math.Abs(mon.WidthMM/mon.HeightMM-aspectPx) <= aspectPx*aspectTolerance {
		d.edidMM = [2]float64{mon.WidthMM, mon.HeightMM}
	}
	d.manual = d.edidMM[0] == 0
	widthMM := d.edidMM[0]
	if widthMM == 0 && area != nil && area.Width() > 0 {
		widthMM = area.Width()
	}
	if widthMM == 0 {
		widthMM = 531 // 24" 16:9
	}
	pitch := widthMM / float64(a.W)
	offset := 0.0
	if area != nil {
		offset = area.OffsetX()
	}
	d.spacing = d.guideMM / pitch
	d.mid = float64(a.W)/2 - offset/pitch
	d.clamp(a)
}

func (d *displaySetup) clamp(a *App) {
	W := float64(a.W)
	d.spacing = math.Max(W*0.05, math.Min(W, d.spacing))
	if !d.manual {
		d.spacing = d.guideMM / (d.edidMM[0] / W)
	}
	half := d.spacing / 2
	if d.guides < 2 {
		half = 0
	}
	d.mid = math.Max(half, math.Min(W-half, d.mid))
}

// choice: larghezza, altezza, offset (mm).
func (d *displaySetup) choice(a *App) (w, h, off float64) {
	W, H := float64(a.W), float64(a.H)
	var pitch float64
	if !d.manual {
		w, h = d.edidMM[0], d.edidMM[1]
		pitch = w / W
	} else {
		pitch = d.guideMM / d.spacing
		w, h = W*pitch, H*pitch
	}
	return w, h, (W/2 - d.mid) * pitch
}

func (d *displaySetup) Static(a *App, img *image.RGBA) {
	a.ShowCursor(true)
	if d.guideMM == 0 && d.err == "" {
		d.init(a)
	}
	cx := a.W / 2
	y := a.Px(250)
	x11ui.Text(img, a.F.Title, cx, y, "Allinea le frecce ai segni del tracker", colText, x11ui.Center)
	y += a.Px(56)
	if d.err != "" {
		x11ui.Text(img, a.F.Body, cx, y, d.err, colWarn, x11ui.Center)
		a.drawKeys(img, cx, a.H-a.Px(150), []keyHint{{"Esc", "indietro"}}, x11ui.Center)
		return
	}
	body := fmt.Sprintf("Sul bordo superiore del tracker ci sono due segni a %.0f mm l'uno dall'altro. "+
		"Sposta le frecce in basso finché ognuna punta al suo segno.", d.guideMM)
	if d.guides < 2 {
		body = "Sposta la freccia in basso finché punta al centro del tracker."
	}
	a.paragraph(img, a.F.Body, cx, y, a.Px(820), body, colDim, x11ui.Center)

	mode := "Misure dall'EDID: le frecce si spostano insieme."
	keys := []keyHint{{"← →", "sposta"}, {"Maiusc", "più veloce"}}
	if d.manual {
		mode = "Misura a mano: la distanza tra le frecce dà la dimensione dello schermo."
		keys = append(keys, keyHint{"↑ ↓", "distanza tra le frecce"})
	}
	if d.edidMM[0] > 0 {
		keys = append(keys, keyHint{"M", map[bool]string{true: "usa l'EDID", false: "misura a mano"}[d.manual]})
	}
	keys = append(keys, keyHint{"Invio", "applica"}, keyHint{"Esc", "annulla"})
	x11ui.Text(img, a.F.Small, cx, a.Px(560), mode, colFaint, x11ui.Center)
	a.drawKeys(img, cx, a.H-a.Px(150), keys, x11ui.Center)
	// Centro geometrico dello schermo, riferimento discreto.
	x11ui.Line(img, float64(cx), float64(a.H)-a.Pf(46), float64(cx), float64(a.H)-a.Pf(30), a.Pf(1.5), colFaint)
}

func (d *displaySetup) Frame(a *App, img *image.RGBA, now time.Time) {
	if d.err != "" {
		return
	}
	w, h, off := d.choice(a)
	cx := a.W / 2
	labelX, valX := cx-a.Px(250), cx-a.Px(40)
	y := a.Px(430)
	mon := a.Monitor.Model
	if mon == "" {
		mon = a.Monitor.Name
	}
	src := "misurato con le frecce"
	if !d.manual {
		src = "dall'EDID di " + mon
	}
	side := "a destra"
	if off > 0 {
		side = "a sinistra"
	}
	rows := [][2]string{
		{"Schermo", fmt.Sprintf("%.0f × %.0f mm, %s", w, h, src)},
		{"Tracker", fmt.Sprintf("%s mm %s del centro", it(math.Abs(off), 0), side)},
	}
	if math.Abs(off) < 0.5 {
		rows[1][1] = "centrato"
	}
	if d.current != nil {
		rows = append(rows, [2]string{"Attuale", fmt.Sprintf("%.0f × %.0f mm, centro a %s mm", d.current.Width(), d.current.Height(), it(d.current.OffsetX(), 0))})
	}
	for _, r := range rows {
		a.Dyn(x11ui.Text(img, a.F.Body, labelX, y, r[0], colFaint, x11ui.Left))
		a.Dyn(x11ui.Text(img, a.F.Body, valX, y, r[1], colText, x11ui.Left))
		y += a.Px(40)
	}
	if d.message != "" {
		c := colDim
		if d.msgCol == cOK {
			c = colOK
		} else if d.msgCol == cWarn {
			c = colWarn
		}
		a.Dyn(x11ui.Text(img, a.F.Body, cx, a.Px(650), d.message, c, x11ui.Center))
	}
	// Frecce sul bordo inferiore, puntate verso il tracker.
	H := float64(a.H)
	tw, th := a.Pf(16), a.Pf(26)
	xs := []float64{d.mid}
	if d.guides >= 2 {
		xs = []float64{d.mid - d.spacing/2, d.mid + d.spacing/2}
		a.Dyn(x11ui.Line(img, xs[0], H-th-a.Pf(14), xs[1], H-th-a.Pf(14), a.Pf(2), x11ui.Alpha(colText, 0.45)))
	}
	for _, x := range xs {
		a.Dyn(x11ui.Triangle(img, [3][2]float64{{x - tw, H - th - a.Pf(2)}, {x + tw, H - th - a.Pf(2)}, {x, H - 1}}, colText))
		a.Dyn(x11ui.Line(img, x, H-th-a.Pf(40), x, H-th-a.Pf(2), a.Pf(2), colText))
	}
}

func (d *displaySetup) Event(a *App, ev x11ui.Event) {
	if d.err != "" {
		if ev.Kind == x11ui.KeyDown && ev.Sym == x11ui.KeyEscape {
			a.Home()
		}
		return
	}
	if d.applying {
		return
	}
	step := 1.0
	if ev.Shift {
		step = 10
	}
	switch ev.Kind {
	case x11ui.KeyDown:
		switch ev.Sym {
		case x11ui.KeyEscape:
			a.Home()
		case x11ui.KeyLeft:
			d.mid -= step
		case x11ui.KeyRight:
			d.mid += step
		case x11ui.KeyUp:
			if d.manual {
				d.spacing += step
			}
		case x11ui.KeyDown_:
			if d.manual {
				d.spacing -= step
			}
		case 'm':
			if d.edidMM[0] > 0 {
				d.manual = !d.manual
				a.RefreshStatic()
			}
		case x11ui.KeyReturn, x11ui.KeyKPEnter:
			d.apply(a)
		}
		d.clamp(a)
	case x11ui.MouseDown:
		if ev.Button != 1 {
			return
		}
		x := float64(ev.X)
		l, r := d.mid-d.spacing/2, d.mid+d.spacing/2
		switch {
		case d.manual && d.guides >= 2 && math.Abs(x-l) < a.Pf(30):
			d.drag = 2
		case d.manual && d.guides >= 2 && math.Abs(x-r) < a.Pf(30):
			d.drag = 3
		case float64(ev.Y) > float64(a.H)*0.75:
			d.drag, d.dragOff = 1, d.mid-x
		}
	case x11ui.MouseMove:
		x := float64(ev.X)
		switch d.drag {
		case 1:
			d.mid = x + d.dragOff
		case 2, 3:
			l, r := d.mid-d.spacing/2, d.mid+d.spacing/2
			if d.drag == 2 {
				l = x
			} else {
				r = x
			}
			if r > l {
				d.mid, d.spacing = (l+r)/2, r-l
			}
		}
		d.clamp(a)
	case x11ui.MouseUp:
		d.drag = 0
	}
}

func (d *displaySetup) apply(a *App) {
	w, h, off := d.choice(a)
	d.applying, d.message, d.msgCol = true, "Applico la display area…", cDim
	mon := a.Monitor.Model
	if mon == "" {
		mon = a.Monitor.Name
	}
	Go(a, func(context.Context) error {
		_, err := a.Ctrl.ApplyDisplay(w, h, off, mon)
		return err
	}, func(err error) {
		if err != nil {
			d.applying, d.message, d.msgCol = false, "Display area non applicata: "+err.Error(), cWarn
			return
		}
		dev, _ := a.Ctrl.Device()
		if dev != nil {
			d.current, _ = dev.DisplayInfo()
		}
		d.message, d.msgCol = fmt.Sprintf("Display area applicata: %.0f × %.0f mm", w, h), cOK
		a.After(900*time.Millisecond, a.Next)
	})
}
