// Package calibui è l'interfaccia nativa a schermo intero dell'app di
// configurazione, sul modello di quella ufficiale Tobii: posizionamento del
// sensore, posizione dell'utente, calibrazione a 7 punti, verifica.
//
// Disegno: due livelli. Lo statico (sfondo e testi) si ridisegna solo quando
// cambia la schermata; il dinamico (punto, occhi, valori dal vivo) a ogni
// frame, e al server X vengono inviate solo le regioni toccate.
package calibui

import (
	"context"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ablescia/tobii4c-linux/internal/calib"
	"github.com/ablescia/tobii4c-linux/internal/headpose"
	"github.com/ablescia/tobii4c-linux/internal/x11ui"
)

// Palette.
var (
	colBg     = x11ui.Hex(0x45494f) // grigio stimolo: luminanza media, pupilla stabile
	colPanel  = x11ui.Hex(0x3b3e43)
	colKey    = x11ui.Hex(0x5b6067)
	colText   = x11ui.Hex(0xf2f2ef)
	colDim    = x11ui.Hex(0xb3b8bf)
	colFaint  = x11ui.Hex(0x868c94)
	colIR     = x11ui.Hex(0xff5d5d) // ciò che il sensore vede dal vivo
	colOK     = x11ui.Hex(0x93d3a2)
	colWarn   = x11ui.Hex(0xf1c56b)
	colBad    = x11ui.Hex(0xff8a80)
	colDot    = x11ui.Hex(0xffffff)
	colDotEye = x11ui.Hex(0x202326)
)

// Screen è una schermata.
type Screen interface {
	Static(a *App, img *image.RGBA)
	Frame(a *App, img *image.RGBA, now time.Time)
	Event(a *App, ev x11ui.Event)
}

// App è l'applicazione.
type App struct {
	Ctrl    *calib.Controller
	Log     *slog.Logger
	Monitor x11ui.Monitor
	F       *x11ui.Fonts
	W, H    int
	S       float64 // scala rispetto a 1080p

	win     *x11ui.Window
	static  *image.RGBA
	canvas  *image.RGBA
	prevDyn []image.Rectangle
	curDyn  []image.Rectangle
	full    bool
	screen  Screen
	async   chan func()
	ctx     context.Context
	cancel  context.CancelFunc
	flow    []func() Screen
	done    bool

	// Ultima verifica di questa esecuzione (per la schermata iniziale).
	LastAccuracy  float64
	LastPrecision float64
	HaveAccuracy  bool

	// HP è lo stimatore della posa della testa (applicazione
	// headpose-calibrate); Start, se non nil, sostituisce il menu iniziale.
	HP    *headpose.Estimator
	Start func() Screen

	// ShotDir: se non vuoto, salva la finestra come PNG a ogni cambio di
	// schermata e ogni 3 s (per controllare l'impaginazione a distanza).
	ShotDir  string
	lastShot time.Time
	shotN    int
}

// home è la schermata iniziale: il menu della calibrazione o quella di Start.
func (a *App) home() Screen {
	if a.Start != nil {
		return a.Start()
	}
	return newHome()
}

// New apre la finestra sul monitor m.
func New(win *x11ui.Window, ctrl *calib.Controller, log *slog.Logger) (*App, error) {
	f, err := x11ui.LoadFonts(win.H)
	if err != nil {
		return nil, err
	}
	b := image.Rect(0, 0, win.W, win.H)
	return &App{Ctrl: ctrl, Log: log, Monitor: win.Monitor, F: f, W: win.W, H: win.H, S: float64(win.H) / 1080,
		win: win, static: image.NewRGBA(b), canvas: image.NewRGBA(b), async: make(chan func(), 16)}, nil
}

// Px scala una misura pensata per 1080p.
func (a *App) Px(v float64) int { return int(math.Round(v * a.S)) }

// Pf come Px ma in virgola mobile.
func (a *App) Pf(v float64) float64 { return v * a.S }

// Dyn registra una regione disegnata nel livello dinamico.
func (a *App) Dyn(r image.Rectangle) {
	if !r.Empty() {
		a.curDyn = append(a.curDyn, r)
	}
}

// SetScreen mostra una schermata.
func (a *App) SetScreen(s Screen) {
	a.screen = s
	a.RefreshStatic()
}

// RefreshStatic ridisegna il livello statico della schermata corrente.
func (a *App) RefreshStatic() {
	x11ui.Fill(a.static, a.static.Bounds(), colBg)
	a.screen.Static(a, a.static)
	a.full = true
}

// Go esegue fn fuori dal loop dell'interfaccia e consegna il risultato a done
// nel loop (nessun accesso concorrente allo stato delle schermate).
func Go[T any](a *App, fn func(ctx context.Context) T, done func(T)) {
	ctx := a.ctx
	go func() {
		r := fn(ctx)
		select {
		case a.async <- func() { done(r) }:
		case <-ctx.Done():
		}
	}()
}

// Post consegna fn al loop dell'interfaccia (da qualsiasi goroutine).
func (a *App) Post(fn func()) {
	select {
	case a.async <- fn:
	case <-a.ctx.Done():
	}
}

// After esegue fn nel loop dopo d.
func (a *App) After(d time.Duration, fn func()) {
	ctx := a.ctx
	go func() {
		select {
		case <-time.After(d):
			select {
			case a.async <- fn:
			case <-ctx.Done():
			}
		case <-ctx.Done():
		}
	}()
}

// Flow imposta i passi successivi e avvia il primo.
func (a *App) Flow(steps ...func() Screen) {
	a.flow = steps
	a.Next()
}

// Next passa al passo successivo del flusso, o alla schermata iniziale.
func (a *App) Next() {
	if len(a.flow) == 0 {
		a.SetScreen(a.home())
		return
	}
	next := a.flow[0]
	a.flow = a.flow[1:]
	a.SetScreen(next())
}

// Home torna alla schermata iniziale abbandonando il flusso.
func (a *App) Home() {
	a.flow = nil
	a.SetScreen(a.home())
}

// Quit chiude l'applicazione.
func (a *App) Quit() { a.done = true }

// ShowCursor mostra o nasconde il puntatore.
func (a *App) ShowCursor(v bool) { a.win.ShowCursor(v) }

// Run esegue il loop a 60 Hz finché l'utente non esce o ctx termina.
func (a *App) Run(ctx context.Context) error {
	a.ctx, a.cancel = context.WithCancel(ctx)
	defer a.cancel()
	a.SetScreen(a.home())
	tk := time.NewTicker(time.Second / 60)
	defer tk.Stop()
	for !a.done {
		select {
		case <-a.ctx.Done():
			return nil
		case ev := <-a.win.Events:
			switch ev.Kind {
			case x11ui.Closed:
				return nil
			case x11ui.Expose:
				a.full = true
			default:
				a.screen.Event(a, ev)
			}
		case fn := <-a.async:
			fn()
		case now := <-tk.C:
			if err := a.frame(now); err != nil {
				return err
			}
		}
	}
	return nil
}

func (a *App) frame(now time.Time) error {
	// Una RefreshStatic chiamata durante screen.Frame (es. fine animazione →
	// nuova fase) riporta a.full a true e vale per il frame successivo:
	// questo canvas è già stato composto con lo statico precedente.
	full := a.full
	a.full = false
	if full {
		copy(a.canvas.Pix, a.static.Pix)
	} else {
		for _, r := range a.prevDyn {
			draw.Draw(a.canvas, r, a.static, r.Min, draw.Src)
		}
	}
	a.curDyn = a.curDyn[:0]
	a.screen.Frame(a, a.canvas, now)
	if a.ShotDir != "" && (full || now.Sub(a.lastShot) > 3*time.Second) {
		a.lastShot = now
		a.shotN++
		if fh, err := os.Create(filepath.Join(a.ShotDir, fmt.Sprintf("screen-%03d.png", a.shotN))); err == nil {
			png.Encode(fh, a.canvas)
			fh.Close()
		}
	}
	var err error
	if full {
		err = a.win.Present(a.canvas, a.canvas.Bounds())
	} else {
		err = a.win.Present(a.canvas, mergeRects(append(a.prevDyn, a.curDyn...))...)
	}
	a.prevDyn = append(a.prevDyn[:0], a.curDyn...)
	return err
}

// mergeRects unisce i rettangoli che si sovrappongono.
func mergeRects(rs []image.Rectangle) []image.Rectangle {
	out := make([]image.Rectangle, 0, len(rs))
	for _, r := range rs {
		if r.Empty() {
			continue
		}
		merged := true
		for merged {
			merged = false
			for i := 0; i < len(out); i++ {
				if out[i].Inset(-4).Overlaps(r) {
					r = r.Union(out[i])
					out = append(out[:i], out[i+1:]...)
					merged = true
					break
				}
			}
		}
		out = append(out, r)
	}
	return out
}

// ---------------------------------------------------------------------------
// Elementi comuni

// keyHint è un tasto con la sua azione.
type keyHint struct{ key, label string }

// drawKeys disegna una riga di tasti; restituisce la larghezza.
func (a *App) drawKeys(img *image.RGBA, x, baseline int, keys []keyHint, align x11ui.Align) image.Rectangle {
	gapKey, gapItem := a.Px(10), a.Px(34)
	padX, padY := a.Px(9), a.Px(6)
	total := 0
	for i, k := range keys {
		total += a.F.Key.Measure(k.key) + 2*padX + gapKey + a.F.Small.Measure(k.label)
		if i > 0 {
			total += gapItem
		}
	}
	switch align {
	case x11ui.Center:
		x -= total / 2
	case x11ui.Right:
		x -= total
	}
	box := image.Rect(x, baseline-a.F.Small.Ascent-padY*2, x+total, baseline+a.F.Small.Desc+padY)
	for _, k := range keys {
		kw := a.F.Key.Measure(k.key) + 2*padX
		cap := image.Rect(x, baseline-a.F.Key.Ascent-padY, x+kw, baseline+a.F.Key.Desc+padY)
		x11ui.RoundRect(img, cap, a.Pf(6), colKey)
		x11ui.Text(img, a.F.Key, x+padX, baseline, k.key, colText, x11ui.Left)
		x += kw + gapKey
		r := x11ui.Text(img, a.F.Small, x, baseline, k.label, colDim, x11ui.Left)
		x = r.Max.X + gapItem
	}
	return box
}

// paragraph disegna un testo a capo automatico; restituisce la y dopo l'ultima riga.
func (a *App) paragraph(img *image.RGBA, f *x11ui.Font, x, y, width int, s string, c color.RGBA, align x11ui.Align) int {
	lh := int(float64(f.Height) * 1.35)
	for _, line := range x11ui.Wrap(f, s, width) {
		x11ui.Text(img, f, x, y, line, c, align)
		y += lh
	}
	return y
}

// dotStyle è lo stato visivo del punto di stimolo.
type dotStyle struct {
	radius   float64 // raggio del disco bianco (px a 1080p)
	progress float64 // arco di raccolta 0..1 (<0 = nessuno)
	pop      float64 // animazione di conferma 0..1 (<0 = nessuna)
}

// drawDot disegna il punto: disco bianco con centro scuro, arco di
// avanzamento durante la raccolta, anello che si espande alla conferma.
func (a *App) drawDot(img *image.RGBA, x, y float64, st dotStyle) {
	if st.pop >= 0 {
		r := a.Pf(10 + 30*st.pop)
		a.Dyn(x11ui.Ring(img, x, y, r, a.Pf(3), x11ui.Alpha(colDot, 1-st.pop)))
		return
	}
	r := a.Pf(st.radius)
	a.Dyn(x11ui.Circle(img, x, y, r, colDot))
	a.Dyn(x11ui.Circle(img, x, y, math.Max(a.Pf(2.5), r*0.24), colDotEye))
	if st.progress >= 0 {
		a.Dyn(x11ui.Arc(img, x, y, a.Pf(31), a.Pf(3.5), -math.Pi/2, -math.Pi/2+2*math.Pi*st.progress, x11ui.Alpha(colDot, 0.85)))
	}
}

// ease: curva morbida in-out.
func ease(t float64) float64 {
	t = math.Max(0, math.Min(1, t))
	return t * t * (3 - 2*t)
}

// decimale all'italiana (senza "-0").
func it(v float64, digits int) string {
	s := fmt.Sprintf("%.*f", digits, v)
	if strings.Trim(s, "-0.") == "" {
		s = strings.TrimPrefix(s, "-")
	}
	return strings.Replace(s, ".", ",", 1)
}

var mesi = []string{"gennaio", "febbraio", "marzo", "aprile", "maggio", "giugno", "luglio",
	"agosto", "settembre", "ottobre", "novembre", "dicembre"}

func itDate(t time.Time) string {
	return fmt.Sprintf("%d %s alle %02d:%02d", t.Day(), mesi[t.Month()-1], t.Hour(), t.Minute())
}
