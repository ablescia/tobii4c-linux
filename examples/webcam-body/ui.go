//go:build linux

package main

import (
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ablescia/tobii4c-linux/internal/bodypose"
	"github.com/ablescia/tobii4c-linux/internal/x11ui"
)

// Palette della finestra di telemetria del driver (come webcam-face).
var (
	colBg     = x11ui.Hex(0x45494f)
	colPanel  = x11ui.Hex(0x3a3d42)
	colLine   = x11ui.Hex(0x4c5056)
	colKey    = x11ui.Hex(0x5b6067)
	colText   = x11ui.Hex(0xf2f2ef)
	colDim    = x11ui.Hex(0xb3b8bf)
	colFaint  = x11ui.Hex(0x868c94)
	colOK     = x11ui.Hex(0x93d3a2)
	colWarn   = x11ui.Hex(0xf1c56b)
	colBad    = x11ui.Hex(0xff8a80)
	colLeft   = x11ui.Hex(0x6cb6ff) // lato sinistro del soggetto
	colRight  = x11ui.Hex(0xffa75e) // lato destro
	colTorso  = x11ui.Hex(0xf2f2ef)
	colFace   = x11ui.Hex(0x5ae678)
	colMask   = x11ui.Hex(0x5ae678)
	colCrop   = x11ui.Hex(0xf1c56b)
	colDet    = x11ui.Hex(0xff8ad8)
	colPlotBg = x11ui.Hex(0x2e3135)
	colAccent = x11ui.Hex(0x4fe0e0)
	colSide   = [2]color.RGBA{colLeft, colRight}
)

// Superficie logica di riferimento: a 1920×1080 la scala vale circa 1.
const (
	baseW = 1880.0
	baseH = 1000.0
)

// Modalità del disegno sopra l'anteprima (tasto M).
const (
	drawSkeleton = iota
	drawSkeletonMask
	drawMask
	drawNone
	drawModes
)

var drawNames = [drawModes]string{"scheletro", "scheletro e maschera", "maschera", "solo immagine"}

type fonts struct {
	title, head, label, small, mono, monoS, big *x11ui.Font
}

// card è un riquadro: titolo e sfondo si disegnano nei ridisegni completi,
// il corpo ogni every tick (0: a ogni frame nuovo della webcam).
type card struct {
	title string
	note  func(a *App) string
	want  float64 // altezza desiderata (unità logiche), per l'impaginazione
	every int
	draw  func(a *App, img *image.RGBA, b image.Rectangle)
	dirty bool // da ridisegnare al prossimo tick

	r, head, body image.Rectangle
	titleEnd      int
}

func (c *card) set(a *App, r image.Rectangle) {
	c.r = r
	pad := a.px(12)
	c.head = image.Rect(r.Min.X+pad, r.Min.Y+a.px(8), r.Max.X-pad, r.Min.Y+a.px(28))
	c.body = image.Rect(r.Min.X+pad, c.head.Max.Y+a.px(10), r.Max.X-pad, r.Max.Y-a.px(10))
	if c.body.Dy() < 0 {
		c.body.Max.Y = c.body.Min.Y
	}
}

func (c *card) noteRect() image.Rectangle {
	r := c.head
	r.Min.X = min(r.Max.X, max(r.Min.X, c.titleEnd+8))
	return r
}

// App è la finestra: comandi e anteprima a sinistra, telemetria a destra.
type App struct {
	Eng     *Engine
	ShotDir string

	win    *x11ui.Window
	W, H   int
	s      float64
	f      fonts
	canvas *image.RGBA
	cards  []*card
	radios []*radioGroup
	full   bool
	frameN int

	snap     Snapshot
	now      time.Time
	rates    rates
	base     Counters // azzeramento dei contatori (tasto R)
	sys      sysStats
	drawMode int
	mirror   bool
	paused   bool
	pvDirty  bool
	msg      string
	msgUntil time.Time
	quit     bool
	async    chan func()
	mouse    image.Point

	// tempi dell'interfaccia
	pvFrames  int
	drawMs    float64
	presentMs float64
	pv        previewRenderer
}

// NewApp prepara l'interfaccia su win (nil: solo rendering in memoria).
func NewApp(win *x11ui.Window, eng *Engine, w, h int, mirror bool) *App {
	a := &App{Eng: eng, win: win, W: w, H: h, mirror: mirror, drawMode: drawSkeleton, ShotDir: ".",
		async: make(chan func(), 16), sys: newSysStats(), now: time.Now()}
	a.radios = newRadios()
	a.cards = []*card{
		{title: "Rilevamento · tipologia, modello e calcolo", note: (*App).controlNote, every: 6, draw: (*App).drawControls},
		{title: "Webcam · a colori", note: (*App).previewNote, draw: (*App).drawPreview},
		{title: "Andamento · ultimi 10 s", every: 2, draw: (*App).drawPlots},
		// colonna A
		{title: "Postura", note: (*App).postureNote, want: 250, every: 3, draw: (*App).drawPosture},
		{title: "Articolazioni · angoli", note: (*App).jointsNote, want: 175, every: 3, draw: (*App).drawJoints},
		{title: "Scheletro 3D · metri", note: (*App).worldNote, want: 270, every: 2, draw: (*App).drawWorld},
		{title: "Processo", note: (*App).procNote, want: 170, every: 10, draw: (*App).drawProcess},
		// colonna B
		{title: "Modello · MediaPipe Pose", note: (*App).modelNote, want: 265, every: 3, draw: (*App).drawModel},
		{title: "Calcolo", note: (*App).computeNote, want: 245, every: 3, draw: (*App).drawCompute},
		{title: "Webcam", note: (*App).camNote, want: 230, every: 6, draw: (*App).drawCamera},
		{title: "Controlli · V4L2", note: (*App).ctrlNote, want: 150, every: 15, draw: (*App).drawCtrls},
	}
	return a
}

func (a *App) px(v float64) int     { return int(math.Round(v * a.s)) }
func (a *App) pf(v float64) float64 { return v * a.s }

// Run esegue il loop della finestra finché l'utente non esce o ctx termina.
func (a *App) Run(ctx context.Context) error {
	if err := a.resize(a.W, a.H); err != nil {
		return err
	}
	tk := time.NewTicker(time.Second / 60)
	defer tk.Stop()
	for !a.quit {
		select {
		case <-ctx.Done():
			return nil
		case ev := <-a.win.Events:
			if err := a.event(ev); err != nil {
				return err
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

func (a *App) event(ev x11ui.Event) error {
	switch ev.Kind {
	case x11ui.Closed:
		a.quit = true
	case x11ui.Expose:
		a.full = true
	case x11ui.Resize:
		return a.resize(ev.W, ev.H)
	case x11ui.KeyDown:
		a.key(ev.Sym)
	case x11ui.MouseMove:
		a.mouse = image.Pt(ev.X, ev.Y)
		for _, g := range a.radios {
			if g.hover(a, a.mouse) {
				a.cards[0].dirty = true
			}
		}
	case x11ui.MouseDown:
		if ev.Button == 1 {
			for _, g := range a.radios {
				if g.click(a, image.Pt(ev.X, ev.Y)) {
					a.cards[0].dirty = true
				}
			}
		}
	}
	return nil
}

func (a *App) key(sym uint32) {
	cfg := a.Eng.Wanted()
	switch sym {
	case x11ui.KeyEscape, 'q':
		a.quit = true
	case 'b', x11ui.KeyTab:
		cfg.Mode = 1 - cfg.Mode
		a.request(cfg)
	case '1', '2', '3':
		if cfg.Mode == bodypose.FullBody {
			cfg.Variant = bodypose.Variants[sym-'1']
			a.request(cfg)
		}
	case 'g':
		cfg.GPU = !cfg.GPU
		a.request(cfg)
	case 'm':
		a.drawMode = (a.drawMode + 1) % drawModes
		a.pvDirty = true
		a.flash("Disegno: " + drawNames[a.drawMode])
	case 'v':
		a.mirror = !a.mirror
		a.full = true
		if a.mirror {
			a.flash("Anteprima a specchio")
		} else {
			a.flash("Anteprima come la vede la webcam")
		}
	case 'p', x11ui.KeySpace:
		a.paused = !a.paused
		if a.paused {
			a.flash("In pausa: i valori sono fermi")
		} else {
			a.flash("Ripresa")
		}
	case 'r':
		a.base = a.snap.C
		a.flash("Contatori azzerati")
	case 's':
		a.screenshot()
	case 'f', 0xffc8: // F, F11
		if a.win != nil {
			a.win.SetFullscreen(!a.win.Fullscreen())
		}
	}
}

// request chiede al motore una configurazione e lo segnala.
func (a *App) request(c Config) {
	if c.GPU && a.Eng.GPU == nil {
		a.flash("GPU non disponibile: " + a.Eng.GPUErr)
		c.GPU = false
	}
	if c.key() == a.Eng.Wanted().key() {
		return
	}
	a.Eng.Request(c)
	a.cards[0].dirty = true
	a.flash("Rilevamento: " + c.key().String())
}

func (a *App) flash(s string) {
	a.msg, a.msgUntil = s, time.Now().Add(4*time.Second)
}

func (a *App) post(fn func()) {
	select {
	case a.async <- fn:
	default:
	}
}

func (a *App) screenshot() {
	name := "webcam-body-" + time.Now().Format("20060102-150405.000") + ".png"
	path := filepath.Join(a.ShotDir, name)
	img := image.NewRGBA(a.canvas.Bounds())
	copy(img.Pix, a.canvas.Pix)
	go func() {
		err := savePNG(path, img)
		a.post(func() {
			if err != nil {
				a.flash("Schermata non salvata: " + err.Error())
				return
			}
			abs, _ := filepath.Abs(path)
			a.flash("Schermata salvata in " + abs)
		})
	}()
}

func savePNG(path string, img image.Image) error {
	fh, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := png.Encode(fh, img); err != nil {
		fh.Close()
		return err
	}
	return fh.Close()
}

// resize ricalcola scala, caratteri e impaginazione.
func (a *App) resize(w, h int) error {
	a.W, a.H = max(w, 1), max(h, 1)
	a.s = math.Max(0.45, math.Min(float64(a.W)/baseW, float64(a.H)/baseH))
	specs := []struct {
		dst  **x11ui.Font
		ttf  []byte
		size float64
	}{
		{&a.f.title, x11ui.TTFMedium, 19},
		{&a.f.head, x11ui.TTFMedium, 12},
		{&a.f.label, x11ui.TTFRegular, 12.5},
		{&a.f.small, x11ui.TTFRegular, 11},
		{&a.f.mono, x11ui.TTFMono, 12},
		{&a.f.monoS, x11ui.TTFMono, 11},
		{&a.f.big, x11ui.TTFMedium, 20},
	}
	for _, sp := range specs {
		var err error
		if *sp.dst, err = x11ui.NewFont(sp.ttf, sp.size*a.s); err != nil {
			return err
		}
	}
	a.canvas = image.NewRGBA(image.Rect(0, 0, a.W, a.H))
	a.layout()
	a.full = true
	return nil
}

func (a *App) headerRect() image.Rectangle { return image.Rect(0, 0, a.W, a.px(46)) }
func (a *App) footerRect() image.Rectangle { return image.Rect(0, a.H-a.px(30), a.W, a.H) }

// layout: a sinistra i comandi, l'anteprima (nel rapporto della webcam) e i
// grafici; a destra due colonne di riquadri, alti in proporzione a want.
func (a *App) layout() {
	m, g := a.px(14), a.px(12)
	body := image.Rect(m, a.headerRect().Max.Y, a.W-m, a.footerRect().Min.Y-a.px(2))
	leftW := int(float64(body.Dx()-g) * 0.53)
	left := image.Rect(body.Min.X, body.Min.Y, body.Min.X+leftW, body.Max.Y)
	right := image.Rect(left.Max.X+g, body.Min.Y, body.Max.X, body.Max.Y)

	ctlH := a.px(28+10+10) + a.px(30)
	a.cards[0].set(a, image.Rect(left.Min.X, left.Min.Y, left.Max.X, left.Min.Y+ctlH))
	top := left.Min.Y + ctlH + g
	asp := 9.0 / 16
	if in := a.Eng.Info; in.W > 0 && in.H > 0 {
		asp = float64(in.H) / float64(in.W)
	}
	chrome := a.px(28+10+10) + a.px(22) // titolo, margini, legenda
	pvH := int(float64(left.Dx()-a.px(24))*asp) + chrome
	pvH = min(pvH, left.Max.Y-top-g-a.px(170))
	a.cards[1].set(a, image.Rect(left.Min.X, top, left.Max.X, top+pvH))
	a.cards[2].set(a, image.Rect(left.Min.X, top+pvH+g, left.Max.X, left.Max.Y))

	colW := (right.Dx() - g) / 2
	a.stack(a.cards[3:7], image.Rect(right.Min.X, right.Min.Y, right.Min.X+colW, right.Max.Y), g)
	a.stack(a.cards[7:11], image.Rect(right.Min.X+colW+g, right.Min.Y, right.Max.X, right.Max.Y), g)
}

func (a *App) stack(cs []*card, r image.Rectangle, g int) {
	total := 0.0
	for _, c := range cs {
		total += c.want
	}
	avail := float64(r.Dy() - g*(len(cs)-1))
	y := r.Min.Y
	for i, c := range cs {
		h := int(avail * c.want / total)
		if i == len(cs)-1 {
			h = r.Max.Y - y
		}
		c.set(a, image.Rect(r.Min.X, y, r.Max.X, y+h))
		y += h + g
	}
}

// ---------------------------------------------------------------------------
// Disegno

func (a *App) frame(now time.Time) error {
	t0 := time.Now()
	dirty := a.paint(now)
	if len(dirty) == 0 || a.win == nil {
		return nil
	}
	t1 := time.Now()
	err := a.win.Present(a.canvas, dirty...)
	ema := func(v *float64, d time.Duration) {
		ms := float64(d.Microseconds()) / 1000
		if *v == 0 {
			*v = ms
		}
		*v += 0.05 * (ms - *v)
	}
	ema(&a.drawMs, t1.Sub(t0))
	ema(&a.presentMs, time.Since(t1))
	return err
}

// paint aggiorna il canvas e restituisce le regioni cambiate.
func (a *App) paint(now time.Time) []image.Rectangle {
	a.frameN++
	fresh := false
	if !a.paused {
		fresh = a.update(now)
	}
	full := a.full
	a.full = false
	if full {
		a.drawStatic(a.canvas)
	}
	var dirty []image.Rectangle
	for _, c := range a.cards {
		due := full || c.dirty
		if c.every == 0 {
			due = due || fresh || a.pvDirty
		} else {
			due = due || (!a.paused && a.frameN%c.every == 0)
		}
		if !due {
			continue
		}
		c.dirty = false
		x11ui.Fill(a.canvas, c.body, colPanel)
		c.draw(a, a.canvas, c.body)
		if c.note != nil {
			nr := c.noteRect()
			x11ui.Fill(a.canvas, nr, colPanel)
			x11ui.Text(a.canvas, a.f.small, nr.Max.X, nr.Max.Y-a.px(3), fit(a.f.small, c.note(a), nr.Dx()), colFaint, x11ui.Right)
			dirty = append(dirty, nr)
		}
		dirty = append(dirty, c.body)
	}
	a.pvDirty = false
	if full || a.frameN%15 == 0 {
		dirty = append(dirty, a.drawHeader(a.canvas), a.drawFooter(a.canvas, now))
	}
	if full {
		return []image.Rectangle{a.canvas.Bounds()}
	}
	return dirty
}

// update copia lo stato della pipeline; restituisce true se c'è un frame nuovo.
func (a *App) update(now time.Time) bool {
	fresh := a.Eng.Snapshot(&a.snap)
	a.now = now
	if fresh {
		a.pvFrames++
	}
	if now.Sub(a.rates.at) >= time.Second {
		a.rates.update(a.snap.C, a.pvFrames, now)
		a.sys.update(now)
	}
	return fresh
}

// drawStatic disegna sfondo, cornici e titoli dei riquadri.
func (a *App) drawStatic(img *image.RGBA) {
	x11ui.Fill(img, img.Bounds(), colBg)
	for _, c := range a.cards {
		if c.r.Dy() <= 0 {
			continue
		}
		x11ui.RoundRect(img, c.r, a.pf(8), colPanel)
		name, detail, _ := strings.Cut(c.title, " · ")
		t := x11ui.Text(img, a.f.head, c.head.Min.X, c.head.Max.Y-a.px(3), strings.ToUpper(name), colDim, x11ui.Left)
		if detail != "" {
			t = x11ui.Text(img, a.f.small, t.Max.X+a.px(6), c.head.Max.Y-a.px(3), detail, colFaint, x11ui.Left)
		}
		c.titleEnd = t.Max.X
		x11ui.Fill(img, image.Rect(c.r.Min.X+a.px(12), c.head.Max.Y+a.px(4), c.r.Max.X-a.px(12), c.head.Max.Y+a.px(5)), colLine)
	}
}

func (a *App) drawHeader(img *image.RGBA) image.Rectangle {
	r := a.headerRect()
	x11ui.Fill(img, r, colBg)
	base := r.Min.Y + a.px(31)
	x := a.px(16)
	t := x11ui.Text(img, a.f.title, x, base, "Scheletro dalla webcam", colText, x11ui.Left)
	x = t.Max.X + a.px(18)
	in := &a.Eng.Info
	parts := []string{in.Card, in.Path}
	if in.USB.ID != ":" && in.USB.ID != "" {
		parts = append(parts, "USB "+in.USB.ID)
	}
	parts = append(parts, "MediaPipe Pose · "+a.snap.Cur.String())

	var state string
	var sc color.RGBA
	switch {
	case a.snap.Err != "":
		state, sc = a.snap.Err, colBad
	case a.snap.Loading != "":
		state, sc = a.snap.Loading, colWarn
	case !a.snap.Have:
		state, sc = "in attesa del primo frame…", colWarn
	default:
		state, sc = "in acquisizione da "+fmtDuration(a.now.Sub(a.sys.Started)), colOK
	}
	if a.paused {
		state, sc = "IN PAUSA · "+state, colWarn
	}
	right := a.W - a.px(16)
	sr := x11ui.Text(img, a.f.label, right, base-a.px(2), fit(a.f.label, state, a.W/3), colText, x11ui.Right)
	x11ui.Circle(img, float64(sr.Min.X)-a.pf(10), float64(base)-a.pf(6.5), a.pf(5), sc)
	end := sr.Min.X - a.px(26)
	if a.rates.have && !a.paused {
		fr := x11ui.Text(img, a.f.mono, end, base-a.px(2), num(a.rates.FPS, 1)+" fps", colDim, x11ui.Right)
		end = fr.Min.X - a.px(18)
	}
	x11ui.Text(img, a.f.label, x, base-a.px(2), fit(a.f.label, strings.Join(parts, " · "), max(0, end-x)), colDim, x11ui.Left)
	return r
}

func (a *App) drawFooter(img *image.RGBA, now time.Time) image.Rectangle {
	r := a.footerRect()
	x11ui.Fill(img, r, colBg)
	base := r.Max.Y - a.px(10)
	x := a.px(16)
	mirror := "no"
	if a.mirror {
		mirror = "sì"
	}
	pause := "pausa"
	if a.paused {
		pause = "riprendi"
	}
	full := "schermo intero"
	if a.win != nil && a.win.Fullscreen() {
		full = "esci dallo schermo intero"
	}
	keys := []struct{ k, l string }{{"B", "corpo / busto"}, {"1 2 3", "lite, full, heavy"}, {"G", "GPU / CPU"},
		{"M", drawNames[a.drawMode]}, {"V", "specchio: " + mirror}, {"P", pause}, {"R", "azzera"}, {"S", "schermata"},
		{"F", full}, {"Esc", "esci"}}
	for _, k := range keys {
		kw := a.f.small.Measure(k.k) + a.px(10)
		kr := image.Rect(x, base-a.px(14), x+kw, base+a.px(4))
		x11ui.RoundRect(img, kr, a.pf(4), colKey)
		x11ui.Text(img, a.f.small, kr.Min.X+kw/2, base-a.px(1), k.k, colText, x11ui.Center)
		t := x11ui.Text(img, a.f.small, kr.Max.X+a.px(6), base-a.px(1), k.l, colDim, x11ui.Left)
		x = t.Max.X + a.px(14)
	}
	if a.msg != "" && now.Before(a.msgUntil) {
		x11ui.Text(img, a.f.label, a.W-a.px(16), base, fit(a.f.label, a.msg, max(0, a.W-a.px(16)-x)), colWarn, x11ui.Right)
	}
	return r
}

// ---------------------------------------------------------------------------
// Frequenze

type rates struct {
	at         time.Time
	prev       Counters
	prevPv     int
	have       bool
	FPS        float64 // frame dalla webcam
	ProcHz     float64
	DropHz     float64 // scartati perché il modello era occupato
	FoundPct   float64
	DetHz      float64 // esecuzioni del detector
	MBps       float64
	FrameKiB   float64
	DecodeUs   float64
	LatencyMs  float64 // driver → applicazione
	PipelineMs float64 // cattura → fine dell'elaborazione
	PreviewHz  float64
}

func (r *rates) update(c Counters, pv int, now time.Time) {
	if r.at.IsZero() {
		r.at, r.prev, r.prevPv = now, c, pv
		return
	}
	dt := now.Sub(r.at).Seconds()
	if dt <= 0 {
		return
	}
	p := r.prev
	frames := float64(c.Captured - p.Captured)
	proc := float64(c.Processed - p.Processed)
	r.FPS = frames / dt
	r.ProcHz = proc / dt
	r.DropHz = float64(c.BusyDrops-p.BusyDrops) / dt
	r.DetHz = float64(c.Detections-p.Detections) / dt
	r.MBps = float64(c.Bytes-p.Bytes) / dt / 1e6
	r.PreviewHz = float64(pv-r.prevPv) / dt
	if frames > 0 {
		r.FrameKiB = float64(c.Bytes-p.Bytes) / frames / 1024
		r.DecodeUs = float64(c.DecodeNs-p.DecodeNs) / frames / 1e3
		r.LatencyMs = float64(c.LatencyNs-p.LatencyNs) / frames / 1e6
	}
	if proc > 0 {
		r.FoundPct = 100 * float64(c.Found-p.Found) / proc
		r.PipelineMs = float64(c.PipelineNs-p.PipelineNs) / proc / 1e6
	}
	r.at, r.prev, r.prevPv, r.have = now, c, pv, true
}

// ---------------------------------------------------------------------------
// Testo

var nan = math.NaN()

func num(v float64, d int) string {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return "—"
	}
	s := strconv.FormatFloat(v, 'f', d, 64)
	if strings.Trim(s, "-0.") == "" {
		s = strings.TrimPrefix(s, "-")
	}
	return strings.Replace(s, ".", ",", 1)
}

func signed(v float64, d int) string {
	s := num(v, d)
	if !strings.HasPrefix(s, "-") && s != num(0, d) && s != "—" {
		s = "+" + s
	}
	return s
}

func itoa(v int) string { return strconv.Itoa(v) }

func fmtDuration(d time.Duration) string {
	d = d.Round(time.Second)
	h, m, s := int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%d:%02d", m, s)
}

func sizeText(n float64) string {
	switch {
	case n >= 1<<30:
		return num(n/(1<<30), 2) + " GiB"
	case n >= 1<<20:
		return num(n/(1<<20), 1) + " MiB"
	case n >= 1<<10:
		return num(n/(1<<10), 0) + " KiB"
	}
	return num(n, 0) + " B"
}

// fit accorcia s con "…" perché stia in w pixel.
func fit(f *x11ui.Font, s string, w int) string {
	if f.Measure(s) <= w {
		return s
	}
	r := []rune(s)
	for len(r) > 0 && f.Measure(string(r)+"…") > w {
		r = r[:len(r)-1]
	}
	if len(r) == 0 {
		return ""
	}
	return string(r) + "…"
}
