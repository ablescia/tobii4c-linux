package telemetry

import (
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/ablescia/tobii4c-linux/internal/ttp"
	"github.com/ablescia/tobii4c-linux/internal/x11ui"
)

// Palette: quella dell'app di calibrazione (sfondo grigio a luminanza media,
// che non altera la pupilla), più i colori dei due occhi.
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
	colLeft   = x11ui.Hex(0x6cb6ff) // occhio sinistro dell'utente
	colRight  = x11ui.Hex(0xffa75e) // occhio destro
	colMesh   = x11ui.Hex(0x5ae678)
	colCrop   = x11ui.Hex(0xf1c56b)
	colSensor = x11ui.Hex(0x4fe0e0)
	colEyeCol = [2]color.RGBA{colLeft, colRight}
)

// Dimensioni logiche di riferimento: l'impaginazione è pensata per questa
// superficie e scala con la finestra (a 1920×1080 la scala vale circa 1).
const (
	baseW = 1880.0
	baseH = 1000.0
)

// Modalità del disegno del viso (tasto M).
const (
	meshPoints = iota
	meshContours
	meshTesselation
	meshNone
	meshModes
)

var meshNames = [meshModes]string{"punti", "punti e contorni", "punti e triangolazione", "solo immagine"}

// Prompt è una domanda da risolvere prima di avviare il tracker (es. fermare
// il servizio systemd che occupa il dispositivo).
type Prompt struct {
	Title   string
	Lines   []string
	Confirm string       // azione del tasto Invio
	Do      func() error // eseguita fuori dal loop alla conferma
}

// App è la finestra di telemetria.
type App struct {
	Col     *Collector
	Log     *slog.Logger
	ShotDir string        // cartella delle schermate (tasto S)
	Start   func()        // avvia il tracker; chiamata subito o dopo il Prompt
	Prompt  *Prompt       // domanda iniziale, se serve
	Title   string        // es. "simulazione"
	Every   time.Duration // periodo di disegno (default 1/30 s)
	// AutoShot: se > 0, salva una schermata a questo intervallo (per
	// controllare l'impaginazione a distanza).
	AutoShot time.Duration

	win    *x11ui.Window
	W, H   int
	s      float64
	f      fonts
	canvas *image.RGBA
	cards  []*card
	full   bool
	frameN int

	snap     Snapshot
	rows     []colRow
	rowsSrc  []ttp.Record
	rates    rates
	base     counters // azzeramento dei contatori (tasto R)
	pv       preview
	meshMode int
	paused   bool
	msg      string
	msgUntil time.Time
	quit     bool
	async    chan func()
	ctx      context.Context

	promptBusy bool
	promptErr  string
	started    bool
	aspect     float64
	fwNames    bool // tabella delle colonne con i nomi del firmware (tasto N)
	lastShot   time.Time
}

type fonts struct {
	title, head, label, small, mono, monoS, big *x11ui.Font
}

// card è una scheda: titolo e sfondo si disegnano solo nei ridisegni
// completi (drawStatic), il corpo ogni every frame.
type card struct {
	title string
	note  func(a *App) string // testo a destra del titolo (aggiornato col corpo)
	r     image.Rectangle
	body  image.Rectangle
	head  image.Rectangle // riga del titolo
	every int
	draw  func(a *App, img *image.RGBA, b image.Rectangle)

	titleEnd int // x dopo il titolo (da drawStatic)
}

// noteRect è lo spazio della nota, a destra del titolo.
func (c *card) noteRect() image.Rectangle {
	r := c.head
	r.Min.X = max(r.Min.X, c.titleEnd+8)
	if r.Min.X > r.Max.X {
		r.Min.X = r.Max.X
	}
	return r
}

// NewApp prepara l'applicazione sulla finestra win.
func NewApp(win *x11ui.Window, col *Collector, log *slog.Logger) *App {
	return &App{win: win, Col: col, Log: log, W: win.W, H: win.H, meshMode: meshContours,
		async: make(chan func(), 16), ShotDir: "."}
}

// px converte una misura logica in pixel.
func (a *App) px(v float64) int { return int(math.Round(v * a.s)) }

// pf come px, in virgola mobile.
func (a *App) pf(v float64) float64 { return v * a.s }

// Run esegue il loop finché l'utente non chiude la finestra o ctx termina.
func (a *App) Run(ctx context.Context) error {
	var cancel context.CancelFunc
	a.ctx, cancel = context.WithCancel(ctx)
	defer cancel()
	if a.Every <= 0 {
		a.Every = time.Second / 30
	}
	if err := a.resize(a.W, a.H); err != nil {
		return err
	}
	if a.Prompt == nil {
		a.start()
	}
	tk := time.NewTicker(a.Every)
	defer tk.Stop()
	for !a.quit {
		select {
		case <-a.ctx.Done():
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

// Started dice se il tracker è stato avviato (dopo l'eventuale Prompt).
func (a *App) Started() bool { return a.started }

func (a *App) start() {
	if a.started {
		return
	}
	a.started = true
	a.Prompt = nil
	a.full = true
	if a.Start != nil {
		a.Start()
	}
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
		a.Log.Debug("key", "keysym", fmt.Sprintf("0x%x", ev.Sym))
		a.key(ev)
	}
	return nil
}

func (a *App) key(ev x11ui.Event) {
	if a.Prompt != nil {
		switch ev.Sym {
		case x11ui.KeyEscape, 'q':
			a.quit = true
		case x11ui.KeyReturn, x11ui.KeyKPEnter:
			if a.promptBusy {
				return
			}
			a.promptBusy, a.promptErr = true, ""
			a.full = true
			p := a.Prompt
			go func() {
				err := p.Do()
				a.post(func() {
					a.promptBusy = false
					if err != nil {
						a.promptErr = err.Error()
						a.full = true
						return
					}
					a.start()
				})
			}()
		}
		return
	}
	switch ev.Sym {
	case x11ui.KeyEscape, 'q':
		a.quit = true
	case 'm':
		a.meshMode = (a.meshMode + 1) % meshModes
		a.pv.dirty = true
		a.flash("Viso: " + meshNames[a.meshMode])
	case 'p', x11ui.KeySpace:
		a.paused = !a.paused
		if a.paused {
			a.flash("In pausa: i valori sono fermi")
		} else {
			a.flash("Ripresa")
		}
	case 'r':
		a.base = a.counters()
		a.flash("Contatori azzerati")
	case 'n':
		a.fwNames = !a.fwNames
		if a.fwNames {
			a.flash("Colonne: nomi del firmware")
		} else {
			a.flash("Colonne: descrizioni in italiano")
		}
	case 's':
		a.screenshot()
	case 'f', 0xffc8: // F, F11
		if a.win != nil {
			a.win.SetFullscreen(!a.win.Fullscreen())
		}
	}
}

func (a *App) post(fn func()) {
	select {
	case a.async <- fn:
	case <-a.ctx.Done():
	}
}

func (a *App) flash(s string) {
	a.msg, a.msgUntil = s, time.Now().Add(4*time.Second)
}

func (a *App) screenshot() {
	name := "telemetria-" + time.Now().Format("20060102-150405.000") + ".png"
	path := filepath.Join(a.ShotDir, name)
	img := image.NewRGBA(a.canvas.Bounds())
	copy(img.Pix, a.canvas.Pix)
	go func() {
		err := func() error {
			fh, err := os.Create(path)
			if err != nil {
				return err
			}
			if err := png.Encode(fh, img); err != nil {
				fh.Close()
				return err
			}
			return fh.Close()
		}()
		a.post(func() {
			if err != nil {
				a.flash("Schermata non salvata: " + err.Error())
				return
			}
			abs, _ := filepath.Abs(path)
			a.flash("Schermata salvata in " + abs)
			a.Log.Info("screenshot saved", "path", abs)
		})
	}()
}

// resize ricalcola scala, caratteri e impaginazione.
func (a *App) resize(w, h int) error {
	a.W, a.H = max(w, 1), max(h, 1)
	a.s = math.Max(0.45, math.Min(float64(a.W)/baseW, float64(a.H)/baseH))
	var err error
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
		if *sp.dst, err = x11ui.NewFont(sp.ttf, sp.size*a.s); err != nil {
			return err
		}
	}
	a.canvas = image.NewRGBA(image.Rect(0, 0, a.W, a.H))
	a.layout()
	a.pv.resized = true
	a.full = true
	return nil
}

// ---------------------------------------------------------------------------
// Disegno

func (a *App) frame(now time.Time) error {
	dirty := a.paint(now)
	if a.AutoShot > 0 && now.Sub(a.lastShot) >= a.AutoShot && a.frameN > 15 {
		a.lastShot = now
		a.screenshot()
	}
	if len(dirty) == 0 || a.win == nil {
		return nil
	}
	return a.win.Present(a.canvas, dirty...)
}

// paint aggiorna il canvas e restituisce le regioni cambiate.
func (a *App) paint(now time.Time) []image.Rectangle {
	a.frameN++
	if !a.paused || a.full {
		a.update(now)
	}
	full := a.full
	a.full = false
	var dirty []image.Rectangle
	if full {
		a.drawStatic(a.canvas)
		dirty = append(dirty, a.canvas.Bounds())
	}
	if a.Prompt != nil {
		if full {
			a.drawPrompt(a.canvas)
		}
		return dirty
	}
	if !a.paused || full {
		for _, c := range a.cards {
			if full || a.frameN%c.every == 0 {
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
		}
	}
	// intestazione e piè di pagina: ogni 10 frame o se cambia il messaggio
	if full || a.frameN%10 == 0 {
		dirty = append(dirty, a.drawHeader(a.canvas), a.drawFooter(a.canvas, now))
	}
	if full {
		dirty = []image.Rectangle{a.canvas.Bounds()}
	}
	return dirty
}

// update copia lo stato del collettore e ricalcola ciò che ne dipende.
func (a *App) update(now time.Time) {
	a.Col.Snapshot(&a.snap)
	if cols := a.snap.Session.Columns[ttp.StreamGaze]; !sameRecords(cols, a.rowsSrc) || a.rows == nil {
		a.rowsSrc = cols
		a.rows = buildColumns(cols, a.Col.KeyMap)
		a.relayout()
	}
	if asp := a.displayAspect(); asp != a.aspect {
		a.aspect = asp
		a.relayout()
	}
	if now.Sub(a.rates.at) >= time.Second {
		a.rates.update(a, now)
	}
	if hp := a.Col.HP; hp != nil {
		if hp.LatestPreview(&a.pv.p) {
			a.pv.dirty, a.pv.have = true, true
			a.pv.lastNew = now
		}
		a.pv.res = hp.Last()
	}
}

// drawStatic disegna sfondo, titoli e cornici delle schede.
func (a *App) drawStatic(img *image.RGBA) {
	x11ui.Fill(img, img.Bounds(), colBg)
	for _, c := range a.cards {
		x11ui.RoundRect(img, c.r, a.pf(8), colPanel)
		// "Nome · dettaglio": il nome in maiuscolo, il dettaglio così com'è
		name, detail, _ := strings.Cut(c.title, " · ")
		t := x11ui.Text(img, a.f.head, c.head.Min.X, c.head.Max.Y-a.px(3), strings.ToUpper(name), colDim, x11ui.Left)
		if detail != "" {
			t = x11ui.Text(img, a.f.small, t.Max.X+a.px(6), c.head.Max.Y-a.px(3), detail, colFaint, x11ui.Left)
		}
		c.titleEnd = t.Max.X
		x11ui.Fill(img, image.Rect(c.r.Min.X+a.px(12), c.head.Max.Y+a.px(4), c.r.Max.X-a.px(12), c.head.Max.Y+a.px(5)), colLine)
	}
	a.drawHeader(img)
	a.drawFooter(img, time.Now())
}

func (a *App) headerRect() image.Rectangle { return image.Rect(0, 0, a.W, a.px(46)) }
func (a *App) footerRect() image.Rectangle { return image.Rect(0, a.H-a.px(30), a.W, a.H) }

func (a *App) drawHeader(img *image.RGBA) image.Rectangle {
	r := a.headerRect()
	x11ui.Fill(img, r, colBg)
	base := r.Min.Y + a.px(31)
	x := a.px(16)
	t := x11ui.Text(img, a.f.title, x, base, "Telemetria del Tobii 4C", colText, x11ui.Left)
	x = t.Max.X + a.px(18)
	si := &a.snap.Session
	var parts []string
	if a.Title != "" {
		parts = append(parts, a.Title)
	}
	if len(si.Strings) >= 4 {
		parts = append(parts, si.Strings[1], si.Strings[0], "firmware "+si.Strings[3])
	} else if si.USB.Serial != "" {
		parts = append(parts, si.USB.Serial)
	}
	right := a.W - a.px(16)
	// stato della connessione
	var state string
	var sc color.RGBA
	switch {
	case a.Prompt != nil:
		state, sc = "in attesa di conferma", colWarn
	case si.Connected:
		state, sc = "connesso da "+fmtDuration(time.Since(si.Since)), colOK
		if a.snap.HaveSample && time.Since(time.Unix(0, a.snap.Sample.HostTimeNs)) > 2*time.Second {
			state, sc = "connesso, nessun dato da "+fmtDuration(time.Since(time.Unix(0, a.snap.Sample.HostTimeNs))), colWarn
		}
	case si.Sessions > 0:
		state, sc = "disconnesso da "+fmtDuration(time.Since(si.ClosedAt))+": riconnessione…", colBad
	default:
		state, sc = "connessione al tracker…", colWarn
	}
	if a.paused {
		state, sc = "IN PAUSA · "+state, colWarn
	}
	sr := x11ui.Text(img, a.f.label, right, base-a.px(2), state, colText, x11ui.Right)
	x11ui.Circle(img, float64(sr.Min.X)-a.pf(10), float64(base)-a.pf(6.5), a.pf(5), sc)
	if hz := a.rates.gazeHz; hz > 0 && !a.paused {
		x11ui.Text(img, a.f.mono, sr.Min.X-a.px(26), base-a.px(2), num(hz, 1)+" Hz", colDim, x11ui.Right)
	}
	x11ui.Text(img, a.f.label, x, base-a.px(2), fit(a.f.label, strings.Join(parts, " · "), max(0, sr.Min.X-a.px(130)-x)), colDim, x11ui.Left)
	return r
}

func (a *App) drawFooter(img *image.RGBA, now time.Time) image.Rectangle {
	r := a.footerRect()
	x11ui.Fill(img, r, colBg)
	base := r.Max.Y - a.px(10)
	x := a.px(16)
	names := "nomi del firmware"
	if a.fwNames {
		names = "descrizioni"
	}
	pause := "pausa"
	if a.paused {
		pause = "riprendi"
	}
	full := "schermo intero"
	if a.win != nil && a.win.Fullscreen() {
		full = "esci dallo schermo intero"
	}
	keys := []struct{ k, l string }{{"M", "viso: " + meshNames[a.meshMode]}, {"N", "colonne: " + names}, {"P", pause},
		{"R", "azzera i contatori"}, {"S", "salva una schermata"}, {"F", full}, {"Esc", "esci"}}
	if a.Prompt != nil {
		keys = keys[len(keys)-1:]
	}
	for _, k := range keys {
		kw := a.f.head.Measure(k.k) + a.px(12)
		cap := image.Rect(x, base-a.f.head.Ascent-a.px(4), x+kw, base+a.f.head.Desc+a.px(3))
		x11ui.RoundRect(img, cap, a.pf(4), colKey)
		x11ui.Text(img, a.f.head, x+a.px(6), base, k.k, colText, x11ui.Left)
		t := x11ui.Text(img, a.f.small, cap.Max.X+a.px(7), base, k.l, colDim, x11ui.Left)
		x = t.Max.X + a.px(20)
	}
	if a.msg != "" && now.Before(a.msgUntil) {
		x11ui.Text(img, a.f.label, a.W-a.px(16), base, fit(a.f.label, a.msg, a.W-x-a.px(32)), colWarn, x11ui.Right)
	} else {
		a.msg = ""
		if a.s < 0.75 && (a.win == nil || !a.win.Fullscreen()) {
			x11ui.Text(img, a.f.label, a.W-a.px(16), base, fit(a.f.label, "Finestra piccola: F per lo schermo intero", a.W-x-a.px(32)), colWarn, x11ui.Right)
		}
	}
	return r
}

func (a *App) drawPrompt(img *image.RGBA) {
	// velo sopra il cruscotto vuoto
	for i := 3; i < len(img.Pix); i += 4 {
		img.Pix[i-3] = uint8(int(img.Pix[i-3]) * 2 / 3)
		img.Pix[i-2] = uint8(int(img.Pix[i-2]) * 2 / 3)
		img.Pix[i-1] = uint8(int(img.Pix[i-1]) * 2 / 3)
	}
	p := a.Prompt
	w := a.px(720)
	lh := a.px(24)
	lines := 0
	var wrapped [][]string
	for _, l := range p.Lines {
		ws := x11ui.Wrap(a.f.label, l, w-a.px(64))
		wrapped = append(wrapped, ws)
		lines += len(ws)
	}
	h := a.px(150) + lines*lh + len(p.Lines)*a.px(8)
	if a.promptErr != "" {
		h += a.px(56)
	}
	r := image.Rect((a.W-w)/2, (a.H-h)/2, (a.W+w)/2, (a.H+h)/2)
	x11ui.RoundRect(img, r, a.pf(12), colPanel)
	x, y := r.Min.X+a.px(32), r.Min.Y+a.px(50)
	x11ui.Text(img, a.f.big, x, y, p.Title, colText, x11ui.Left)
	y += a.px(38)
	for _, ws := range wrapped {
		for _, l := range ws {
			x11ui.Text(img, a.f.label, x, y, l, colDim, x11ui.Left)
			y += lh
		}
		y += a.px(8)
	}
	if a.promptErr != "" {
		for _, l := range x11ui.Wrap(a.f.label, "Errore: "+a.promptErr, w-a.px(64))[:1] {
			x11ui.Text(img, a.f.label, x, y+a.px(8), l, colBad, x11ui.Left)
		}
		y += a.px(56)
	}
	y = r.Max.Y - a.px(30)
	confirm := p.Confirm
	if a.promptBusy {
		confirm = "attendere…"
	}
	for _, k := range []struct{ k, l string }{{"Invio", confirm}, {"Esc", "esci"}} {
		kw := a.f.head.Measure(k.k) + a.px(16)
		cap := image.Rect(x, y-a.f.head.Ascent-a.px(6), x+kw, y+a.f.head.Desc+a.px(5))
		x11ui.RoundRect(img, cap, a.pf(5), colKey)
		x11ui.Text(img, a.f.head, x+a.px(8), y, k.k, colText, x11ui.Left)
		t := x11ui.Text(img, a.f.label, cap.Max.X+a.px(10), y, k.l, colText, x11ui.Left)
		x = t.Max.X + a.px(32)
	}
}

// ---------------------------------------------------------------------------
// Impaginazione

// layout dispone le schede: a sinistra la camera con viso e testa, a destra
// tre colonne con sguardo e occhi, colonne del firmware e stream, dispositivo,
// comunicazione e registro.
func (a *App) layout() {
	// La superficie logica cresce al più del 15% in larghezza e del 20% in
	// altezza rispetto al progetto: oltre, il cruscotto si centra invece di
	// stirarsi (finestre affiancate, strette e alte).
	lw, lh := float64(a.W)/a.s, float64(a.H)/a.s
	ox, oy := 0.0, 0.0 // margine per centrare il cruscotto
	if maxW := baseW * 1.15; lw > maxW {
		ox, lw = (lw-maxW)/2, maxW
	}
	if maxH := baseH * 1.2; lh > maxH {
		lh = maxH // finestra stretta e alta: il cruscotto resta in alto
	}
	const m, g, foot = 14.0, 12.0, 34.0
	top := 50.0
	bodyH := lh - top - foot
	a.cards = a.cards[:0]
	add := func(title string, x, y, w, h float64, every int, draw func(*App, *image.RGBA, image.Rectangle), note func(*App) string) {
		x, y = x+ox, y+oy
		r := image.Rect(a.px(x), a.px(y), a.px(x+w), a.px(y+h))
		head := image.Rect(r.Min.X+a.px(12), r.Min.Y+a.px(8), r.Max.X-a.px(12), r.Min.Y+a.px(26))
		body := image.Rect(r.Min.X+a.px(12), r.Min.Y+a.px(34), r.Max.X-a.px(12), r.Max.Y-a.px(10))
		// finestra troppo piccola: la scheda che non ci sta non si disegna
		if w <= 60 || h <= 60 || !body.In(a.canvas.Bounds()) {
			return
		}
		a.cards = append(a.cards, &card{title: title, r: r, head: head, body: body, every: every, draw: draw, note: note})
	}

	// colonna sinistra: camera (4:3), sotto testa e modello del viso
	leftW := 640.0 + math.Max(0, lw-baseW)*0.4
	camH := (leftW-24)*0.75 + 34 + 10 + 24
	if maxCam := bodyH - 330; camH > maxCam { // lascia spazio alle schede sotto
		camH = maxCam
		leftW = (camH-68)/0.75 + 24
	}
	add("Camera IR · stream 0x501", m, top, leftW, camH, 1, (*App).drawCamera, (*App).cameraNote)
	y2 := top + camH + g
	hw := (leftW - g) / 2
	add("Testa", m, y2, hw, bodyH-camH-g, 3, (*App).drawHead, (*App).headNote)
	add("Modello del viso", m+hw+g, y2, hw, bodyH-camH-g, 3, (*App).drawFaceModel, nil)

	// destra: tre colonne
	x0 := m + leftW + g
	cw := (lw - m - x0 - 2*g) / 3
	xa, xb, xc := x0, x0+cw+g, x0+2*(cw+g)

	// colonna A: sguardo, occhi nel track box, pupilla e palpebre, presenza
	aspect := a.displayAspect()
	gazeH := 44 + (cw-24)/aspect + 50
	add("Sguardo sullo schermo", xa, top, cw, gazeH, 1, (*App).drawGaze, nil)
	boxH := 44 + (cw-24-g)/2*0.85 + 34
	add("Occhi nel track box", xa, top+gazeH+g, cw, boxH, 1, (*App).drawTrackBox, nil)
	plotY := top + gazeH + boxH + 2*g
	presH := 104.0
	add("Pupilla e palpebre · ultimi 10 s", xa, plotY, cw, bodyH-(plotY-top)-presH-g, 1, (*App).drawPlots, nil)
	add("Presenza · stream 0x504", xa, top+bodyH-presH, cw, presH, 3, (*App).drawPresence, nil)

	// colonna B: colonne del firmware, stream
	nrows := max(len(a.rows), 16)
	colH := 44 + 22 + float64(nrows)*30 + 6
	add("Stream gaze · 0x500, colonne del firmware", xb, top, cw, colH, 3, (*App).drawColumns, (*App).columnsNote)
	add("Stream del dispositivo", xb, top+colH+g, cw, bodyH-colH-g, 3, (*App).drawStreams, nil)

	// colonna C: dispositivo, comunicazione, registro
	devH := 44 + 19*17.5 + 8
	add("Dispositivo", xc, top, cw, devH, 15, (*App).drawDevice, nil)
	pipeH := 44 + 12*17.5 + 6
	add("Comunicazione e pipeline", xc, top+devH+g, cw, pipeH, 3, (*App).drawPipeline, nil)
	add("Registro · firmware e driver", xc, top+devH+pipeH+2*g, cw, bodyH-devH-pipeH-2*g, 3, (*App).drawLog, (*App).logNote)
}

// relayout ricalcola l'impaginazione quando cambiano i dati che la
// determinano (numero di colonne del firmware, proporzioni dello schermo).
func (a *App) relayout() {
	if a.canvas == nil {
		return
	}
	a.layout()
	a.full = true
}

func (a *App) displayAspect() float64 {
	if d := a.snap.Session.Display; d != nil && d.Height() > 0 {
		return math.Round(100*math.Max(1, math.Min(2.5, d.Width()/d.Height()))) / 100
	}
	return 16.0 / 9
}

func sameRecords(x, y []ttp.Record) bool {
	return slices.EqualFunc(x, y, func(a, b ttp.Record) bool { return a.ID == b.ID && a.Name == b.Name })
}

// ---------------------------------------------------------------------------
// Utilità

// wrap spezza s in righe larghe al più w pixel; le parole più lunghe di
// una riga (percorsi, valori senza spazi) si spezzano a metà.
func wrap(f *x11ui.Font, s string, w int) []string {
	var out []string
	for _, l := range x11ui.Wrap(f, s, w) {
		for f.Measure(l) > w && w > 0 {
			r := []rune(l)
			n := len(r)
			for n > 1 && f.Measure(string(r[:n])) > w {
				n--
			}
			out = append(out, string(r[:n]))
			l = string(r[n:])
		}
		out = append(out, l)
	}
	return out
}

// fit accorcia s con "…" finché sta in w pixel.
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

func fmtDuration(d time.Duration) string {
	d = d.Round(time.Second)
	h, m, s := int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%d:%02d", m, s)
}

func deg(r float64) float64 { return r * 180 / math.Pi }

// signed formatta un angolo con il segno.
func signed(v float64, d int) string {
	s := num(v, d)
	if !strings.HasPrefix(s, "-") && s != num(0, d) {
		s = "+" + s
	}
	return s
}
