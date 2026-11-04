package calibui

import (
	"context"
	"image"
	"math"
	"time"

	"github.com/ablescia/tobii4c-linux/internal/calib"
	"github.com/ablescia/tobii4c-linux/internal/ttp"
	"github.com/ablescia/tobii4c-linux/internal/x11ui"
)

// Tempi dell'animazione del punto.
const (
	travelTime   = 450 * time.Millisecond // spostamento da un punto al successivo
	shrinkTime   = 1100 * time.Millisecond
	popTime      = 240 * time.Millisecond
	collectGuess = 750 * time.Millisecond // durata tipica del collect sul dispositivo
	maxAttempts  = 3
)

type calPhase int

const (
	calIntro calPhase = iota
	calStarting
	calRunning
	calFailed
	calDone
)

// dotAnim anima il punto di stimolo (usato da calibrazione e verifica).
type dotAnim struct {
	from, to     [2]float64
	travelStart  time.Time
	arrived      bool
	waitStart    time.Time
	collectStart time.Time
	popStart     time.Time
	popping      bool
}

func (d *dotAnim) moveTo(p [2]float64, now time.Time) {
	d.from, d.to = d.pos(now), p
	d.travelStart, d.arrived, d.popping = now, false, false
	d.collectStart = time.Time{}
}

func (d *dotAnim) pos(now time.Time) [2]float64 {
	if d.travelStart.IsZero() {
		return d.to
	}
	t := ease(float64(now.Sub(d.travelStart)) / float64(travelTime))
	return [2]float64{d.from[0] + (d.to[0]-d.from[0])*t, d.from[1] + (d.to[1]-d.from[1])*t}
}

// draw disegna il punto; restituisce true quando è arrivato a destinazione
// per la prima volta in questo frame.
func (d *dotAnim) draw(a *App, img *image.RGBA, now time.Time, shrink bool) bool {
	p := d.pos(now)
	x, y := p[0]*float64(a.W), p[1]*float64(a.H)
	justArrived := false
	if !d.arrived && now.Sub(d.travelStart) >= travelTime {
		d.arrived, d.waitStart, justArrived = true, now, true
	}
	st := dotStyle{radius: 22, progress: -1, pop: -1}
	switch {
	case d.popping:
		st.pop = float64(now.Sub(d.popStart)) / float64(popTime)
		if st.pop >= 1 {
			return justArrived
		}
	case d.arrived && shrink:
		// Si contrae mentre l'utente fissa: invita a guardarne il centro.
		st.radius = 22 - 12*ease(float64(now.Sub(d.waitStart))/float64(shrinkTime))
		if !d.collectStart.IsZero() {
			st.progress = math.Min(0.97, float64(now.Sub(d.collectStart))/float64(collectGuess))
		}
	case d.arrived:
		// Verifica: respiro leggero, nessuna contrazione.
		st.radius = 16 + 1.5*math.Sin(float64(now.Sub(d.waitStart))/float64(time.Second)*2*math.Pi)
	}
	a.drawDot(img, x, y, st)
	return justArrived
}

// ---------------------------------------------------------------------------

type calibration struct {
	phase       calPhase
	order       []int // indici in CalibrationPoints7, batch dopo batch
	batchEnd    map[int]bool
	idx         int
	attempt     int
	dot         dotAnim
	busy        bool
	computing   bool
	computeFrom time.Time
	hint        string
	hintUntil   time.Time
	failure     string
	cancel      context.CancelFunc
}

func newCalibration() *calibration {
	c := &calibration{batchEnd: map[int]bool{}}
	for _, b := range ttp.CalibrationBatches7 {
		c.order = append(c.order, b...)
		c.batchEnd[len(c.order)-1] = true
	}
	return c
}

func (c *calibration) point(i int) [2]float64 {
	p := ttp.CalibrationPoints7[c.order[i]]
	return [2]float64{float64(p[0]), float64(p[1])}
}

func (c *calibration) Static(a *App, img *image.RGBA) {
	cx := a.W / 2
	switch c.phase {
	case calIntro:
		a.ShowCursor(false)
		x11ui.Text(img, a.F.Title, cx, a.Px(430), "Segui il punto con lo sguardo", colText, x11ui.Center)
		a.paragraph(img, a.F.Body, cx, a.Px(490), a.Px(760),
			"Tieni ferma la testa e guarda il centro del punto finché non scompare. Sono 7 punti, circa 20 secondi.",
			colDim, x11ui.Center)
		a.drawKeys(img, cx, a.Px(640), []keyHint{{"Spazio", "inizia"}, {"Esc", "annulla"}}, x11ui.Center)
	case calFailed:
		a.ShowCursor(true)
		x11ui.Text(img, a.F.Title, cx, a.Px(450), "La calibrazione non è riuscita", colText, x11ui.Center)
		a.paragraph(img, a.F.Body, cx, a.Px(510), a.Px(820), c.failure, colDim, x11ui.Center)
		a.drawKeys(img, cx, a.Px(660), []keyHint{{"R", "riprova"}, {"Esc", "torna al menu"}}, x11ui.Center)
	case calDone:
		x11ui.Text(img, a.F.Title, cx, a.H/2, "Calibrazione completata", colText, x11ui.Center)
	}
}

func (c *calibration) Frame(a *App, img *image.RGBA, now time.Time) {
	switch c.phase {
	case calStarting:
		a.Dyn(x11ui.Text(img, a.F.Body, a.W/2, a.H/2, "Preparo il tracker…", colDim, x11ui.Center))
	case calRunning:
		if c.computing {
			// Spinner discreto solo se il calcolo si fa attendere.
			if el := now.Sub(c.computeFrom); el > 250*time.Millisecond {
				ang := float64(el) / float64(time.Second) * 2 * math.Pi
				cx, cy := float64(a.W)/2, float64(a.H)/2
				a.Dyn(x11ui.Arc(img, cx, cy, a.Pf(26), a.Pf(4), ang, ang+math.Pi*1.2, x11ui.Alpha(colDot, 0.8)))
				a.Dyn(x11ui.Text(img, a.F.Small, a.W/2, a.H/2+a.Px(70), "Elaboro la calibrazione", colDim, x11ui.Center))
			}
			return
		}
		if c.dot.draw(a, img, now, true) {
			c.collect(a)
		}
		if c.dot.popping && now.Sub(c.dot.popStart) >= popTime {
			c.dot.popping = false
			c.advance(a, now)
		}
		if c.hint != "" && now.Before(c.hintUntil) {
			a.Dyn(x11ui.Text(img, a.F.Heading, a.W/2, a.H-a.Px(160), c.hint, colWarn, x11ui.Center))
		}
	}
}

func (c *calibration) start(a *App) {
	c.phase, c.idx, c.attempt = calStarting, 0, 0
	a.RefreshStatic()
	Go(a, func(context.Context) error { return a.Ctrl.Begin() }, func(err error) {
		if err != nil {
			c.fail(a, "Il tracker non ha avviato la calibrazione: "+err.Error())
			return
		}
		c.phase = calRunning
		a.RefreshStatic()
		now := time.Now()
		c.dot = dotAnim{to: [2]float64{0.5, 0.5}}
		c.dot.moveTo(c.point(0), now)
	})
}

// collect parte quando il punto è arrivato: il controller attende la
// fissazione, poi il dispositivo raccoglie i campioni.
func (c *calibration) collect(a *App) {
	p := c.point(c.idx)
	a.Ctrl.Show(p[0], p[1], true)
	c.busy = true
	ctx, cancel := context.WithCancel(a.ctx)
	c.cancel = cancel
	Go(a, func(context.Context) calib.Outcome {
		return a.Ctrl.Collect(ctx, p[0], p[1], func() {
			now := time.Now()
			a.Post(func() { c.dot.collectStart = now })
		})
	}, func(o calib.Outcome) {
		c.busy = false
		if c.phase != calRunning {
			return
		}
		now := time.Now()
		if o.OK {
			c.attempt = 0
			c.dot.popping, c.dot.popStart = true, now
			return
		}
		if o.Err != nil && o.Reason == "" {
			c.fail(a, "Errore del tracker: "+o.Err.Error())
			return
		}
		c.attempt++
		reason := map[string]string{
			"no_eyes":        "Non vedo i tuoi occhi: guarda il punto",
			"lost_eyes":      "Ti ho perso durante la raccolta: guarda ancora il punto",
			"device_refused": "Il tracker ha rifiutato il punto: riproviamo",
		}[o.Reason]
		if c.attempt >= maxAttempts {
			c.fail(a, reason+". Controlla la posizione davanti allo schermo e riprova.")
			return
		}
		c.hint, c.hintUntil = reason, now.Add(1800*time.Millisecond)
		// Ripresenta lo stesso punto con un piccolo salto, per riattirare lo sguardo.
		c.dot.waitStart = now
		c.dot.arrived = false
		c.dot.travelStart = now.Add(-travelTime / 2)
		c.dot.from = c.dot.to
	})
}

// advance passa al punto successivo, o calcola alla fine di un batch.
func (c *calibration) advance(a *App, now time.Time) {
	if c.batchEnd[c.idx] {
		c.computing, c.computeFrom = true, now
		last := c.idx == len(c.order)-1
		Go(a, func(context.Context) error { _, err := a.Ctrl.Compute(); return err }, func(err error) {
			c.computing = false
			if err != nil {
				c.fail(a, "Il tracker non è riuscito a calcolare la calibrazione: "+err.Error())
				return
			}
			if last {
				c.finish(a)
				return
			}
			c.idx++
			c.dot.moveTo(c.point(c.idx), time.Now())
		})
		return
	}
	c.idx++
	c.dot.moveTo(c.point(c.idx), now)
}

func (c *calibration) finish(a *App) {
	a.Ctrl.Show(0, 0, false)
	Go(a, func(context.Context) error { _, err := a.Ctrl.Finish(); return err }, func(err error) {
		if err != nil {
			c.fail(a, "Chiusura della calibrazione non riuscita: "+err.Error())
			return
		}
		c.phase = calDone
		a.RefreshStatic()
		a.After(1100*time.Millisecond, a.Next)
	})
}

func (c *calibration) fail(a *App, msg string) {
	a.Log.Warn("calibration interrupted", "reason", msg, "point", c.idx+1, "attempt", c.attempt)
	if c.cancel != nil {
		c.cancel()
	}
	a.Ctrl.Show(0, 0, false)
	c.phase, c.failure, c.computing = calFailed, msg, false
	a.RefreshStatic()
	Go(a, func(context.Context) bool { r, _ := a.Ctrl.Abort(); return r }, func(restored bool) {
		if restored {
			c.failure += " Ho rimesso la calibrazione precedente."
			a.RefreshStatic()
		}
	})
}

func (c *calibration) Event(a *App, ev x11ui.Event) {
	if ev.Kind != x11ui.KeyDown {
		return
	}
	switch c.phase {
	case calIntro:
		switch ev.Sym {
		case x11ui.KeySpace, x11ui.KeyReturn, x11ui.KeyKPEnter:
			c.start(a)
		case x11ui.KeyEscape:
			a.Home()
		}
	case calStarting, calRunning:
		if ev.Sym == x11ui.KeyEscape {
			c.fail(a, "Calibrazione annullata.")
		}
	case calFailed:
		switch ev.Sym {
		case 'r':
			a.SetScreen(newCalibration()) // il resto del flusso (la verifica) resta in coda
		case x11ui.KeyEscape:
			a.Home()
		}
	}
}
