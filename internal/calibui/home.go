package calibui

import (
	"fmt"
	"image"
	"math"
	"os"
	"time"

	"github.com/ablescia/tobii4c-linux/internal/gaze"
	"github.com/ablescia/tobii4c-linux/internal/store"
	"github.com/ablescia/tobii4c-linux/internal/x11ui"
)

// liveEyes è la vista dal vivo della camera: i due occhi come bagliori
// infrarossi, a specchio (il tuo occhio destro appare a destra).
type liveEyes struct {
	frame image.Rectangle
}

func (l *liveEyes) static(a *App, img *image.RGBA, accent bool) {
	x11ui.RoundRect(img, l.frame, a.Pf(18), colPanel)
	// Mirino: quattro angoli, il campo utile della camera.
	in := l.frame.Inset(a.Px(22))
	arm, w := a.Pf(26), a.Pf(2)
	c := colFaint
	if accent {
		c = colDim
	}
	for _, p := range [][4]float64{
		{float64(in.Min.X), float64(in.Min.Y), 1, 1}, {float64(in.Max.X), float64(in.Min.Y), -1, 1},
		{float64(in.Min.X), float64(in.Max.Y), 1, -1}, {float64(in.Max.X), float64(in.Max.Y), -1, -1},
	} {
		x11ui.Line(img, p[0], p[1], p[0]+p[2]*arm, p[1], w, c)
		x11ui.Line(img, p[0], p[1], p[0], p[1]+p[3]*arm, w, c)
	}
	cx, cy := float64(l.frame.Min.X+l.frame.Dx()/2), float64(l.frame.Min.Y+l.frame.Dy()/2)
	x11ui.Line(img, cx-a.Pf(8), cy, cx+a.Pf(8), cy, a.Pf(1.5), colFaint)
	x11ui.Line(img, cx, cy-a.Pf(8), cx, cy+a.Pf(8), a.Pf(1.5), colFaint)
}

// frameEyes disegna gli occhi; restituisce il campione e se è attuale.
func (l *liveEyes) frameEyes(a *App, img *image.RGBA, now time.Time) (gaze.Sample, bool) {
	s, ok := a.Ctrl.Hub.Latest()
	fresh := ok && now.Sub(time.Unix(0, s.HostTimeNs)) < 250*time.Millisecond
	if !fresh {
		return s, false
	}
	scale := 1.0
	if mid, ok := s.EyesTrackerMM(); ok {
		scale = math.Max(0.6, math.Min(1.8, 620/norm(mid)))
	}
	for _, e := range []gaze.Eye{s.Left, s.Right} {
		if !e.TrackBoxValid {
			continue
		}
		// TrackBox: x cresce verso la sinistra dell'utente → specchio con 1−x.
		x := float64(l.frame.Min.X) + (1-float64(e.TrackBox[0]))*float64(l.frame.Dx())
		y := float64(l.frame.Min.Y) + float64(e.TrackBox[1])*float64(l.frame.Dy())
		r := a.Pf(13) * scale
		a.Dyn(x11ui.Circle(img, x, y, r*2.6, x11ui.Alpha(colIR, 0.16)))
		a.Dyn(x11ui.Circle(img, x, y, r*1.6, x11ui.Alpha(colIR, 0.32)))
		a.Dyn(x11ui.Circle(img, x, y, r, colIR))
		a.Dyn(x11ui.Circle(img, x, y, r*0.32, colText))
	}
	return s, s.Left.Tracked || s.Right.Tracked
}

func norm(p [3]float32) float64 {
	return math.Sqrt(float64(p[0]*p[0] + p[1]*p[1] + p[2]*p[2]))
}

// ---------------------------------------------------------------------------

type step struct {
	key, title string
	run        func(a *App)
	rect       image.Rectangle
}

type home struct {
	eyes  liveEyes
	steps []step
	hover int
	info  []stepInfo
	conn  bool
}

type stepInfo struct {
	text string
	col  colorName
}

type colorName int

const (
	cDim colorName = iota
	cOK
	cWarn
)

func newHome() *home { return &home{hover: -1} }

func (h *home) Static(a *App, img *image.RGBA) {
	a.ShowCursor(true)
	x0 := a.Px(190)
	y := a.Px(200)
	x11ui.Text(img, a.F.Title, x0, y, "Tobii Eye Tracker 4C", colText, x11ui.Left)
	y += a.Px(44)
	dev, err := a.Ctrl.Device()
	h.conn = err == nil
	if err != nil {
		x11ui.Text(img, a.F.Body, x0, y, "Tracker non connesso: collegalo e attendi qualche secondo.", colWarn, x11ui.Left)
	} else {
		id := dev.Identity()
		line := "Tracker connesso"
		if len(id) >= 4 {
			line = fmt.Sprintf("%s, firmware %s, numero di serie %s", id[1], id[3], id[0])
		}
		x11ui.Text(img, a.F.Small, x0, y, line, colDim, x11ui.Left)
	}

	h.steps = []step{
		{"1", "Posiziona il sensore", func(a *App) { a.Flow(func() Screen { return newDisplaySetup() }) }, image.Rectangle{}},
		{"2", "Controlla la tua posizione", func(a *App) { a.Flow(func() Screen { return newPosition() }) }, image.Rectangle{}},
		{"3", "Calibra lo sguardo", func(a *App) {
			a.Flow(func() Screen { return newCalibration() }, func() Screen { return newVerify(false) })
		}, image.Rectangle{}},
		{"4", "Verifica l'accuratezza", func(a *App) { a.Flow(func() Screen { return newVerify(true) }) }, image.Rectangle{}},
	}
	h.info = h.describe(a)
	y += a.Px(92)
	numX, textX := x0, x0+a.Px(48)
	for i := range h.steps {
		st := &h.steps[i]
		top := y - a.F.Heading.Ascent - a.Px(16)
		st.rect = image.Rect(x0-a.Px(24), top, x0+a.Px(760), y+a.Px(46))
		if i == h.hover {
			x11ui.RoundRect(img, st.rect, a.Pf(12), colPanel)
		}
		x11ui.Text(img, a.F.Heading, numX, y, st.key, colFaint, x11ui.Left)
		x11ui.Text(img, a.F.Heading, textX, y, st.title, colText, x11ui.Left)
		inf := h.info[i]
		c := colDim
		switch inf.col {
		case cOK:
			c = colOK
		case cWarn:
			c = colWarn
		}
		x11ui.Text(img, a.F.Small, textX, y+a.Px(32), inf.text, c, x11ui.Left)
		y += a.Px(104)
	}
	a.drawKeys(img, x0, a.H-a.Px(110), []keyHint{
		{"Invio", "configurazione completa"}, {"1–4", "un solo passo"}, {"Esc", "esci"},
	}, x11ui.Left)

	fw, fh := a.Px(560), a.Px(380)
	fx := a.W - a.Px(190) - fw
	h.eyes.frame = image.Rect(fx, a.Px(250), fx+fw, a.Px(250)+fh)
	h.eyes.static(a, img, false)
}

// describe produce la riga di stato sotto ogni passo.
func (h *home) describe(a *App) []stepInfo {
	out := make([]stepInfo, 4)
	m := a.Monitor
	monName := m.Name
	if m.Model != "" {
		monName = m.Model
	}
	out[0] = stepInfo{"Display area non disponibile", cDim}
	if dev, err := a.Ctrl.Device(); err == nil {
		if d, _ := dev.DisplayInfo(); d != nil {
			w, hh := d.Width(), d.Height()
			if m.WidthMM <= 0 || m.HeightMM <= 0 {
				out[0] = stepInfo{fmt.Sprintf("Display area %.0f × %.0f mm; le misure di %s non sono note", w, hh, monName), cDim}
			} else if math.Abs(w-m.WidthMM)/m.WidthMM > 0.03 || math.Abs(hh-m.HeightMM)/m.HeightMM > 0.03 {
				out[0] = stepInfo{fmt.Sprintf("La display area (%.0f × %.0f mm) non corrisponde a %s (%.0f × %.0f mm)",
					w, hh, monName, m.WidthMM, m.HeightMM), cWarn}
			} else {
				out[0] = stepInfo{fmt.Sprintf("Display area %.0f × %.0f mm, adatta a %s", w, hh, monName), cOK}
			}
		}
	}
	out[1] = stepInfo{"Siediti a 50–80 cm, con gli occhi al centro del riquadro", cDim}
	out[2] = stepInfo{"Nessuna calibrazione salvata: il tracker usa quella interna", cDim}
	if p, err := store.CalibrationPath(); err == nil {
		if st, err := os.Stat(p); err == nil {
			out[2] = stepInfo{"Calibrazione salvata il " + itDate(st.ModTime()), cOK}
		}
	}
	out[3] = stepInfo{"Non ancora verificata in questa sessione", cDim}
	if a.HaveAccuracy {
		v, c := verdict(a.LastAccuracy)
		out[3] = stepInfo{fmt.Sprintf("Accuratezza %s°, precisione %s°: %s", it(a.LastAccuracy, 1), it(a.LastPrecision, 2), v), c}
	}
	return out
}

func (h *home) Frame(a *App, img *image.RGBA, now time.Time) {
	s, seen := h.eyes.frameEyes(a, img, now)
	x := h.eyes.frame.Min.X
	y := h.eyes.frame.Max.Y + a.Px(48)
	msg, c := "Non vedo i tuoi occhi", colDim
	if mid, ok := s.EyesTrackerMM(); seen && ok {
		msg, c = fmt.Sprintf("Ti vedo a %s cm", it(norm(mid)/10, 0)), colText
	} else if seen {
		msg, c = "Vedo un solo occhio", colDim
	}
	a.Dyn(x11ui.Text(img, a.F.Body, x, y, msg, c, x11ui.Left))
	// Lo stato di connessione cambia nel tempo: aggiorna lo statico se serve.
	if _, err := a.Ctrl.Device(); (err == nil) != h.conn {
		a.RefreshStatic()
	}
}

func (h *home) Event(a *App, ev x11ui.Event) {
	switch ev.Kind {
	case x11ui.KeyDown:
		switch ev.Sym {
		case x11ui.KeyEscape, 'q':
			a.Quit()
		case x11ui.KeyReturn, x11ui.KeyKPEnter:
			if h.ready(a) {
				a.Flow(func() Screen { return newDisplaySetup() }, func() Screen { return newPosition() },
					func() Screen { return newCalibration() }, func() Screen { return newVerify(false) })
			}
		case '1', '2', '3', '4':
			if h.ready(a) {
				h.steps[ev.Sym-'1'].run(a)
			}
		}
	case x11ui.MouseMove:
		hv := -1
		for i, st := range h.steps {
			if image.Pt(ev.X, ev.Y).In(st.rect) {
				hv = i
			}
		}
		if hv != h.hover {
			h.hover = hv
			a.RefreshStatic()
		}
	case x11ui.MouseDown:
		if ev.Button == 1 && h.hover >= 0 && h.ready(a) {
			h.steps[h.hover].run(a)
		}
	}
}

func (h *home) ready(a *App) bool {
	_, err := a.Ctrl.Device()
	return err == nil
}

// verdictSentence è il giudizio per esteso, per la schermata dei risultati.
func verdictSentence(deg float64) (string, colorName) {
	switch {
	case deg < 1.0:
		return "Ottima: il tracker è pronto", cOK
	case deg < 1.5:
		return "Buona: il tracker è pronto", cOK
	case deg < 2.5:
		return "Discreta: ricalibrare può migliorarla", cWarn
	}
	return "Scarsa: ricalibra, dopo aver controllato la posizione", cWarn
}

// verdict traduce l'accuratezza media in un giudizio.
func verdict(deg float64) (string, colorName) {
	switch {
	case deg < 1.0:
		return "ottima", cOK
	case deg < 1.5:
		return "buona", cOK
	case deg < 2.5:
		return "discreta, ricalibrare può migliorarla", cWarn
	}
	return "scarsa, ricalibra", cWarn
}
