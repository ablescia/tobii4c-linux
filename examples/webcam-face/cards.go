//go:build linux

package main

import (
	"image"
	"image/color"
	"runtime"
	"strings"

	"github.com/ablescia/tobii4c-linux/internal/headpose"
	"github.com/ablescia/tobii4c-linux/internal/x11ui"
)

// kvList disegna righe etichetta → valore.
type kvList struct {
	a      *App
	img    *image.RGBA
	b      image.Rectangle
	y      int // linea di base della riga corrente
	labelW int
	lh     int
}

func (a *App) newKV(img *image.RGBA, b image.Rectangle, labelW float64) *kvList {
	return &kvList{a: a, img: img, b: b, y: b.Min.Y + a.px(12), labelW: a.px(labelW), lh: a.px(17.5)}
}

// row disegna label e valore (monospazio se mono).
func (k *kvList) row(label, value string, c color.RGBA, mono bool) {
	if k.y > k.b.Max.Y {
		return
	}
	a := k.a
	x11ui.Text(k.img, a.f.small, k.b.Min.X, k.y, fit(a.f.small, label, k.labelW-a.px(6)), colDim, x11ui.Left)
	f := a.f.label
	if mono {
		f = a.f.mono
	}
	x11ui.Text(k.img, f, k.b.Min.X+k.labelW, k.y, fit(f, value, k.b.Dx()-k.labelW), c, x11ui.Left)
	k.y += k.lh
}

func (k *kvList) gap() { k.y += k.a.px(5) }

func vec(v [3]float64, d int) string {
	return num(v[0], d) + "; " + num(v[1], d) + "; " + num(v[2], d)
}

// faceOK dice se l'ultimo frame ha un viso.
func (a *App) faceOK() bool { return a.snap.Have && a.snap.Res.Face }

// --- testa -------------------------------------------------------------------

func (a *App) headNote() string {
	return "focale ipotizzata " + num(a.Eng.Focal, 0) + " px (campo visivo " + num(a.Eng.FOV, 0) + "°)"
}

func (a *App) drawHead(img *image.RGBA, b image.Rectangle) {
	res := &a.snap.Res
	m := &res.M
	ok := a.faceOK() && m.PoseOK
	gs := min(a.px(112), b.Dx()*2/5, max(0, b.Dy()-a.px(4)))
	gr := image.Rect(b.Min.X, b.Min.Y+a.px(4), b.Min.X+gs, b.Min.Y+a.px(4)+gs)
	a.drawGizmo(img, gr, m.Pose.Pitch, m.Pose.Yaw, m.Pose.Roll, ok)
	x := gr.Max.X + a.px(14)
	y := gr.Min.Y + a.px(22)
	// segni come Euler del driver: yaw positivo verso la sinistra del
	// soggetto, roll positivo con la testa piegata sulla spalla sinistra
	hints := [3][2]string{{"in su", "in giù"}, {"verso la sua sx", "verso la sua dx"}, {"sulla spalla sx", "sulla spalla dx"}}
	for i, ang := range [3]float64{m.Pose.Pitch, m.Pose.Yaw, m.Pose.Roll} {
		name := [3]string{"Pitch", "Yaw", "Roll"}[i]
		val, c, hint := "—", colFaint, ""
		if ok {
			val, c = signed(deg(ang), 1)+"°", colText
			if ang >= 0 {
				hint = hints[i][0]
			} else {
				hint = hints[i][1]
			}
		}
		x11ui.Text(img, a.f.small, x, y, name, colDim, x11ui.Left)
		x11ui.Text(img, a.f.big, x+a.px(44), y+a.px(2), val, c, x11ui.Left)
		x11ui.Text(img, a.f.small, b.Max.X, y, hint, colFaint, x11ui.Right)
		y += a.px(36)
	}
	k := a.newKV(img, image.Rect(b.Min.X, gr.Max.Y+a.px(8), b.Max.X, b.Max.Y), 140)
	if !a.faceOK() {
		k.row("Posa", "nessun viso", colFaint, false)
		return
	}
	if m.PoseOK {
		k.row("Centro della testa", vec(m.Pose.Center, 0)+" mm", colText, true)
		k.row("Distanza (modello)", num(m.DistMM, 0)+" mm", colText, true)
	} else {
		k.row("Posa", "fit del modello canonico non riuscito", colWarn, false)
	}
	k.row("Distanza (iride)", num(m.IrisMM, 0)+" mm · iride di "+num(irisDiameterMM, 1)+" mm", colText, true)
	k.row("Linea degli occhi", signed(deg(m.EyeLine), 1)+"° nell'immagine", colText, true)
}

// --- occhi -------------------------------------------------------------------

func (a *App) drawEyes(img *image.RGBA, b image.Rectangle) {
	res := &a.snap.Res
	m := &res.M
	ok := a.faceOK()
	labelW := a.px(150)
	colW := (b.Dx() - labelW) / 2
	y := b.Min.Y + a.px(12)
	lh := a.px(17.5)
	for k, name := range [2]string{"sinistro", "destro"} {
		x11ui.Text(img, a.f.small, b.Min.X+labelW+k*colW, y, name, colEyeCol[k], x11ui.Left)
	}
	y += lh
	row := func(label string, f func(e *EyeMetrics) string) {
		if y > b.Max.Y {
			return
		}
		x11ui.Text(img, a.f.small, b.Min.X, y, fit(a.f.small, label, labelW-a.px(6)), colDim, x11ui.Left)
		for k := range m.Eyes {
			v, c := "—", colFaint
			if ok {
				v, c = f(&m.Eyes[k]), colText
			}
			x11ui.Text(img, a.f.mono, b.Min.X+labelW+k*colW, y, fit(a.f.mono, v, colW-a.px(4)), c, x11ui.Left)
		}
		y += lh
	}
	row("Centro dell'iride, px", func(e *EyeMetrics) string { return num(e.Iris[0], 1) + "; " + num(e.Iris[1], 1) })
	row("Diametro dell'iride", func(e *EyeMetrics) string { return num(e.IrisPx, 1) + " px" })
	row("EAR (apertura relativa)", func(e *EyeMetrics) string { return num(e.EAR, 3) })
	row("Distanza tra le palpebre", func(e *EyeMetrics) string { return num(e.OpenMM, 1) + " mm" })
	row("Iride: orizzontale", func(e *EyeMetrics) string { return signed(e.PosH, 2) })
	row("Iride: verticale", func(e *EyeMetrics) string { return signed(e.PosV, 2) })
	k := &kvList{a: a, img: img, b: b, y: y + a.px(5), labelW: labelW, lh: lh}
	if !ok {
		k.row("Stato", "nessun viso", colFaint, false)
		return
	}
	state, c := "aperti", colOK
	if res.Closed {
		state, c = "chiusi", colWarn
	}
	k.row("Stato", state+" · EAR a occhi aperti "+num(res.BlinkBase, 3), c, false)
	bl := a.snap.C.Blinks - a.base.Blinks
	k.row("Ammiccamenti", itoa(int(bl))+" · "+itoa(res.BlinksMin)+"/min · ultimo "+
		itoa(int(res.BlinkDur.Milliseconds()))+" ms", colText, true)
	k.row("Distanza tra le iridi", num(m.IPDPx, 1)+" px · "+num(m.IPDMM, 1)+" mm", colText, true)
}

// --- bocca e viso ------------------------------------------------------------

func (a *App) drawFace(img *image.RGBA, b image.Rectangle) {
	res := &a.snap.Res
	m := &res.M
	k := a.newKV(img, b, 150)
	if !a.faceOK() {
		k.row("Viso", "nessun viso", colFaint, false)
		return
	}
	k.row("Apertura della bocca", num(m.MouthOpenMM, 1)+" mm · MAR "+num(m.MAR, 2), colText, true)
	k.row("Larghezza della bocca", num(m.MouthWidthMM, 1)+" mm", colText, true)
	bw, bh := m.Box[2]-m.Box[0], m.Box[3]-m.Box[1]
	k.row("Riquadro dei punti", num(bw, 0)+"×"+num(bh, 0)+" px · "+num(100*bw/float64(res.W), 0)+" % della larghezza", colText, true)
	cx, cy := (m.Box[0]+m.Box[2])/2/float64(res.W), (m.Box[1]+m.Box[3])/2/float64(res.H)
	k.row("Centro del viso", num(cx, 3)+"; "+num(cy, 3)+" (0..1)", colText, true)
	k.row("Scala dall'iride", num(m.MMPerPx, 3)+" mm/px", colText, true)
	k.row("Profondità dei punti", num(m.DepthSpan, 0)+" px (z relativa del modello)", colText, true)
	st := &res.Img
	if st.HaveFace {
		k.row("Luminanza del viso", num(st.FaceMean, 0)+" ± "+num(st.FaceStd, 0)+" · "+itoa(st.FaceP1)+"–"+itoa(st.FaceP99), colText, true)
		k.row("Nitidezza del viso", num(st.FaceSharp, 0)+" (varianza del laplaciano)", colText, true)
	}
}

// --- modello -----------------------------------------------------------------

func (a *App) drawModel(img *image.RGBA, b image.Rectangle) {
	res := &a.snap.Res
	r := &a.rates
	k := a.newKV(img, b, 140)
	if !a.snap.Have {
		k.row("Stato", "in attesa del primo frame", colFaint, false)
	} else {
		c := colOK
		if !res.Face {
			c = colWarn
		}
		k.row("Probabilità del viso", num(res.Score, 3), c, true)
		k.row("Modalità", res.Mode, colText, false)
		k.row("Tempo per frame", num(res.InferMs, 1)+" ms · media "+num(res.InfMean, 1)+" ms", colText, true)
		k.row("p95 · massimo", num(res.InfP95, 1)+" · "+num(res.InfMax, 1)+" ms (120 frame)", colText, true)
	}
	k.row("Elaborati", num(r.ProcHz, 1)+"/s · scartati "+num(r.DropHz, 1)+"/s", colText, true)
	k.row("Viso trovato", num(r.FacePct, 0)+" % dei frame", colText, true)
	c := a.snap.C
	k.row("Ricerche a griglia", itoa(int(c.Searches-a.base.Searches))+" · ultima "+num(res.SearchMs, 0)+" ms", colText, true)
	if cr := res.Crop; cr.Side > 0 {
		k.row("Ritaglio", num(cr.CX, 0)+"; "+num(cr.CY, 0)+" · lato "+num(cr.Side, 0)+" px · "+signed(deg(cr.Angle), 1)+"°", colText, true)
	}
	k.row("Cattura → risultato", num(r.PipelineMs, 1)+" ms", colText, true)
	k.gap()
	k.row("Rete", itoa(headpose.ModelInput)+"×"+itoa(headpose.ModelInput)+" → "+itoa(headpose.NumLandmarks)+" punti (468 + 10 delle iridi)", colText, false)
	k.row("Costo", num(float64(a.Eng.MACs)/1e6, 0)+" MMAC per inferenza · "+itoa(a.Eng.Workers)+" goroutine", colText, true)
	sha, sc := "SHA-256 verificato", colOK
	if !a.Eng.ModelOK {
		sha, sc = "SHA-256 diverso da quello del driver", colWarn
	}
	k.row("File", a.Eng.Model+" · "+sha, sc, false)
}

// --- webcam ------------------------------------------------------------------

func (a *App) camNote() string { return a.Eng.Info.Path }

var colorspaces = map[uint32]string{1: "SMPTE 170M", 2: "SMPTE 240M", 3: "Rec. 709", 5: "470 M", 6: "470 BG",
	7: "JPEG", 8: "sRGB", 9: "opRGB", 10: "BT.2020", 11: "raw", 12: "DCI-P3"}
var quantizations = map[uint32]string{0: "quantizzazione predefinita", 1: "gamma completa", 2: "gamma ridotta"}

func (a *App) drawCamera(img *image.RGBA, b image.Rectangle) {
	in := &a.Eng.Info
	r := &a.rates
	c := a.snap.C
	k := a.newKV(img, b, 130)
	k.row("Nome", in.Card, colText, false)
	u := in.USB
	if u.ID != "" && u.ID != ":" {
		s := u.ID
		if p := strings.TrimSpace(u.Manufacturer + " " + u.Product); p != "" {
			s += " · " + p
		}
		k.row("USB", s, colText, false)
		k.row("Collegamento", u.Speed+" Mbit/s · porta "+u.Port+" · rev. "+u.BCD, colText, false)
		k.row("Seriale", u.Serial, colText, true)
	}
	k.row("Driver", in.Driver+" "+in.Version+" · "+in.Bus, colText, false)
	k.row("Formato", fourccString(in.PixFmt)+" · "+itoa(in.W)+"×"+itoa(in.H)+" · "+itoa(in.BytesPerLine)+" B/riga", colText, true)
	cs := colorspaces[in.Colorspace]
	if cs == "" {
		cs = "spazio " + itoa(int(in.Colorspace))
	}
	k.row("Colore", cs+" · "+quantizations[in.Quantization], colText, false)
	k.row("Frequenza", "nominale "+num(in.FPS, 1)+" fps · misurata "+num(r.FPS, 2)+" fps", colText, true)
	seq := "—"
	if a.snap.Have {
		seq = itoa(int(a.snap.Res.CamSeq))
	}
	k.row("Frame", "sequenza "+seq+" · ricevuti "+itoa(int(c.Captured-a.base.Captured)), colText, true)
	lost := c.SeqGaps - a.base.SeqGaps
	lc := colText
	if lost > 0 {
		lc = colWarn
	}
	k.row("Persi dal driver", itoa(int(lost))+" · con errore "+itoa(int(c.BufErrors-a.base.BufErrors))+
		" · illeggibili "+itoa(int(c.DecodeErrs-a.base.DecodeErrs)), lc, true)
	k.row("Inizio frame → app", num(r.LatencyMs, 1)+" ms (timestamp del driver)", colText, true)
	k.row("Banda", num(r.MBps, 2)+" MB/s · "+num(r.FrameKiB, 0)+" KiB/frame", colText, true)
	conv := "solo la Y"
	if in.PixFmt == pixMJPG {
		conv = "JPEG → Y"
	}
	k.row("Conversione", num(r.DecodeUs, 0)+" µs/frame · "+conv, colText, true)
	k.row("Buffer · formati", itoa(in.Buffers)+" × "+sizeText(float64(in.BufferBytes))+" · "+strings.Join(in.Formats, ", "), colText, false)
}

// --- immagine ----------------------------------------------------------------

func (a *App) drawImage(img *image.RGBA, b image.Rectangle) {
	st := &a.snap.Res.Img
	hh := min(a.px(54), b.Dy()/3)
	a.drawHist(img, image.Rect(b.Min.X, b.Min.Y+a.px(2), b.Max.X, b.Min.Y+a.px(2)+hh), st)
	k := a.newKV(img, image.Rect(b.Min.X, b.Min.Y+hh+a.px(6), b.Max.X, b.Max.Y), 130)
	if !a.snap.Have {
		return
	}
	k.row("Media ± deviazione", num(st.Mean, 1)+" ± "+num(st.Std, 1), colText, true)
	k.row("Min · mediana · max", itoa(st.Min)+" · "+itoa(st.P50)+" · "+itoa(st.Max), colText, true)
	k.row("1° – 99° percentile", itoa(st.P1)+" – "+itoa(st.P99), colWarn, true)
	k.row("Neri · saturati", num(100*st.Dark, 2)+" % · "+num(100*st.Bright, 2)+" %", colText, true)
	k.row("Calcolo", num(st.ComputeMicroseconds, 0)+" µs su "+itoa(st.SampledPixels)+" pixel (passo 2)", colFaint, true)
}

// --- controlli ---------------------------------------------------------------

func (a *App) ctrlNote() string {
	n := 0
	for _, c := range a.snap.Ctrls {
		if !c.Class {
			n++
		}
	}
	return itoa(n) + " controlli · letti ogni secondo · in grigio gli inattivi"
}

// Nomi italiani dei controlli standard (gli altri restano come li dà il driver).
var ctrlNames = map[uint32]string{
	0x00980900: "Luminosità", 0x00980901: "Contrasto", 0x00980902: "Saturazione", 0x00980903: "Tinta",
	0x0098090c: "Bianco auto", 0x00980910: "Gamma", 0x00980913: "Guadagno",
	0x00980918: "Antisfarfallio", 0x0098091a: "Temp. del bianco", 0x0098091b: "Nitidezza",
	0x0098091c: "Controluce", 0x009a0901: "Esposizione", 0x009a0902: "Tempo di posa",
	0x009a0903: "Priorità posa", 0x009a090a: "Messa a fuoco", 0x009a090c: "Fuoco auto",
	0x009a090d: "Zoom", 0x009a0908: "Pan", 0x009a0909: "Tilt",
}

func ctrlName(c *Control) string {
	if n := ctrlNames[c.ID]; n != "" {
		return n
	}
	return c.Name
}

var menuNames = map[string]string{
	"Disabled": "spento", "Manual Mode": "manuale", "Aperture Priority Mode": "automatica",
	"Auto Mode": "automatica", "Shutter Priority Mode": "priorità otturatore",
}

func ctrlValue(c *Control) string {
	if !c.OK {
		return "—"
	}
	var s string
	switch c.Type {
	case ctrlTypeBoolean:
		s = "no"
		if c.Value != 0 {
			s = "sì"
		}
	case ctrlTypeMenu, ctrlTypeIntegerMenu:
		s = c.Menu[c.Value]
		if t, ok := menuNames[s]; ok {
			s = t
		}
		if s == "" {
			s = itoa(int(c.Value))
		}
	case ctrlTypeInteger:
		s = itoa(int(c.Value))
		if c.ID == 0x009a0902 { // unità di 100 µs
			s = num(float64(c.Value)/10, 1) + " ms"
		}
	default:
		s = itoa(int(c.Value))
	}
	if c.Value != c.Default && c.Type != ctrlTypeMenu {
		s += " (pred. " + itoa(int(c.Default)) + ")"
	}
	return s
}

func (a *App) drawControls(img *image.RGBA, b image.Rectangle) {
	var cs []*Control
	for i := range a.snap.Ctrls {
		if c := &a.snap.Ctrls[i]; !c.Class {
			cs = append(cs, c)
		}
	}
	if len(cs) == 0 {
		x11ui.Text(img, a.f.small, b.Min.X, b.Min.Y+a.px(12), "nessun controllo", colFaint, x11ui.Left)
		return
	}
	g := a.px(14)
	colW := (b.Dx() - g) / 2
	rows := (len(cs) + 1) / 2
	lh := min(a.px(17.5), max(a.px(12), (b.Dy()-a.px(4))/max(1, rows)))
	labelW := 0
	for _, c := range cs {
		labelW = max(labelW, a.f.small.Measure(ctrlName(c)))
	}
	labelW = min(labelW+a.px(10), colW*3/5)
	for i, c := range cs {
		col, rr := i/rows, i%rows
		x := b.Min.X + col*(colW+g)
		y := b.Min.Y + a.px(12) + rr*lh
		if y > b.Max.Y {
			continue
		}
		lc, vc := colDim, colText
		if c.Flags&ctrlFlagInactive != 0 {
			lc, vc = colFaint, colFaint
		}
		x11ui.Text(img, a.f.small, x, y, fit(a.f.small, ctrlName(c), labelW-a.px(4)), lc, x11ui.Left)
		x11ui.Text(img, a.f.monoS, x+labelW, y, fit(a.f.monoS, ctrlValue(c), colW-labelW), vc, x11ui.Left)
	}
}

// --- processo ----------------------------------------------------------------

func (a *App) procNote() string {
	return runtime.Version() + " · " + runtime.GOOS + "/" + runtime.GOARCH
}

func (a *App) drawProcess(img *image.RGBA, b image.Rectangle) {
	s := &a.sys
	k := a.newKV(img, b, 130)
	k.row("CPU", num(s.CPUPct, 0)+" % di un core · "+itoa(runtime.NumCPU())+" core", colText, true)
	k.row("Memoria RSS", sizeText(float64(s.RSS))+" · Go dal sistema "+sizeText(float64(s.TotalSys)), colText, true)
	k.row("Heap Go", sizeText(float64(s.HeapAlloc))+" in uso · "+sizeText(float64(s.HeapSys))+" riservati", colText, true)
	k.row("Garbage collector", itoa(int(s.NumGC))+" cicli · "+num(s.GCPerSec, 1)+"/s · pausa "+num(float64(s.LastPause.Microseconds()), 0)+" µs", colText, true)
	k.row("Goroutine · thread", itoa(s.Gorout)+" · "+itoa(s.Threads)+" · GOMAXPROCS "+itoa(runtime.GOMAXPROCS(0)), colText, true)
	k.row("Interfaccia", "disegno "+num(a.drawMs, 1)+" ms · invio a X "+num(a.presentMs, 1)+" ms", colText, true)
	k.row("Finestra", itoa(a.W)+"×"+itoa(a.H)+" · scala "+num(a.s, 2), colText, true)
	k.row("Avviato da", fmtDuration(a.now.Sub(s.Started)), colText, true)
}
