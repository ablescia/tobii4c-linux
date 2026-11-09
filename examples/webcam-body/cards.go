//go:build linux

package main

import (
	"image"
	"image/color"
	"math"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/ablescia/tobii4c-linux/internal/bodypose"
	"github.com/ablescia/tobii4c-linux/internal/webcam"
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

func deg(v float64, ok bool) string {
	if !ok {
		return "—"
	}
	return signed(v, 1) + "°"
}

// found dice se l'ultimo frame elaborato ha una persona.
func (a *App) found() bool { return a.snap.Have && a.snap.Res.Pose.Found }

// --- postura ------------------------------------------------------------------

func (a *App) postureNote() string {
	if a.snap.Res.Cfg.Mode == bodypose.UpperBody {
		return "dai 25 punti nell'immagine"
	}
	return "dalle coordinate 3D del modello"
}

func (a *App) drawPosture(img *image.RGBA, b image.Rectangle) {
	res := &a.snap.Res
	m := &res.M
	k := a.newKV(img, b, 150)
	if !a.found() {
		x11ui.Text(img, a.f.big, b.Min.X, b.Min.Y+a.px(22), "nessuna persona", colFaint, x11ui.Left)
		return
	}
	name := bodypose.PostureNames[m.Posture]
	c := colText
	if m.Posture == bodypose.PostureUnknown {
		c = colFaint
	}
	t := x11ui.Text(img, a.f.big, b.Min.X, b.Min.Y+a.px(22), name, c, x11ui.Left)
	var extra []string
	if m.HandUp[0] {
		extra = append(extra, "mano sx alzata")
	}
	if m.HandUp[1] {
		extra = append(extra, "mano dx alzata")
	}
	if len(extra) > 0 {
		x11ui.Text(img, a.f.label, t.Max.X+a.px(14), b.Min.Y+a.px(20), strings.Join(extra, " · "), colWarn, x11ui.Left)
	}
	k.y = b.Min.Y + a.px(48)
	if res.Cfg.Mode == bodypose.FullBody {
		th := "—"
		if !math.IsNaN(m.ThighDeg[0]) || !math.IsNaN(m.ThighDeg[1]) {
			th = num(m.ThighDeg[0], 0) + "° · " + num(m.ThighDeg[1], 0) + "° (sx · dx)"
		}
		k.row("Cosce dalla verticale", th, colText, true)
	}
	k.row("Busto inclinato di lato", deg(m.LeanDeg, m.LeanOK)+lean(m.LeanDeg, m.LeanOK, "verso destra", "verso sinistra"), colText, true)
	if res.Cfg.Mode == bodypose.FullBody {
		k.row("Busto in avanti", deg(m.ForwardDeg, m.ForwardOK)+lean(m.ForwardDeg, m.ForwardOK, "verso la camera", "all'indietro"), colText, true)
		k.row("Rotazione del corpo", deg(m.YawDeg, m.YawOK)+lean(m.YawDeg, m.YawOK, "spalla dx indietro", "spalla sx indietro"), colText, true)
	}
	k.row("Spalle", deg(m.ShoulderTiltDeg, m.ShouldersOK)+" · larghe "+num(m.ShoulderPx, 0)+" px", colText, true)
	k.row("Testa inclinata", deg(m.HeadTiltDeg, m.HeadOK)+lean(m.HeadTiltDeg, m.HeadOK, "sulla spalla dx", "sulla spalla sx"), colText, true)
	dist := "—"
	if m.DistOK {
		dist = num(m.DistM, 2) + " m"
		if m.DistAssume {
			dist += " (spalle di " + num(bodypose.ShoulderWidthM*100, 0) + " cm ipotizzate)"
		}
	}
	k.row("Distanza dalla camera", dist, colText, true)
	n := res.Pose.N
	k.row("Punti visibili", itoa(m.Visible)+" su "+itoa(n)+" · visibilità media "+num(m.MeanVis, 2), colText, true)
	if m.Visible > 0 {
		k.row("Altezza nel frame", num(m.HeightPx, 0)+" px · "+num(100*m.HeightPx/float64(max(a.snap.Pix.H, 1)), 0)+" % dell'immagine", colText, true)
	}
}

// lean dice il verso di un angolo (per |v| ≥ 3°).
func lean(v float64, ok bool, pos, neg string) string {
	if !ok || math.Abs(v) < 3 {
		return ""
	}
	if v > 0 {
		return " " + pos
	}
	return " " + neg
}

// --- articolazioni ------------------------------------------------------------

func (a *App) jointsNote() string {
	if a.snap.Res.M.Joints3D {
		return "in 3D · visibili con probabilità ≥ 0,5"
	}
	return "nel piano dell'immagine"
}

func (a *App) drawJoints(img *image.RGBA, b image.Rectangle) {
	m := &a.snap.Res.M
	ok := a.found()
	labelW := min(a.px(200), b.Dx()/2)
	colW := (b.Dx() - labelW) / 2
	y := b.Min.Y + a.px(12)
	lh := a.px(19)
	for k, name := range [2]string{"sinistra", "destra"} {
		x11ui.Text(img, a.f.small, b.Min.X+labelW+k*colW, y, name, colSide[k], x11ui.Left)
	}
	y += lh
	help := [4]string{"braccio e fianco", "braccio e avambraccio", "busto e coscia", "coscia e gamba"}
	for row, name := range [4]string{"Spalla", "Gomito", "Anca", "Ginocchio"} {
		if y > b.Max.Y {
			break
		}
		x11ui.Text(img, a.f.label, b.Min.X, y, name, colText, x11ui.Left)
		if hw := labelW - a.f.label.Measure(name) - a.px(14); hw > a.px(50) {
			x11ui.Text(img, a.f.small, b.Min.X+a.f.label.Measure(name)+a.px(6), y, fit(a.f.small, help[row], hw), colFaint, x11ui.Left)
		}
		for side := 0; side < 2; side++ {
			j := m.Joints[2*row+side]
			v, c := "—", colFaint
			if ok && j.OK {
				v, c = num(j.Deg, 0)+"°", colText
			}
			x := b.Min.X + labelW + side*colW
			tr := x11ui.Text(img, a.f.mono, x, y, v, c, x11ui.Left)
			if ok && j.OK {
				// barra: 0..180°
				bw := colW - (tr.Max.X - x) - a.px(18)
				if bw > a.px(20) {
					br := image.Rect(tr.Max.X+a.px(8), y-a.px(8), tr.Max.X+a.px(8)+bw, y-a.px(3))
					x11ui.Fill(img, br, colPlotBg)
					f := br
					f.Max.X = br.Min.X + int(float64(bw)*math.Min(1, j.Deg/180))
					x11ui.Fill(img, f, colSide[side])
				}
			}
		}
		y += lh
	}
}

// --- scheletro 3D ---------------------------------------------------------------

func (a *App) worldNote() string {
	if a.snap.Res.Cfg.Mode == bodypose.UpperBody {
		return "solo con il corpo intero"
	}
	return "origine tra i fianchi · griglia 0,5 m"
}

func (a *App) drawWorld(img *image.RGBA, b image.Rectangle) {
	p := &a.snap.Res.Pose
	if !a.found() || !p.HasWorld {
		msg := "nessuna persona"
		if a.snap.Have && a.snap.Res.Cfg.Mode == bodypose.UpperBody {
			msg = "il modello del mezzo busto non stima le coordinate 3D"
		}
		x11ui.Text(img, a.f.small, b.Min.X+b.Dx()/2, b.Min.Y+b.Dy()/2, fit(a.f.small, msg, b.Dx()), colFaint, x11ui.Center)
		return
	}
	g := a.px(12)
	w := (b.Dx() - g) / 2
	lh := a.px(16)
	r1 := image.Rect(b.Min.X, b.Min.Y+lh, b.Min.X+w, b.Max.Y)
	r2 := image.Rect(r1.Max.X+g, b.Min.Y+lh, b.Max.X, b.Max.Y)
	x11ui.Text(img, a.f.small, r1.Min.X, b.Min.Y+a.px(10), "di fronte", colDim, x11ui.Left)
	x11ui.Text(img, a.f.small, r2.Min.X, b.Min.Y+a.px(10), "di lato", colDim, x11ui.Left)
	a.drawSkeleton3D(img, r1, false, p)
	a.drawSkeleton3D(img, r2, true, p)
}

// --- modello ------------------------------------------------------------------

func (a *App) modelNote() string {
	if i, ok := a.Eng.ModelInfo(a.snap.Cur); ok {
		return i.Spec.License
	}
	return ""
}

func (a *App) drawModel(img *image.RGBA, b image.Rectangle) {
	res := &a.snap.Res
	p := &res.Pose
	r := &a.rates
	k := a.newKV(img, b, 140)
	info, ok := a.Eng.ModelInfo(a.snap.Cur)
	if ok {
		k.row("Modello", info.Spec.Title, colText, false)
	}
	if !a.snap.Have {
		k.row("Stato", "in attesa del primo frame", colFaint, false)
		return
	}
	c := colOK
	if !p.Found {
		c = colWarn
	}
	k.row("Presenza della persona", num(p.Score, 3)+" (soglia 0,5)", c, true)
	mode := "inseguimento dal frame precedente"
	if !p.Tracking {
		mode = "detector sul frame intero"
		if p.DetRan && p.DetCount > 0 {
			mode += " · " + itoa(p.DetCount) + " persone, punteggio " + num(p.Det.Score, 2)
		} else if p.DetRan {
			mode += " · nessuna persona"
		}
	}
	k.row("Ritaglio da", mode, colText, false)
	if p.ROI.W > 0 {
		k.row("Ritaglio", num(p.ROI.CX, 0)+"; "+num(p.ROI.CY, 0)+" · lato "+num(p.ROI.W, 0)+" px · "+signed(p.ROI.Angle*180/math.Pi, 1)+"°", colText, true)
	}
	k.row("Tempo per frame", num(res.TotalMs, 1)+" ms · media "+num(res.InfMean, 1)+" · p95 "+num(res.InfP95, 1)+" · max "+num(res.InfMax, 1), colText, true)
	det := "non eseguito"
	if p.DetRan {
		det = num(res.DetMs, 2) + " ms"
	}
	k.row("Detector · landmark", det+" · "+num(res.LmMs, 2)+" ms · dopo "+num(res.PostMs, 2)+" ms", colText, true)
	k.row("Elaborati", num(r.ProcHz, 1)+"/s · scartati "+num(r.DropHz, 1)+"/s · detector "+num(r.DetHz, 1)+"/s", colText, true)
	k.row("Persona trovata", num(r.FoundPct, 0)+" % dei frame", colText, true)
	k.row("Cattura → risultato", num(r.PipelineMs, 1)+" ms", colText, true)
	if ok {
		k.gap()
		k.row("Detector", itoa(info.DetNodes)+" nodi · "+num(float64(info.DetMACs)/1e6, 0)+" MMAC", colText, true)
		k.row("Landmark", itoa(info.LmNodes)+" nodi · "+num(float64(info.LmMACs)/1e6, 0)+" MMAC · "+itoa(info.LandmarkInput)+"×"+itoa(info.LandmarkInput)+" → "+itoa(p.N)+" punti", colText, true)
		names := make([]string, len(info.Files))
		for i, f := range info.Files {
			names[i] = filepath.Base(f)
		}
		k.row("File (SHA-256 verificato)", strings.Join(names, ", "), colText, false)
	}
}

// --- calcolo --------------------------------------------------------------------

func (a *App) computeNote() string {
	if a.snap.Cur.GPU {
		return "Vulkan · shader SPIR-V del driver"
	}
	return "interprete TFLite in Go"
}

func (a *App) drawCompute(img *image.RGBA, b image.Rectangle) {
	res := &a.snap.Res
	k := a.newKV(img, b, 140)
	info, haveInfo := a.Eng.ModelInfo(a.snap.Cur)
	gpu := a.snap.Cur.GPU && a.Eng.GPU != nil
	if gpu {
		in := a.Eng.GPU.Info()
		k.row("GPU", in.Name, colText, false)
		k.row("Tipo · id", in.Type+" · "+hex4(in.VendorID)+":"+hex4(in.DeviceID)+" · Vulkan "+in.API, colText, true)
		k.row("Driver", in.Driver+" · "+in.DriverInfo, colText, false)
		k.row("Memoria locale", sizeText(float64(in.DeviceMemory))+" · workgroup "+itoa(in.MaxInvocations)+" thread · "+sizeText(float64(in.SharedMem))+" condivisa", colText, true)
		if a.snap.Have {
			det := "—"
			if res.Pose.DetRan {
				det = num(res.DetGPU, 2) + " ms"
			}
			k.row("Tempo GPU", "detector "+det+" · landmark "+num(res.LmGPU, 2)+" ms", colOK, true)
			over := (res.DetMs + res.LmMs) - (res.DetGPU + res.LmGPU)
			k.row("Invio e attesa", num(over, 2)+" ms (CPU oltre ai timestamp della GPU)", colText, true)
			if res.LmGPU > 0 && haveInfo {
				k.row("Throughput landmark", num(2*float64(info.LmMACs)/(res.LmGPU*1e-3)/1e9, 0)+" GFLOP/s", colText, true)
			}
		}
	} else {
		k.row("CPU", itoa(runtime.NumCPU())+" core · "+a.Eng.CPU.Device(), colText, false)
		if a.snap.Have {
			k.row("Tempo", "detector "+num(res.DetMs, 1)+" · landmark "+num(res.LmMs, 1)+" ms", colText, true)
			if res.LmMs > 0 && haveInfo {
				k.row("Throughput landmark", num(2*float64(info.LmMACs)/(res.LmMs*1e-3)/1e9, 1)+" GFLOP/s", colText, true)
			}
		}
		if a.Eng.GPU != nil {
			k.row("GPU disponibile", a.Eng.GPU.Info().Name+" (tasto G)", colOK, false)
		} else {
			k.row("GPU", "non disponibile: "+a.Eng.GPUErr, colWarn, false)
		}
	}
	if haveInfo {
		k.row("Memoria dei modelli", num(info.WeightsMB, 1)+" MB di pesi (float32) · "+num(info.ArenaMB, 1)+" MB di tensori", colText, true)
		k.row("Caricamento", num(float64(info.LoadTime.Milliseconds()), 0)+" ms (lettura, piano, pesi sul dispositivo)", colFaint, true)
	}
	k.row("Cambi di modello", itoa(int(a.snap.C.Switches-a.base.Switches)), colFaint, true)
}

func hex4(v uint32) string {
	const d = "0123456789abcdef"
	return string([]byte{d[v>>12&15], d[v>>8&15], d[v>>4&15], d[v&15]})
}

// --- webcam -------------------------------------------------------------------

func (a *App) camNote() string { return a.Eng.Info.Path }

var colorspaces = map[uint32]string{1: "SMPTE 170M", 2: "SMPTE 240M", 3: "Rec. 709", 5: "470 M", 6: "470 BG",
	7: "JPEG", 8: "sRGB", 9: "opRGB", 10: "BT.2020", 11: "raw", 12: "DCI-P3"}

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
		k.row("USB", s+" · "+u.Speed+" Mbit/s", colText, false)
	}
	cs := colorspaces[in.Colorspace]
	if cs == "" {
		cs = "spazio " + itoa(int(in.Colorspace))
	}
	k.row("Formato · colore", webcam.FourCCString(in.PixFmt)+" · "+itoa(in.W)+"×"+itoa(in.H)+" · "+cs, colText, true)
	k.row("Modalità", in.Choice, colText, false)
	k.row("Frequenza", "nominale "+num(in.FPS, 1)+" fps · misurata "+num(r.FPS, 2)+" fps", colText, true)
	lost := c.SeqGaps - a.base.SeqGaps
	lc := colText
	if lost > 0 {
		lc = colWarn
	}
	k.row("Frame persi", itoa(int(lost))+" · con errore "+itoa(int(c.BufErrors-a.base.BufErrors))+
		" · illeggibili "+itoa(int(c.DecodeErrs-a.base.DecodeErrs)), lc, true)
	k.row("Inizio frame → app", num(r.LatencyMs, 1)+" ms (timestamp del driver)", colText, true)
	k.row("Banda", num(r.MBps, 2)+" MB/s · "+num(r.FrameKiB, 0)+" KiB/frame", colText, true)
	k.row("Decodifica", num(r.DecodeUs, 0)+" µs · "+a.Eng.ConvNote(), colText, true)
}

// --- controlli ---------------------------------------------------------------

func (a *App) ctrlNote() string {
	if len(a.Eng.Info.Changed) > 0 {
		return "grigi gli inattivi, verdi i cambiati"
	}
	return "letti ogni secondo"
}

func (a *App) changedCtrl(id uint32) bool {
	for _, ch := range a.Eng.Info.Changed {
		if ch.ID == id {
			return true
		}
	}
	return false
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
	case webcam.CtrlTypeBoolean:
		s = "no"
		if c.Value != 0 {
			s = "sì"
		}
	case webcam.CtrlTypeMenu, webcam.CtrlTypeIntegerMenu:
		s = c.Menu[c.Value]
		if t, ok := menuNames[s]; ok {
			s = t
		}
		if s == "" {
			s = itoa(int(c.Value))
		}
	case webcam.CtrlTypeInteger:
		s = itoa(int(c.Value))
		if c.ID == 0x009a0902 { // unità di 100 µs
			s = num(float64(c.Value)/10, 1) + " ms"
		}
	default:
		s = itoa(int(c.Value))
	}
	return s
}

func (a *App) drawCtrls(img *image.RGBA, b image.Rectangle) {
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
		if c.Flags&webcam.CtrlFlagInactive != 0 {
			lc, vc = colFaint, colFaint
		}
		if a.changedCtrl(c.ID) {
			vc = colOK
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
