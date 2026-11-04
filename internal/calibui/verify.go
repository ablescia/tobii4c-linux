package calibui

import (
	"context"
	"fmt"
	"image"
	"math"
	"time"

	"github.com/ablescia/tobii4c-linux/internal/calib"
	"github.com/ablescia/tobii4c-linux/internal/x11ui"
)

type verPhase int

const (
	verIntro verPhase = iota
	verRunning
	verResults
	verMessage
)

// verify misura accuratezza e precisione su 5 punti diversi da quelli di
// calibrazione, poi mostra il risultato.
type verify struct {
	standalone bool // avviata dal menu (non subito dopo una calibrazione)
	phase      verPhase
	idx        int
	dot        dotAnim
	results    []calib.PointResult
	meanAcc    float64
	meanPrec   float64
	showGaze   bool
	canRevert  bool
	message    string
	err        string
}

func newVerify(standalone bool) *verify { return &verify{standalone: standalone} }

func (v *verify) Static(a *App, img *image.RGBA) {
	cx := a.W / 2
	switch v.phase {
	case verIntro:
		a.ShowCursor(false)
		title := "Controlliamo l'accuratezza"
		body := "Guarda i 5 punti che compaiono, come durante la calibrazione."
		if !v.standalone {
			title = "Ora controlliamo il risultato"
		}
		x11ui.Text(img, a.F.Title, cx, a.Px(430), title, colText, x11ui.Center)
		a.paragraph(img, a.F.Body, cx, a.Px(490), a.Px(760), body, colDim, x11ui.Center)
		a.drawKeys(img, cx, a.Px(600), []keyHint{{"Spazio", "inizia"}, {"Esc", "salta"}}, x11ui.Center)
	case verResults:
		a.ShowCursor(false)
		v.drawResults(a, img)
	case verMessage:
		x11ui.Text(img, a.F.Title, cx, a.H/2, v.message, colText, x11ui.Center)
	}
}

func (v *verify) drawResults(a *App, img *image.RGBA) {
	W, H := float64(a.W), float64(a.H)
	for _, r := range v.results {
		tx, ty := r.Target[0]*W, r.Target[1]*H
		for _, s := range r.Samples {
			x11ui.Circle(img, s[0]*W, s[1]*H, a.Pf(2.4), x11ui.Alpha(colIR, 0.55))
		}
		x11ui.Ring(img, tx, ty, a.Pf(16), a.Pf(2), x11ui.Alpha(colText, 0.8))
		x11ui.Line(img, tx-a.Pf(5), ty, tx+a.Pf(5), ty, a.Pf(1.5), colText)
		x11ui.Line(img, tx, ty-a.Pf(5), tx, ty+a.Pf(5), a.Pf(1.5), colText)
		label := "nessun dato"
		// L'etichetta va dal lato opposto all'errore, per non coprire lo sguardo medio.
		ux, uy := 0.8, -0.6
		if r.N > 0 {
			mx, my := r.Mean[0]*W, r.Mean[1]*H
			x11ui.Line(img, tx, ty, mx, my, a.Pf(2), colText)
			x11ui.Circle(img, mx, my, a.Pf(6), colText)
			label = it(r.ErrorDeg, 1) + "°"
			if d := math.Hypot(mx-tx, my-ty); d > a.Pf(4) {
				ux, uy = -(mx-tx)/d, -(my-ty)/d
			}
		}
		lx, ly := tx+ux*a.Pf(40), ty+uy*a.Pf(40)+float64(a.F.Body.Ascent)/2
		al := x11ui.Left
		if ux < -0.3 {
			al = x11ui.Right
		} else if ux < 0.3 {
			al = x11ui.Center
		}
		x11ui.Text(img, a.F.Body, int(lx), int(ly), label, colText, al)
	}
	// Riepilogo nella fascia libera sotto i bersagli più bassi (y = 0,75).
	cx := a.W / 2
	panel := image.Rect(cx-a.Px(560), int(0.81*H), cx+a.Px(560), int(0.81*H)+a.Px(150))
	x11ui.RoundRect(img, panel, a.Pf(16), colPanel)
	y := panel.Min.Y + a.Px(48)
	if v.err != "" {
		x11ui.Text(img, a.F.Heading, cx, y, "Verifica non riuscita", colText, x11ui.Center)
		x11ui.Text(img, a.F.Small, cx, y+a.Px(36), v.err, colWarn, x11ui.Center)
	} else {
		vd, c := verdictSentence(v.meanAcc)
		col := colOK
		if c == cWarn {
			col = colWarn
		}
		head := fmt.Sprintf("Accuratezza %s°   Precisione %s°", it(v.meanAcc, 1), it(v.meanPrec, 2))
		hw := a.F.Heading.Measure(head)
		vw := a.F.Body.Measure(vd)
		x := cx - (hw+a.Px(36)+vw)/2
		x11ui.Text(img, a.F.Heading, x, y, head, colText, x11ui.Left)
		x11ui.Text(img, a.F.Body, x+hw+a.Px(36), y, vd, col, x11ui.Left)
	}
	y += a.Px(62)
	keys := []keyHint{{"Invio", "fatto"}, {"R", "ricalibra"}}
	if v.canRevert {
		keys = append(keys, keyHint{"P", "ripristina la precedente"})
	}
	keys = append(keys, keyHint{"G", map[bool]string{false: "mostra lo sguardo", true: "nascondi lo sguardo"}[v.showGaze]})
	a.drawKeys(img, cx, y, keys, x11ui.Center)
}

func (v *verify) Frame(a *App, img *image.RGBA, now time.Time) {
	switch v.phase {
	case verRunning:
		if v.dot.draw(a, img, now, false) {
			v.measure(a)
		}
		if v.dot.popping && now.Sub(v.dot.popStart) >= popTime {
			v.dot.popping = false
			v.next(a, now)
		}
	case verResults:
		if v.showGaze {
			s, ok := a.Ctrl.Hub.Latest()
			if ok && s.GazeValid && now.Sub(time.Unix(0, s.HostTimeNs)) < 200*time.Millisecond {
				x, y := float64(s.Gaze[0])*float64(a.W), float64(s.Gaze[1])*float64(a.H)
				a.Dyn(x11ui.Circle(img, x, y, a.Pf(34), x11ui.Alpha(colText, 0.18)))
				a.Dyn(x11ui.Ring(img, x, y, a.Pf(34), a.Pf(2), x11ui.Alpha(colText, 0.6)))
			}
		}
	}
}

func (v *verify) start(a *App) {
	v.phase, v.idx, v.results = verRunning, 0, nil
	a.RefreshStatic()
	v.dot = dotAnim{to: [2]float64{0.5, 0.5}}
	v.dot.moveTo(calib.ValidationPoints[0], time.Now())
}

func (v *verify) measure(a *App) {
	p := calib.ValidationPoints[v.idx]
	a.Ctrl.Show(p[0], p[1], true)
	Go(a, func(ctx context.Context) result {
		r, err := a.Ctrl.Validate(ctx, p[0], p[1])
		return result{r, err}
	}, func(res result) {
		if v.phase != verRunning {
			return
		}
		if res.err != nil {
			v.err = res.err.Error()
			v.finish(a)
			return
		}
		v.results = append(v.results, res.r)
		v.dot.popping, v.dot.popStart = true, time.Now()
	})
}

type result struct {
	r   calib.PointResult
	err error
}

func (v *verify) next(a *App, now time.Time) {
	v.idx++
	if v.idx < len(calib.ValidationPoints) {
		v.dot.moveTo(calib.ValidationPoints[v.idx], now)
		return
	}
	v.finish(a)
}

func (v *verify) finish(a *App) {
	a.Ctrl.Show(0, 0, false)
	var sa, sp float64
	n := 0
	for _, r := range v.results {
		if r.N > 0 {
			sa += r.ErrorDeg
			sp += r.PrecisionMS
			n++
		}
	}
	if n > 0 {
		v.meanAcc, v.meanPrec = sa/float64(n), sp/float64(n)
		a.LastAccuracy, a.LastPrecision, a.HaveAccuracy = v.meanAcc, v.meanPrec, true
	} else if v.err == "" {
		v.err = "Nessuno sguardo valido durante la verifica: controlla la posizione."
	}
	v.canRevert = a.Ctrl.CanRevert()
	v.phase = verResults
	a.RefreshStatic()
}

func (v *verify) Event(a *App, ev x11ui.Event) {
	if ev.Kind != x11ui.KeyDown {
		return
	}
	switch v.phase {
	case verIntro:
		switch ev.Sym {
		case x11ui.KeySpace, x11ui.KeyReturn, x11ui.KeyKPEnter:
			v.start(a)
		case x11ui.KeyEscape:
			a.Next()
		}
	case verRunning:
		if ev.Sym == x11ui.KeyEscape {
			a.Ctrl.Show(0, 0, false)
			a.Next()
		}
	case verResults:
		switch ev.Sym {
		case x11ui.KeyReturn, x11ui.KeyKPEnter, x11ui.KeyEscape:
			a.Next()
		case 'g':
			v.showGaze = !v.showGaze
			a.RefreshStatic()
		case 'r':
			a.Flow(func() Screen { return newCalibration() }, func() Screen { return newVerify(false) })
		case 'p':
			if !v.canRevert {
				return
			}
			Go(a, func(context.Context) error { return a.Ctrl.Revert() }, func(err error) {
				v.phase = verMessage
				v.message = "Ho rimesso la calibrazione precedente"
				if err != nil {
					v.message = "Ripristino non riuscito: " + err.Error()
				}
				a.HaveAccuracy = false
				a.RefreshStatic()
				a.After(1400*time.Millisecond, a.Home)
			})
		}
	}
}
