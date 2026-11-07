// Package x11ui è un livello minimo per una finestra a schermo intero su X11,
// scritto in Go puro sopra il protocollo (github.com/jezek/xgb): niente cgo,
// niente toolkit. Copre esattamente ciò che serve alla calibrazione: una
// finestra che copre un monitor preciso (RandR), tastiera presa in esclusiva,
// mouse, e upload di regioni di un'immagine RGBA disegnata in software.
//
// Funziona su X11 e, tramite XWayland, nelle sessioni Wayland.
package x11ui

import (
	"errors"
	"fmt"
	"image"
	"os"
	"time"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/randr"
	"github.com/jezek/xgb/xproto"
)

// Monitor è un'uscita video attiva.
type Monitor struct {
	Name       string
	X, Y, W, H int
	WidthMM    float64 // dimensioni fisiche (EDID se disponibile)
	HeightMM   float64
	Model      string // nome commerciale dall'EDID
	Primary    bool
	EDIDSize   bool // WidthMM/HeightMM vengono dal detailed timing dell'EDID
}

// EventKind classifica gli eventi.
type EventKind int

const (
	KeyDown EventKind = iota + 1
	MouseDown
	MouseUp
	MouseMove
	Expose
	Closed
	Resize // la finestra ha cambiato dimensione: nuova misura in W, H
)

// Keysym X11 usati dall'interfaccia.
const (
	KeyEscape    = 0xff1b
	KeyReturn    = 0xff0d
	KeyKPEnter   = 0xff8d
	KeySpace     = 0x0020
	KeyBackSpace = 0xff08
	KeyTab       = 0xff09
	KeyLeft      = 0xff51
	KeyUp        = 0xff52
	KeyRight     = 0xff53
	KeyDown_     = 0xff54
)

// Event è un evento d'ingresso tradotto.
type Event struct {
	Kind             EventKind
	Sym              uint32 // keysym (minuscolo per le lettere)
	Shift, Ctrl, Alt bool
	X, Y             int // coordinate nella finestra
	Button           int
	W, H             int // Resize: nuova dimensione
}

// Window è una finestra override-redirect che copre un monitor.
type Window struct {
	W, H    int
	Monitor Monitor
	Events  chan Event

	conn   *xgb.Conn
	win    xproto.Window
	gc     xproto.Gcontext
	depth  byte
	maxReq int
	blank  xproto.Cursor
	minKC  xproto.Keycode
	perKC  int
	syms   []xproto.Keysym
	buf    []byte
	closed chan struct{}

	managed     bool // finestra normale (OpenWindow), non a schermo intero
	root        xproto.Window
	fullscreen  bool
	wmProtocols xproto.Atom // WM_PROTOCOLS
	wmDelete    xproto.Atom // WM_DELETE_WINDOW: chiusura dal window manager
}

// Connect apre la connessione al server X ($DISPLAY).
func Connect() (*xgb.Conn, error) {
	c, err := xgb.NewConn()
	if err != nil {
		return nil, fmt.Errorf("connessione a X11 ($DISPLAY): %w", err)
	}
	return c, nil
}

// Monitors elenca le uscite attive via RandR, con le misure dell'EDID.
func Monitors(c *xgb.Conn) ([]Monitor, error) {
	scr := xproto.Setup(c).DefaultScreen(c)
	full := []Monitor{{Name: "screen", W: int(scr.WidthInPixels), H: int(scr.HeightInPixels),
		WidthMM: float64(scr.WidthInMillimeters), HeightMM: float64(scr.HeightInMillimeters), Primary: true}}
	if err := randr.Init(c); err != nil {
		return full, nil
	}
	res, err := randr.GetScreenResourcesCurrent(c, scr.Root).Reply()
	if err != nil {
		return full, nil
	}
	var primary randr.Output
	if p, err := randr.GetOutputPrimary(c, scr.Root).Reply(); err == nil {
		primary = p.Output
	}
	edidAtom, _ := xproto.InternAtom(c, true, 4, "EDID").Reply()
	var out []Monitor
	for _, o := range res.Outputs {
		oi, err := randr.GetOutputInfo(c, o, res.ConfigTimestamp).Reply()
		if err != nil || oi.Connection != randr.ConnectionConnected || oi.Crtc == 0 {
			continue
		}
		ci, err := randr.GetCrtcInfo(c, oi.Crtc, res.ConfigTimestamp).Reply()
		if err != nil || ci.Width == 0 {
			continue
		}
		m := Monitor{Name: string(oi.Name), X: int(ci.X), Y: int(ci.Y), W: int(ci.Width), H: int(ci.Height),
			WidthMM: float64(oi.MmWidth), HeightMM: float64(oi.MmHeight), Primary: o == primary}
		if edidAtom != nil && edidAtom.Atom != 0 {
			if p, err := randr.GetOutputProperty(c, o, edidAtom.Atom, xproto.AtomAny, 0, 256, false, false).Reply(); err == nil {
				if e, ok := ParseEDID(p.Data); ok {
					m.Model = e.Model
					if e.WidthMM > 0 && e.HeightMM > 0 {
						m.WidthMM, m.HeightMM, m.EDIDSize = e.WidthMM, e.HeightMM, true
					}
				}
			}
		}
		// Un monitor ruotato riporta comunque le misure orizzontali.
		if (m.W > m.H) != (m.WidthMM > m.HeightMM) {
			m.WidthMM, m.HeightMM = m.HeightMM, m.WidthMM
		}
		out = append(out, m)
	}
	if len(out) == 0 {
		return full, nil
	}
	return out, nil
}

// checkScreen verifica che lo schermo predefinito sia TrueColor a 32 bpp
// little-endian, l'unico formato che Present sa caricare.
func checkScreen(c *xgb.Conn) (*xproto.SetupInfo, *xproto.ScreenInfo, error) {
	setup := xproto.Setup(c)
	scr := setup.DefaultScreen(c)
	if setup.ImageByteOrder != xproto.ImageOrderLSBFirst {
		return nil, nil, errors.New("x11: byte order MSB non supportato")
	}
	bpp := 0
	for _, f := range setup.PixmapFormats {
		if f.Depth == scr.RootDepth {
			bpp = int(f.BitsPerPixel)
		}
	}
	if scr.RootDepth != 24 && scr.RootDepth != 32 || bpp != 32 {
		return nil, nil, fmt.Errorf("x11: profondità %d/%d bpp non supportata (serve TrueColor 24/32)", scr.RootDepth, bpp)
	}
	return setup, scr, nil
}

// loadKeymap legge la tabella keycode → keysym.
func (w *Window) loadKeymap(setup *xproto.SetupInfo) error {
	km, err := xproto.GetKeyboardMapping(w.conn, setup.MinKeycode, byte(setup.MaxKeycode-setup.MinKeycode+1)).Reply()
	if err != nil {
		return fmt.Errorf("x11: keyboard mapping: %w", err)
	}
	w.minKC, w.perKC, w.syms = setup.MinKeycode, int(km.KeysymsPerKeycode), km.Keysyms
	return nil
}

func setString(c *xgb.Conn, win xproto.Window, prop, typ xproto.Atom, v string) {
	xproto.ChangeProperty(c, xproto.PropModeReplace, win, prop, typ, 8, uint32(len(v)), []byte(v))
}

func atom(c *xgb.Conn, name string) xproto.Atom {
	r, err := xproto.InternAtom(c, false, uint16(len(name)), name).Reply()
	if err != nil || r == nil {
		return 0
	}
	return r.Atom
}

// WindowOptions configura una finestra normale (OpenWindow).
type WindowOptions struct {
	Title      string
	Class      string // WM_CLASS: permette regole del window manager (es. windowrule di Hyprland)
	X, Y       int    // posizione iniziale (il window manager può ignorarla)
	W, H       int    // dimensione iniziale
	MinW, MinH int    // dimensione minima suggerita al window manager
	Background uint32 // 0xRRGGBB
	Maximized  bool   // chiede di aprirla massimizzata (_NET_WM_STATE)
	Fullscreen bool   // chiede di aprirla a schermo intero
}

// OpenWindow crea una finestra normale, gestita dal window manager: ha un
// titolo, si sposta e si ridimensiona (eventi Resize), si chiude dal window
// manager (evento Closed) e riceve la tastiera solo quando ha il fuoco.
func OpenWindow(c *xgb.Conn, o WindowOptions) (*Window, error) {
	setup, scr, err := checkScreen(c)
	if err != nil {
		return nil, err
	}
	o.W, o.H = max(o.W, 1), max(o.H, 1)
	w := &Window{W: o.W, H: o.H, Monitor: Monitor{Name: "window", W: o.W, H: o.H}, conn: c, depth: scr.RootDepth,
		maxReq: int(setup.MaximumRequestLength) * 4, Events: make(chan Event, 256), closed: make(chan struct{}),
		managed: true}
	if w.win, err = xproto.NewWindowId(c); err != nil {
		return nil, err
	}
	events := uint32(xproto.EventMaskKeyPress | xproto.EventMaskButtonPress | xproto.EventMaskButtonRelease |
		xproto.EventMaskPointerMotion | xproto.EventMaskExposure | xproto.EventMaskStructureNotify)
	if err := xproto.CreateWindowChecked(c, scr.RootDepth, w.win, scr.Root, int16(o.X), int16(o.Y), uint16(o.W), uint16(o.H), 0,
		xproto.WindowClassInputOutput, scr.RootVisual, xproto.CwBackPixel|xproto.CwEventMask,
		[]uint32{o.Background, events}).Check(); err != nil {
		return nil, fmt.Errorf("x11: create window: %w", err)
	}
	setString(c, w.win, xproto.AtomWmName, xproto.AtomString, o.Title)
	if a, u := atom(c, "_NET_WM_NAME"), atom(c, "UTF8_STRING"); a != 0 && u != 0 {
		setString(c, w.win, a, u, o.Title)
	}
	if o.Class != "" {
		setString(c, w.win, xproto.AtomWmClass, xproto.AtomString, o.Class+"\x00"+o.Class+"\x00")
	}
	if pid := atom(c, "_NET_WM_PID"); pid != 0 {
		v := uint32(os.Getpid())
		xproto.ChangeProperty(c, xproto.PropModeReplace, w.win, pid, xproto.AtomCardinal, 32, 1,
			[]byte{byte(v), byte(v >> 8), byte(v >> 16), byte(v >> 24)})
	}
	// Stato iniziale: secondo EWMH il client lo imposta prima della mappatura.
	w.root = scr.Root
	var states []string
	if o.Maximized {
		states = append(states, "_NET_WM_STATE_MAXIMIZED_VERT", "_NET_WM_STATE_MAXIMIZED_HORZ")
	}
	if o.Fullscreen {
		states = append(states, "_NET_WM_STATE_FULLSCREEN")
		w.fullscreen = true
	}
	if st := atom(c, "_NET_WM_STATE"); st != 0 && len(states) > 0 {
		var b []byte
		for _, name := range states {
			a := atom(c, name)
			b = append(b, byte(a), byte(a>>8), byte(a>>16), byte(a>>24))
		}
		xproto.ChangeProperty(c, xproto.PropModeReplace, w.win, st, xproto.AtomAtom, 32, uint32(len(states)), b)
	}
	// WM_NORMAL_HINTS: 18 CARD32, flag PMinSize (16) con min_width/min_height.
	if o.MinW > 0 && o.MinH > 0 {
		hints := make([]byte, 18*4)
		put := func(i int, v uint32) {
			hints[4*i], hints[4*i+1], hints[4*i+2], hints[4*i+3] = byte(v), byte(v>>8), byte(v>>16), byte(v>>24)
		}
		put(0, 16)
		put(5, uint32(o.MinW))
		put(6, uint32(o.MinH))
		xproto.ChangeProperty(c, xproto.PropModeReplace, w.win, xproto.AtomWmNormalHints, xproto.AtomWmSizeHints, 32, 18, hints)
	}
	w.wmProtocols, w.wmDelete = atom(c, "WM_PROTOCOLS"), atom(c, "WM_DELETE_WINDOW")
	if w.wmProtocols != 0 && w.wmDelete != 0 {
		d := uint32(w.wmDelete)
		xproto.ChangeProperty(c, xproto.PropModeReplace, w.win, w.wmProtocols, xproto.AtomAtom, 32, 1,
			[]byte{byte(d), byte(d >> 8), byte(d >> 16), byte(d >> 24)})
	}
	if w.gc, err = xproto.NewGcontextId(c); err != nil {
		return nil, err
	}
	xproto.CreateGC(c, w.gc, xproto.Drawable(w.win), 0, nil)
	if err := xproto.MapWindowChecked(c, w.win).Check(); err != nil {
		return nil, fmt.Errorf("x11: map: %w", err)
	}
	// Alcuni window manager leggono lo stato solo dalla richiesta EWMH a
	// finestra mappata: si manda anche quella. (Omarchy ignora apposta le
	// richieste di massimizzazione: lì resta lo schermo intero, vedi
	// SetFullscreen.)
	if o.Maximized {
		w.requestState(1, "_NET_WM_STATE_MAXIMIZED_VERT", "_NET_WM_STATE_MAXIMIZED_HORZ")
	}
	if o.Fullscreen {
		w.requestState(1, "_NET_WM_STATE_FULLSCREEN")
	}
	if err := w.loadKeymap(setup); err != nil {
		w.Close()
		return nil, err
	}
	go w.loop()
	return w, nil
}

// requestState chiede al window manager di cambiare _NET_WM_STATE (action:
// 0 togli, 1 aggiungi, 2 alterna) per uno o due stati.
func (w *Window) requestState(action uint32, states ...string) {
	st := atom(w.conn, "_NET_WM_STATE")
	if st == 0 || len(states) == 0 {
		return
	}
	var d [5]uint32
	d[0] = action
	for i, name := range states[:min(2, len(states))] {
		d[1+i] = uint32(atom(w.conn, name))
	}
	d[3] = 1 // richiesta di un'applicazione normale
	ev := xproto.ClientMessageEvent{Format: 32, Window: w.win, Type: st, Data: xproto.ClientMessageDataUnionData32New(d[:])}
	xproto.SendEvent(w.conn, false, w.root, xproto.EventMaskSubstructureRedirect|xproto.EventMaskSubstructureNotify, string(ev.Bytes()))
	xproto.GetInputFocus(w.conn).Reply() // la richiesta parte subito
}

// SetFullscreen chiede al window manager lo schermo intero (o di uscirne).
// Solo per le finestre di OpenWindow.
func (w *Window) SetFullscreen(on bool) {
	if !w.managed {
		return
	}
	action := uint32(0)
	if on {
		action = 1
	}
	w.fullscreen = on
	w.requestState(action, "_NET_WM_STATE_FULLSCREEN")
}

// Fullscreen dice se l'ultima richiesta è stata lo schermo intero.
func (w *Window) Fullscreen() bool { return w.fullscreen }

// Open crea la finestra sul monitor m, prende la tastiera e nasconde nulla:
// il cursore si gestisce con ShowCursor.
func Open(c *xgb.Conn, m Monitor, bg uint32) (*Window, error) {
	setup, scr, err := checkScreen(c)
	if err != nil {
		return nil, err
	}
	w := &Window{W: m.W, H: m.H, Monitor: m, conn: c, depth: scr.RootDepth,
		maxReq: int(setup.MaximumRequestLength) * 4, Events: make(chan Event, 256), closed: make(chan struct{})}
	if w.win, err = xproto.NewWindowId(c); err != nil {
		return nil, err
	}
	mask := uint32(xproto.CwBackPixel | xproto.CwOverrideRedirect | xproto.CwEventMask)
	events := uint32(xproto.EventMaskKeyPress | xproto.EventMaskButtonPress | xproto.EventMaskButtonRelease |
		xproto.EventMaskPointerMotion | xproto.EventMaskExposure | xproto.EventMaskStructureNotify)
	if err := xproto.CreateWindowChecked(c, scr.RootDepth, w.win, scr.Root, int16(m.X), int16(m.Y),
		uint16(m.W), uint16(m.H), 0, xproto.WindowClassInputOutput, scr.RootVisual,
		mask, []uint32{bg, 1, events}).Check(); err != nil {
		return nil, fmt.Errorf("x11: create window: %w", err)
	}
	title := "tobii4c calibrazione"
	xproto.ChangeProperty(c, xproto.PropModeReplace, w.win, xproto.AtomWmName, xproto.AtomString, 8, uint32(len(title)), []byte(title))
	if w.gc, err = xproto.NewGcontextId(c); err != nil {
		return nil, err
	}
	xproto.CreateGC(c, w.gc, xproto.Drawable(w.win), 0, nil)

	// Cursore invisibile per la calibrazione (pixmap 1×1 a profondità 1).
	pix, _ := xproto.NewPixmapId(c)
	xproto.CreatePixmap(c, 1, pix, xproto.Drawable(scr.Root), 1, 1)
	w.blank, _ = xproto.NewCursorId(c)
	xproto.CreateCursor(c, w.blank, pix, pix, 0, 0, 0, 0, 0, 0, 0, 0)
	xproto.FreePixmap(c, pix)

	if err := xproto.MapWindowChecked(c, w.win).Check(); err != nil {
		return nil, fmt.Errorf("x11: map: %w", err)
	}
	xproto.ConfigureWindow(c, w.win, xproto.ConfigWindowStackMode, []uint32{xproto.StackModeAbove})

	// Tastiera in esclusiva: senza window manager (override-redirect) è
	// l'unico modo di ricevere i tasti. Si riprova finché un altro client
	// (es. un menu aperto) non rilascia la sua grab.
	grabbed := false
	for i := 0; i < 40 && !grabbed; i++ {
		r, err := xproto.GrabKeyboard(c, false, w.win, xproto.TimeCurrentTime, xproto.GrabModeAsync, xproto.GrabModeAsync).Reply()
		grabbed = err == nil && r.Status == xproto.GrabStatusSuccess
		if !grabbed {
			time.Sleep(50 * time.Millisecond)
		}
	}
	if !grabbed {
		w.Close()
		return nil, errors.New("x11: impossibile prendere la tastiera (un'altra applicazione la tiene)")
	}
	xproto.SetInputFocus(c, xproto.InputFocusParent, w.win, xproto.TimeCurrentTime)

	if err := w.loadKeymap(setup); err != nil {
		w.Close()
		return nil, err
	}
	go w.loop()
	return w, nil
}

func (w *Window) keysym(kc xproto.Keycode, shift bool) uint32 {
	i := int(kc-w.minKC) * w.perKC
	if i < 0 || i >= len(w.syms) {
		return 0
	}
	s := uint32(w.syms[i])
	if shift && w.perKC > 1 && w.syms[i+1] != 0 && s >= 0x30 && s < 0x7f && (s < 'a' || s > 'z') {
		s = uint32(w.syms[i+1]) // simboli shiftati, non le lettere
	}
	return s
}

func (w *Window) send(e Event) {
	select {
	case w.Events <- e:
	default: // l'interfaccia è bloccata: si perdono solo movimenti del mouse
	}
}

func (w *Window) loop() {
	defer close(w.closed)
	curW, curH := w.W, w.H
	for {
		ev, err := w.conn.WaitForEvent()
		if ev == nil && err == nil {
			w.send(Event{Kind: Closed})
			return
		}
		switch e := ev.(type) {
		case xproto.MappingNotifyEvent:
			// la tabella dei tasti è cambiata (layout, tastiere virtuali)
			if e.Request == xproto.MappingKeyboard {
				if setup := xproto.Setup(w.conn); setup != nil {
					_ = w.loadKeymap(setup)
				}
			}
		case xproto.KeyPressEvent:
			st := e.State
			w.send(Event{Kind: KeyDown, Sym: w.keysym(e.Detail, st&xproto.ModMaskShift != 0),
				Shift: st&xproto.ModMaskShift != 0, Ctrl: st&xproto.ModMaskControl != 0, Alt: st&xproto.ModMask1 != 0})
		case xproto.ButtonPressEvent:
			w.send(Event{Kind: MouseDown, X: int(e.EventX), Y: int(e.EventY), Button: int(e.Detail)})
		case xproto.ButtonReleaseEvent:
			w.send(Event{Kind: MouseUp, X: int(e.EventX), Y: int(e.EventY), Button: int(e.Detail)})
		case xproto.MotionNotifyEvent:
			w.send(Event{Kind: MouseMove, X: int(e.EventX), Y: int(e.EventY)})
		case xproto.ExposeEvent:
			if e.Count == 0 {
				w.send(Event{Kind: Expose})
			}
		case xproto.ConfigureNotifyEvent:
			if e.Window == w.win && (int(e.Width) != curW || int(e.Height) != curH) {
				curW, curH = int(e.Width), int(e.Height)
				w.sendResize(curW, curH)
			}
		case xproto.ClientMessageEvent:
			if w.wmDelete != 0 && e.Type == w.wmProtocols && e.Format == 32 && xproto.Atom(e.Data.Data32[0]) == w.wmDelete {
				w.send(Event{Kind: Closed})
			}
		case xproto.DestroyNotifyEvent:
			w.send(Event{Kind: Closed})
			return
		}
	}
}

// sendResize consegna un Resize anche con la coda piena (il ridimensionamento
// non si può perdere: l'interfaccia ridisegnerebbe alla misura vecchia).
func (w *Window) sendResize(width, height int) {
	ev := Event{Kind: Resize, W: width, H: height}
	select {
	case w.Events <- ev:
	case <-time.After(time.Second):
	}
}

// ShowCursor mostra o nasconde il puntatore sopra la finestra.
func (w *Window) ShowCursor(show bool) {
	c := uint32(w.blank)
	if show {
		c = 0
	}
	xproto.ChangeWindowAttributes(w.conn, w.win, xproto.CwCursor, []uint32{c})
}

// Present carica sul server le regioni indicate di img (RGBA → BGRX), a
// strisce che rispettano la dimensione massima di una richiesta X.
func (w *Window) Present(img *image.RGBA, rects ...image.Rectangle) error {
	for _, r := range rects {
		r = r.Intersect(img.Bounds())
		if r.Empty() {
			continue
		}
		rowBytes := r.Dx() * 4
		rows := max(1, (w.maxReq-64)/rowBytes)
		for y0 := r.Min.Y; y0 < r.Max.Y; y0 += rows {
			y1 := min(y0+rows, r.Max.Y)
			n := (y1 - y0) * rowBytes
			if cap(w.buf) < n {
				w.buf = make([]byte, n)
			}
			b := w.buf[:n]
			k := 0
			for y := y0; y < y1; y++ {
				src := img.Pix[img.PixOffset(r.Min.X, y):][:rowBytes]
				for x := 0; x < rowBytes; x += 4 {
					b[k], b[k+1], b[k+2], b[k+3] = src[x+2], src[x+1], src[x], 0xff
					k += 4
				}
			}
			xproto.PutImage(w.conn, xproto.ImageFormatZPixmap, xproto.Drawable(w.win), w.gc,
				uint16(r.Dx()), uint16(y1-y0), int16(r.Min.X), int16(y0), 0, w.depth, b)
		}
	}
	// Round-trip: il frame è sul server prima del successivo (niente code infinite).
	_, err := xproto.GetInputFocus(w.conn).Reply()
	return err
}

// Close rilascia tastiera e finestra.
func (w *Window) Close() {
	if !w.managed {
		xproto.UngrabKeyboard(w.conn, xproto.TimeCurrentTime)
	}
	xproto.DestroyWindow(w.conn, w.win)
	xproto.GetInputFocus(w.conn).Reply()
}

// EDIDInfo sono i campi utili di un EDID.
type EDIDInfo struct {
	Model             string
	WidthPx, HeightPx int
	WidthMM, HeightMM float64
}

// ParseEDID legge il primo detailed timing descriptor (dimensioni in mm e
// risoluzione nativa) e il nome del monitor (descrittore 0xFC).
func ParseEDID(b []byte) (EDIDInfo, bool) {
	var e EDIDInfo
	hdr := []byte{0, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0}
	if len(b) < 128 || string(b[:8]) != string(hdr) {
		return e, false
	}
	d := b[54:72]
	if d[0] != 0 || d[1] != 0 {
		e.WidthPx = int(d[2]) | int(d[4]>>4)<<8
		e.HeightPx = int(d[5]) | int(d[7]>>4)<<8
		e.WidthMM = float64(int(d[12]) | int(d[14]>>4)<<8)
		e.HeightMM = float64(int(d[13]) | int(d[14]&0x0f)<<8)
	}
	if e.WidthMM == 0 && b[21] > 0 {
		e.WidthMM, e.HeightMM = float64(b[21])*10, float64(b[22])*10
	}
	for _, o := range []int{54, 72, 90, 108} {
		if b[o] == 0 && b[o+1] == 0 && b[o+2] == 0 && b[o+3] == 0xfc {
			name := b[o+5 : o+18]
			for i, ch := range name {
				if ch == '\n' {
					name = name[:i]
					break
				}
			}
			e.Model = string(trimSpace(name))
		}
	}
	return e, true
}

func trimSpace(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == ' ' || b[len(b)-1] == 0) {
		b = b[:len(b)-1]
	}
	return b
}
