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

// Open crea la finestra sul monitor m, prende la tastiera e nasconde nulla:
// il cursore si gestisce con ShowCursor.
func Open(c *xgb.Conn, m Monitor, bg uint32) (*Window, error) {
	setup := xproto.Setup(c)
	scr := setup.DefaultScreen(c)
	if setup.ImageByteOrder != xproto.ImageOrderLSBFirst {
		return nil, errors.New("x11: byte order MSB non supportato")
	}
	bpp := 0
	for _, f := range setup.PixmapFormats {
		if f.Depth == scr.RootDepth {
			bpp = int(f.BitsPerPixel)
		}
	}
	if scr.RootDepth != 24 && scr.RootDepth != 32 || bpp != 32 {
		return nil, fmt.Errorf("x11: profondità %d/%d bpp non supportata (serve TrueColor 24/32)", scr.RootDepth, bpp)
	}
	w := &Window{W: m.W, H: m.H, Monitor: m, conn: c, depth: scr.RootDepth,
		maxReq: int(setup.MaximumRequestLength) * 4, Events: make(chan Event, 256), closed: make(chan struct{})}
	var err error
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

	km, err := xproto.GetKeyboardMapping(c, setup.MinKeycode, byte(setup.MaxKeycode-setup.MinKeycode+1)).Reply()
	if err != nil {
		w.Close()
		return nil, fmt.Errorf("x11: keyboard mapping: %w", err)
	}
	w.minKC, w.perKC, w.syms = setup.MinKeycode, int(km.KeysymsPerKeycode), km.Keysyms
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
	for {
		ev, err := w.conn.WaitForEvent()
		if ev == nil && err == nil {
			w.send(Event{Kind: Closed})
			return
		}
		switch e := ev.(type) {
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
		case xproto.DestroyNotifyEvent:
			w.send(Event{Kind: Closed})
			return
		}
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
	xproto.UngrabKeyboard(w.conn, xproto.TimeCurrentTime)
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
