//go:build linux

package main

import (
	"image"
	"image/color"
	"math"
	"runtime"
	"sync"
	"time"

	"github.com/ablescia/tobii4c-linux/internal/bodypose"
	"github.com/ablescia/tobii4c-linux/internal/webcam"
	"github.com/ablescia/tobii4c-linux/internal/x11ui"
)

// ---------------------------------------------------------------------------
// Anteprima a colori

// Tabelle della conversione YCbCr → RGB (JPEG, gamma piena) in virgola
// fissa a 16 bit.
var crR, cbG, crG, cbB [256]int32

func init() {
	for i := range crR {
		c := int32(i - 128)
		crR[i] = 91881 * c
		cbG[i] = -22554 * c
		crG[i] = -46802 * c
		cbB[i] = 116130 * c
	}
}

func clampByte(v int32) byte {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return byte(v)
}

// previewRenderer ingrandisce il frame nel riquadro: luminanza bilineare,
// crominanza del pixel sorgente più vicino, la maschera della persona
// sfumata sopra. Le righe sono divise tra più goroutine.
type previewRenderer struct {
	x0, x1, xw, y0, y1, yw []int32
	cx, cy                 []int32   // pixel della crominanza
	fx, fy                 []float32 // coordinate sorgente (per la maschera)
	w, h, srcW, srcH       int
	cw, ch                 int
	mirror                 bool
	workers                int
}

func (t *previewRenderer) prepare(w, h int, f *webcam.Frame, mirror bool) {
	if t.x0 != nil && t.w == w && t.h == h && t.srcW == f.W && t.srcH == f.H && t.cw == f.CW && t.ch == f.CH && t.mirror == mirror {
		return
	}
	t.x0, t.x1, t.xw, t.fx = axisTable(t.x0[:0], t.x1[:0], t.xw[:0], t.fx[:0], w, f.W, mirror)
	t.y0, t.y1, t.yw, t.fy = axisTable(t.y0[:0], t.y1[:0], t.yw[:0], t.fy[:0], h, f.H, false)
	t.cx, t.cy = t.cx[:0], t.cy[:0]
	for x := 0; x < w; x++ {
		s := t.x0[x]
		if t.xw[x] >= 128 {
			s = t.x1[x]
		}
		t.cx = append(t.cx, int32(int(s)*max(f.CW, 1)/f.W))
	}
	for y := 0; y < h; y++ {
		s := t.y0[y]
		if t.yw[y] >= 128 {
			s = t.y1[y]
		}
		t.cy = append(t.cy, int32(int(s)*max(f.CH, 1)/f.H))
	}
	t.w, t.h, t.srcW, t.srcH, t.cw, t.ch, t.mirror = w, h, f.W, f.H, f.CW, f.CH, mirror
	if t.workers == 0 {
		t.workers = min(max(runtime.NumCPU()/4, 1), 6)
	}
}

func axisTable(i0, i1, wt []int32, fs []float32, n, src int, mirror bool) ([]int32, []int32, []int32, []float32) {
	for i := 0; i < n; i++ {
		f := (float64(i)+0.5)*float64(src)/float64(n) - 0.5
		if mirror {
			f = float64(src-1) - f
		}
		f = math.Max(0, math.Min(float64(src-1), f))
		a := int(f)
		i0 = append(i0, int32(a))
		i1 = append(i1, int32(min(a+1, src-1)))
		wt = append(wt, int32((f-float64(a))*256))
		fs = append(fs, float32(f))
	}
	return i0, i1, wt, fs
}

// maskOverlay è la maschera da sfumare sopra l'anteprima.
type maskOverlay struct {
	on    bool
	roi   bodypose.ROI
	m     []float32
	mw    int
	mh    int
	alpha float32
}

func (mo *maskOverlay) at(sx, sy float32) float32 {
	u, v := mo.roi.FromImage(float64(sx), float64(sy))
	fx, fy := u*float64(mo.mw)-0.5, v*float64(mo.mh)-0.5
	if fx < -1 || fy < -1 || fx > float64(mo.mw) || fy > float64(mo.mh) {
		return 0
	}
	x0, y0 := int(math.Floor(fx)), int(math.Floor(fy))
	dx, dy := float32(fx-float64(x0)), float32(fy-float64(y0))
	get := func(x, y int) float32 {
		if x < 0 || y < 0 || x >= mo.mw || y >= mo.mh {
			return 0
		}
		return mo.m[y*mo.mw+x]
	}
	return get(x0, y0)*(1-dx)*(1-dy) + get(x0+1, y0)*dx*(1-dy) + get(x0, y0+1)*(1-dx)*dy + get(x0+1, y0+1)*dx*dy
}

func (t *previewRenderer) render(img *image.RGBA, r image.Rectangle, f *webcam.Frame, mo *maskOverlay) {
	w, h := r.Dx(), r.Dy()
	if w <= 0 || h <= 0 {
		return
	}
	var wg sync.WaitGroup
	n := min(t.workers, h)
	for k := 0; k < n; k++ {
		wg.Add(1)
		go func(y0, y1 int) {
			defer wg.Done()
			t.rows(img, r, f, mo, y0, y1)
		}(k*h/n, (k+1)*h/n)
	}
	wg.Wait()
}

func (t *previewRenderer) rows(img *image.RGBA, r image.Rectangle, f *webcam.Frame, mo *maskOverlay, ya, yb int) {
	w := r.Dx()
	W := f.W
	color := f.Color()
	mr, mg, mb := float32(colMask.R), float32(colMask.G), float32(colMask.B)
	for y := ya; y < yb; y++ {
		r0 := f.Y[int(t.y0[y])*W : int(t.y0[y]+1)*W]
		r1 := f.Y[int(t.y1[y])*W : int(t.y1[y]+1)*W]
		wy := t.yw[y]
		var cbRow, crRow []byte
		if color {
			c := int(t.cy[y]) * f.CW
			cbRow, crRow = f.Cb[c:c+f.CW], f.Cr[c:c+f.CW]
		}
		o := img.PixOffset(r.Min.X, r.Min.Y+y)
		row := img.Pix[o : o+4*w]
		for x := 0; x < w; x++ {
			i0, i1, wx := t.x0[x], t.x1[x], t.xw[x]
			top := int32(r0[i0])*(256-wx) + int32(r0[i1])*wx
			bot := int32(r1[i0])*(256-wx) + int32(r1[i1])*wx
			yv := (top*(256-wy) + bot*wy) >> 16
			p := row[4*x : 4*x+4 : 4*x+4]
			if color {
				cb, cr := cbRow[t.cx[x]], crRow[t.cx[x]]
				y16 := yv << 16
				p[0] = clampByte((y16 + crR[cr] + 32768) >> 16)
				p[1] = clampByte((y16 + cbG[cb] + crG[cr] + 32768) >> 16)
				p[2] = clampByte((y16 + cbB[cb] + 32768) >> 16)
			} else {
				p[0], p[1], p[2] = byte(yv), byte(yv), byte(yv)
			}
			p[3] = 255
			if mo != nil && mo.on {
				if m := mo.at(t.fx[x], t.fy[y]) * mo.alpha; m > 0.004 {
					p[0] = byte(float32(p[0])*(1-m) + mr*m)
					p[1] = byte(float32(p[1])*(1-m) + mg*m)
					p[2] = byte(float32(p[2])*(1-m) + mb*m)
				}
			}
		}
	}
}

func (a *App) previewNote() string {
	in := &a.Eng.Info
	s := itoa(in.W) + "×" + itoa(in.H) + " " + webcam.FourCCString(in.PixFmt)
	if a.snap.Pix.Color() {
		s += " · crominanza " + itoa(a.snap.Pix.CW) + "×" + itoa(a.snap.Pix.CH)
	}
	if a.rates.have {
		s += " · anteprima " + num(a.rates.PreviewHz, 1) + " Hz"
	}
	if a.mirror {
		s += " · a specchio"
	}
	return s
}

// previewRect è il riquadro dell'immagine nel corpo b, nel rapporto della
// webcam; sotto resta la legenda.
func (a *App) previewRect(b image.Rectangle) image.Rectangle {
	legendH := a.px(22)
	area := image.Rect(b.Min.X, b.Min.Y, b.Max.X, b.Max.Y-legendH)
	sw, sh := a.Eng.Info.W, a.Eng.Info.H
	if a.snap.Pix.W > 0 {
		sw, sh = a.snap.Pix.W, a.snap.Pix.H
	}
	if sw <= 0 || sh <= 0 {
		sw, sh = 16, 9
	}
	w := area.Dx()
	h := w * sh / sw
	if h > area.Dy() {
		h = max(0, area.Dy())
		w = h * sw / sh
	}
	x := area.Min.X + (area.Dx()-w)/2
	return image.Rect(x, area.Min.Y, x+w, area.Min.Y+h)
}

func (a *App) drawPreview(img *image.RGBA, b image.Rectangle) {
	r := a.previewRect(b)
	f := &a.snap.Pix
	if f.W == 0 || len(f.Y) < f.W*f.H || r.Empty() {
		x11ui.Fill(img, r, x11ui.Hex(0x202326))
		msg := "In attesa della webcam…"
		if a.snap.Err != "" {
			msg = a.snap.Err
		}
		x11ui.Text(img, a.f.label, r.Min.X+r.Dx()/2, r.Min.Y+r.Dy()/2, fit(a.f.label, msg, r.Dx()-a.px(20)), colDim, x11ui.Center)
		a.drawLegend(img, b)
		return
	}
	p := &a.snap.Res.Pose
	mo := &maskOverlay{}
	if a.snap.Overlay && p.Found && len(p.Mask) == p.MaskW*p.MaskH && p.MaskW > 0 &&
		(a.drawMode == drawSkeletonMask || a.drawMode == drawMask) {
		*mo = maskOverlay{on: true, roi: p.ROI, m: p.Mask, mw: p.MaskW, mh: p.MaskH, alpha: 0.45}
	}
	a.pv.prepare(r.Dx(), r.Dy(), f, a.mirror)
	a.pv.render(img, r, f, mo)
	a.drawOverlay(img, r)
	a.drawLegend(img, b)
}

// toPreview porta un pixel dell'immagine nel riquadro dell'anteprima.
func (a *App) toPreview(r image.Rectangle, x, y float64) (float64, float64) {
	W, H := float64(a.snap.Pix.W), float64(a.snap.Pix.H)
	if a.mirror {
		x = W - x
	}
	return float64(r.Min.X) + x*float64(r.Dx())/W, float64(r.Min.Y) + y*float64(r.Dy())/H
}

// boneColor è il colore di un segmento: lato del soggetto, busto o viso.
func boneColor(b bodypose.Bone) color.RGBA {
	switch b.Part() {
	case bodypose.PartFace:
		return colFace
	case bodypose.PartTorso:
		return colTorso
	case -1:
		return colLeft
	}
	return colRight
}

func pointColor(i int) color.RGBA {
	switch bodypose.Side(i) {
	case -1:
		return colLeft
	case 1:
		return colRight
	}
	return colFace
}

// drawOverlay disegna ritaglio, riquadro del detector, scheletro e stato,
// se sono quelli del frame mostrato.
func (a *App) drawOverlay(img *image.RGBA, r image.Rectangle) {
	res := &a.snap.Res
	p := &res.Pose
	ov := a.snap.Overlay
	tp := func(x, y float64) (float64, float64) { return a.toPreview(r, x, y) }
	if ov && (p.Found || p.DetRan) && a.drawMode != drawNone && p.ROI.W > 0 {
		cs := p.ROI.Corners()
		for k := 0; k < 4; k++ {
			x0, y0 := tp(cs[k][0], cs[k][1])
			x1, y1 := tp(cs[(k+1)%4][0], cs[(k+1)%4][1])
			x11ui.ThinLine(img, r, x0, y0, x1, y1, colCrop, 0.85)
		}
	}
	if ov && p.DetRan && p.DetCount > 0 && a.drawMode != drawNone {
		bx := p.Det.Box
		var pts [4][2]float64
		for k, c := range [4][2]float64{{bx[0], bx[1]}, {bx[2], bx[1]}, {bx[2], bx[3]}, {bx[0], bx[3]}} {
			pts[k][0], pts[k][1] = tp(c[0], c[1])
		}
		for k := 0; k < 4; k++ {
			q := pts[(k+1)%4]
			x11ui.ThinLine(img, r, pts[k][0], pts[k][1], q[0], q[1], colDet, 0.9)
		}
	}
	if ov && p.Found && (a.drawMode == drawSkeleton || a.drawMode == drawSkeletonMask) {
		lw := math.Max(1.5, a.pf(3))
		for _, b := range bodypose.Bones {
			if b[0] >= p.N || b[1] >= p.N {
				continue
			}
			x0, y0 := tp(p.LM[b[0]].X, p.LM[b[0]].Y)
			x1, y1 := tp(p.LM[b[1]].X, p.LM[b[1]].Y)
			if p.Visible(b[0]) && p.Visible(b[1]) {
				thickLine(img, r, x0, y0, x1, y1, lw, boneColor(b), 1)
			} else {
				x11ui.ThinLine(img, r, x0, y0, x1, y1, boneColor(b), 0.3)
			}
		}
		dotR := math.Max(2, a.pf(3.6))
		for i := 0; i < p.N; i++ {
			x, y := tp(p.LM[i].X, p.LM[i].Y)
			c := pointColor(i)
			if p.Visible(i) {
				x11ui.Dot(img, r, x, y, dotR+a.pf(1.2), x11ui.Hex(0x202326), 1)
				x11ui.Dot(img, r, x, y, dotR, c, 1)
			} else {
				x11ui.Dot(img, r, x, y, dotR*0.7, c, 0.35)
			}
		}
	}
	// postura in alto a sinistra
	if ov && p.Found && a.drawMode != drawNone {
		s := bodypose.PostureNames[res.M.Posture]
		tw := a.f.big.Measure(s)
		box := image.Rect(r.Min.X+a.px(8), r.Min.Y+a.px(8), r.Min.X+a.px(24)+tw, r.Min.Y+a.px(40))
		x11ui.RoundRect(img, box, a.pf(5), x11ui.Alpha(x11ui.Hex(0x202326), 0.8))
		x11ui.Text(img, a.f.big, box.Min.X+a.px(8), box.Max.Y-a.px(9), s, colText, x11ui.Left)
	}
	// stato in basso a destra
	where := "CPU"
	if res.Cfg.GPU {
		where = "GPU " + num(res.DetGPU+res.LmGPU, 2) + " ms"
	}
	s, c := "persona "+num(p.Score, 3)+" · "+num(res.TotalMs, 1)+" ms · "+where, colOK
	switch {
	case a.snap.Loading != "":
		s, c = a.snap.Loading, colWarn
	case !ov:
		s, c = "in attesa del modello…", colWarn
	case !p.Found && p.DetRan && p.DetCount == 0:
		s, c = "nessuna persona trovata dal detector", colWarn
	case !p.Found:
		s, c = "persona persa (presenza "+num(p.Score, 2)+")", colWarn
	}
	tw := a.f.small.Measure(s)
	box := image.Rect(r.Max.X-tw-a.px(22), r.Max.Y-a.px(28), r.Max.X-a.px(8), r.Max.Y-a.px(8))
	x11ui.RoundRect(img, box, a.pf(4), x11ui.Alpha(x11ui.Hex(0x202326), 0.8))
	x11ui.Text(img, a.f.small, box.Min.X+a.px(7), box.Max.Y-a.px(6), s, c, x11ui.Left)
	if a.paused {
		ps := "IN PAUSA"
		box := image.Rect(r.Max.X-a.px(22)-a.f.small.Measure(ps), r.Min.Y+a.px(8), r.Max.X-a.px(8), r.Min.Y+a.px(28))
		x11ui.RoundRect(img, box, a.pf(4), x11ui.Alpha(x11ui.Hex(0x202326), 0.8))
		x11ui.Text(img, a.f.small, box.Min.X+a.px(7), box.Max.Y-a.px(6), ps, colWarn, x11ui.Left)
	}
}

func (a *App) drawLegend(img *image.RGBA, b image.Rectangle) {
	y := b.Max.Y - a.px(4)
	x := b.Min.X
	item := func(c color.RGBA, line bool, label string) {
		cx, cy := float64(x)+a.pf(5), float64(y)-a.pf(4)
		if line {
			x11ui.Line(img, cx-a.pf(6), cy, cx+a.pf(6), cy, a.pf(1.6), c)
		} else {
			x11ui.Circle(img, cx, cy, a.pf(3.5), c)
		}
		t := x11ui.Text(img, a.f.small, x+a.px(15), y, label, colDim, x11ui.Left)
		x = t.Max.X + a.px(14)
	}
	n := "33 punti"
	if a.snap.Cur.Mode == bodypose.UpperBody {
		n = "25 punti"
	}
	item(colLeft, false, "lato sx del soggetto")
	item(colRight, false, "lato dx")
	item(colTorso, true, "busto")
	item(colFace, false, "viso · "+n)
	item(colMask, false, "maschera")
	item(colCrop, true, "ritaglio della rete")
	item(colDet, true, "riquadro del detector")
}

// ---------------------------------------------------------------------------
// Grafici nel tempo

type series struct {
	name string
	c    color.RGBA
	get  func(h *HistPoint) (float64, bool)
}

const plotWindow = 10 * time.Second

func valid(v float64) (float64, bool) { return v, !math.IsNaN(v) }

func (a *App) drawPlots(img *image.RGBA, b image.Rectangle) {
	g := a.px(18)
	w := (b.Dx() - 2*g) / 3
	r1 := image.Rect(b.Min.X, b.Min.Y, b.Min.X+w, b.Max.Y)
	r2 := image.Rect(r1.Max.X+g, b.Min.Y, r1.Max.X+g+w, b.Max.Y)
	r3 := image.Rect(r2.Max.X+g, b.Min.Y, b.Max.X, b.Max.Y)
	joint := func(name string, j int, c color.RGBA) series {
		return series{name, c, func(h *HistPoint) (float64, bool) { return valid(h.Joints[j]) }}
	}
	var angles []series
	title := "Gomiti e ginocchia, °"
	if a.snap.Cur.Mode == bodypose.UpperBody {
		title = "Gomiti e spalle, °"
		angles = []series{joint("gom. sx", bodypose.JElbowL, colLeft), joint("dx", bodypose.JElbowR, colRight),
			joint("spalla sx", bodypose.JShoulderL, colAccent), joint("dx", bodypose.JShoulderR, colWarn)}
	} else {
		angles = []series{joint("gom. sx", bodypose.JElbowL, colLeft), joint("dx", bodypose.JElbowR, colRight),
			joint("gin. sx", bodypose.JKneeL, colAccent), joint("dx", bodypose.JKneeR, colWarn)}
	}
	trunk := []series{
		{"lato", colAccent, func(h *HistPoint) (float64, bool) { return valid(h.Lean) }},
		{"spalle", colWarn, func(h *HistPoint) (float64, bool) { return valid(h.Tilt) }},
		{"avanti", colDet, func(h *HistPoint) (float64, bool) { return valid(h.Forward) }},
	}
	tm := []series{
		{"totale", colText, func(h *HistPoint) (float64, bool) { return h.TotalMs, h.TotalMs > 0 }},
		{"GPU", colOK, func(h *HistPoint) (float64, bool) { return h.GPUMs, h.GPUMs > 0 }},
	}
	a.plot(img, r1, title, angles, 20, 0, false)
	a.plot(img, r2, "Busto: inclinazioni, °", trunk, 10, 1, false)
	a.plot(img, r3, "Tempo dei modelli per frame, ms", tm, 4, 1, true)
}

// plot disegna le serie degli ultimi 10 s con la scala sui dati visibili;
// i buchi (persona assente) restano vuoti. Con positive la scala parte da 0.
func (a *App) plot(img *image.RGBA, r image.Rectangle, title string, ss []series, minSpan float64, dec int, positive bool) {
	base := r.Min.Y + a.px(11)
	hist := a.snap.Hist
	x := r.Max.X
	for k := len(ss) - 1; k >= 0; k-- {
		s := ss[k]
		txt := "—"
		if n := len(hist); n > 0 {
			if v, ok := s.get(&hist[n-1]); ok {
				txt = num(v, dec)
			}
		}
		tr := x11ui.Text(img, a.f.monoS, x, base, txt, s.c, x11ui.Right)
		x = tr.Min.X - a.px(3)
		if s.name != "" {
			tr = x11ui.Text(img, a.f.small, x, base, s.name, colFaint, x11ui.Right)
			x = tr.Min.X - a.px(9)
		}
	}
	x11ui.Text(img, a.f.small, r.Min.X, base, fit(a.f.small, title, max(0, x-r.Min.X-a.px(4))), colDim, x11ui.Left)
	pr := image.Rect(r.Min.X+a.px(36), base+a.px(8), r.Max.X, r.Max.Y)
	if pr.Dy() < a.px(24) || pr.Dx() < a.px(40) {
		return
	}
	x11ui.Fill(img, pr, colPlotBg)
	end := a.now
	lo, hi := math.Inf(1), math.Inf(-1)
	for i := range hist {
		h := &hist[i]
		if end.Sub(h.T) > plotWindow {
			continue
		}
		for _, s := range ss {
			if v, ok := s.get(h); ok {
				lo, hi = math.Min(lo, v), math.Max(hi, v)
			}
		}
	}
	noData := math.IsInf(lo, 1)
	if noData {
		lo, hi = 0, minSpan
	}
	mid := (lo + hi) / 2
	span := math.Max(minSpan, (hi-lo)*1.25)
	step := niceStep(span / 4)
	lo = math.Floor((mid-span/2)/step) * step
	hi = math.Ceil((mid+span/2)/step) * step
	if positive && lo < 0 {
		hi, lo = hi-lo, 0
	}
	yAt := func(v float64) float64 {
		return float64(pr.Max.Y-1) - (v-lo)/(hi-lo)*float64(pr.Dy()-2)
	}
	lines := int(math.Round((hi - lo) / step))
	every := max(1, int(math.Ceil(float64(lines)*float64(a.px(14))/float64(pr.Dy()))))
	for i := 0; i <= lines; i++ {
		v := lo + float64(i)*step
		y := int(math.Round(yAt(v)))
		c := colLine
		if math.Abs(v) < step*1e-6 && lo < 0 {
			c = colKey
		}
		x11ui.Fill(img, image.Rect(pr.Min.X, y, pr.Max.X, y+1), c)
		if i%every == 0 {
			x11ui.Text(img, a.f.monoS, pr.Min.X-a.px(4), y+a.px(4), num(v, decimalsFor(step)), colFaint, x11ui.Right)
		}
	}
	for sec := 1; sec < 10; sec++ {
		xx := pr.Max.X - pr.Dx()*sec/10
		x11ui.Fill(img, image.Rect(xx, pr.Max.Y-a.px(4), xx+1, pr.Max.Y), colKey)
	}
	if noData {
		x11ui.Text(img, a.f.small, pr.Min.X+pr.Dx()/2, pr.Min.Y+pr.Dy()/2+a.px(4), "nessun valore negli ultimi 10 s", colFaint, x11ui.Center)
		return
	}
	xAt := func(t time.Time) float64 {
		return float64(pr.Max.X-1) - float64(end.Sub(t))/float64(plotWindow)*float64(pr.Dx()-1)
	}
	for _, s := range ss {
		prev := false
		var px, py float64
		var pt time.Time
		for i := range hist {
			h := &hist[i]
			v, ok := s.get(h)
			if !ok || end.Sub(h.T) > plotWindow {
				prev = false
				continue
			}
			x, y := xAt(h.T), yAt(v)
			if prev && h.T.Sub(pt) < 250*time.Millisecond {
				x11ui.ThinLine(img, pr, px, py, x, y, s.c, 1)
			} else {
				x11ui.Dot(img, pr, x, y, 1, s.c, 1)
			}
			px, py, pt, prev = x, y, h.T, true
		}
	}
}

// niceStep è il passo "tondo" (1, 2, 5 × 10ⁿ) più piccolo ≥ x.
func niceStep(x float64) float64 {
	if !(x > 0) {
		return 1
	}
	p := math.Pow(10, math.Floor(math.Log10(x)))
	for _, m := range []float64{1, 2, 5} {
		if m*p >= x*(1-1e-9) {
			return m * p
		}
	}
	return 10 * p
}

func decimalsFor(step float64) int {
	d := 0
	for step < 0.999 && d < 4 {
		step *= 10
		d++
	}
	return d
}

// ---------------------------------------------------------------------------
// Scheletro 3D

// drawSkeleton3D disegna le coordinate 3D (metri, origine tra i fianchi)
// proiettate sul piano scelto: di fronte (x, y) o di lato (z, y, con la
// camera a sinistra).
func (a *App) drawSkeleton3D(img *image.RGBA, r image.Rectangle, side bool, p *bodypose.Pose) {
	x11ui.Fill(img, r, colPlotBg)
	const span = 2.2 // metri visibili in altezza
	s := float64(r.Dy()) / span
	cx := float64(r.Min.X) + float64(r.Dx())/2
	cy := float64(r.Min.Y) + float64(r.Dy())*0.45
	// griglia ogni 0,5 m
	for k := -4; k <= 4; k++ {
		y := int(cy + float64(k)*0.5*s)
		if y > r.Min.Y && y < r.Max.Y {
			x11ui.Fill(img, image.Rect(r.Min.X, y, r.Max.X, y+1), colLine)
		}
		x := int(cx + float64(k)*0.5*s)
		if x > r.Min.X && x < r.Max.X {
			x11ui.Fill(img, image.Rect(x, r.Min.Y, x+1, r.Max.Y), colLine)
		}
	}
	proj := func(i int) (float64, float64) {
		w := p.World[i]
		h := w[0]
		if side {
			h = w[2]
		} else if a.mirror {
			h = -h
		}
		return cx + h*s, cy + w[1]*s
	}
	if side {
		x11ui.Text(img, a.f.small, r.Min.X+a.px(6), r.Min.Y+a.px(14), "camera da questa parte", colFaint, x11ui.Left)
	}
	lw := math.Max(1.2, a.pf(2.2))
	for _, b := range bodypose.Bones {
		if b[0] >= p.N || b[1] >= p.N {
			continue
		}
		x0, y0 := proj(b[0])
		x1, y1 := proj(b[1])
		if p.Visible(b[0]) && p.Visible(b[1]) {
			thickLine(img, r, x0, y0, x1, y1, lw, boneColor(b), 1)
		} else {
			x11ui.ThinLine(img, r, x0, y0, x1, y1, boneColor(b), 0.25)
		}
	}
	for i := 0; i < p.N; i++ {
		x, y := proj(i)
		if p.Visible(i) {
			x11ui.Dot(img, r, x, y, a.pf(2.4), pointColor(i), 1)
		}
	}
}

// thickLine disegna un segmento spesso w con le estremità arrotondate,
// antialias, limitato a clip: scorre l'asse principale e per ogni riga (o
// colonna) copre solo la fascia attorno al segmento. Nessuna allocazione,
// a differenza di x11ui.Line.
func thickLine(dst *image.RGBA, clip image.Rectangle, x0, y0, x1, y1, w float64, c color.RGBA, a float64) {
	hw := w / 2
	dx, dy := x1-x0, y1-y0
	l2 := dx*dx + dy*dy
	dist := func(px, py float64) float64 {
		t := 0.0
		if l2 > 0 {
			t = math.Max(0, math.Min(1, ((px-x0)*dx+(py-y0)*dy)/l2))
		}
		return math.Hypot(px-(x0+t*dx), py-(y0+t*dy))
	}
	cover := func(x, y int) {
		if cov := hw + 0.5 - dist(float64(x)+0.5, float64(y)+0.5); cov > 0 {
			x11ui.Blend(dst, clip, x, y, c, a*math.Min(1, cov))
		}
	}
	l := math.Sqrt(l2)
	if math.Abs(dy) > math.Abs(dx) {
		span := hw*l/math.Abs(dy) + 2
		ya := max(int(math.Floor(math.Min(y0, y1)-hw-1)), clip.Min.Y)
		yb := min(int(math.Ceil(math.Max(y0, y1)+hw+1)), clip.Max.Y)
		for y := ya; y <= yb; y++ {
			t := math.Max(0, math.Min(1, (float64(y)+0.5-y0)/dy))
			xc := x0 + t*dx
			for x := int(math.Floor(xc - span)); x <= int(math.Ceil(xc+span)); x++ {
				cover(x, y)
			}
		}
		return
	}
	span := hw + 2
	if l > 0 {
		span = hw*l/math.Abs(dx) + 2
	}
	xa := max(int(math.Floor(math.Min(x0, x1)-hw-1)), clip.Min.X)
	xb := min(int(math.Ceil(math.Max(x0, x1)+hw+1)), clip.Max.X)
	for x := xa; x <= xb; x++ {
		t := 0.0
		if dx != 0 {
			t = math.Max(0, math.Min(1, (float64(x)+0.5-x0)/dx))
		}
		yc := y0 + t*dy
		for y := int(math.Floor(yc - span)); y <= int(math.Ceil(yc+span)); y++ {
			cover(x, y)
		}
	}
}
