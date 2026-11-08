//go:build linux

package main

import (
	"image"
	"image/color"
	"math"
	"time"

	"github.com/ablescia/tobii4c-linux/internal/headpose"
	"github.com/ablescia/tobii4c-linux/internal/x11ui"
)

// ---------------------------------------------------------------------------
// Anteprima

// bilinear è la tabella del campionamento dell'anteprima: per ogni colonna
// e riga d'uscita i due pixel sorgente e il peso (0..256) del secondo.
type bilinear struct {
	x0, x1, xw, y0, y1, yw []int32
	w, h, srcW, srcH       int
	mirror                 bool
}

func (t *bilinear) prepare(w, h, srcW, srcH int, mirror bool) {
	if t.x0 != nil && t.w == w && t.h == h && t.srcW == srcW && t.srcH == srcH && t.mirror == mirror {
		return
	}
	t.x0, t.x1, t.xw = axisTable(t.x0[:0], t.x1[:0], t.xw[:0], w, srcW, mirror)
	t.y0, t.y1, t.yw = axisTable(t.y0[:0], t.y1[:0], t.yw[:0], h, srcH, false)
	t.w, t.h, t.srcW, t.srcH, t.mirror = w, h, srcW, srcH, mirror
}

func axisTable(i0, i1, wt []int32, n, src int, mirror bool) ([]int32, []int32, []int32) {
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
	}
	return i0, i1, wt
}

func (a *App) previewNote() string {
	in := &a.Eng.Info
	s := itoa(in.W) + "×" + itoa(in.H) + " " + fourccString(in.PixFmt)
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
	if a.snap.PixW > 0 {
		sw, sh = a.snap.PixW, a.snap.PixH
	}
	if sw <= 0 || sh <= 0 {
		sw, sh = 4, 3
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
	if len(a.snap.Pix) == 0 || r.Empty() {
		x11ui.Fill(img, r, x11ui.Hex(0x202326))
		msg := "In attesa della webcam…"
		if a.snap.Err != "" {
			msg = a.snap.Err
		}
		x11ui.Text(img, a.f.label, r.Min.X+r.Dx()/2, r.Min.Y+r.Dy()/2, fit(a.f.label, msg, r.Dx()-a.px(20)), colDim, x11ui.Center)
		a.drawLegend(img, b)
		return
	}
	a.renderGray(img, r)
	a.drawOverlay(img, r)
	a.drawLegend(img, b)
}

// renderGray ingrandisce la luminanza nel riquadro r: solo grigi, R = G = B.
func (a *App) renderGray(img *image.RGBA, r image.Rectangle) {
	t := &a.pvTable
	w, h := r.Dx(), r.Dy()
	t.prepare(w, h, a.snap.PixW, a.snap.PixH, a.mirror)
	pix := a.snap.Pix
	W := a.snap.PixW
	for y := 0; y < h; y++ {
		r0 := pix[int(t.y0[y])*W : int(t.y0[y]+1)*W]
		r1 := pix[int(t.y1[y])*W : int(t.y1[y]+1)*W]
		wy := t.yw[y]
		o := img.PixOffset(r.Min.X, r.Min.Y+y)
		row := img.Pix[o : o+4*w]
		for x := 0; x < w; x++ {
			i0, i1, wx := t.x0[x], t.x1[x], t.xw[x]
			top := int32(r0[i0])*(256-wx) + int32(r0[i1])*wx
			bot := int32(r1[i0])*(256-wx) + int32(r1[i1])*wx
			v := byte((top*(256-wy) + bot*wy) >> 16)
			p := row[4*x : 4*x+4 : 4*x+4]
			p[0], p[1], p[2], p[3] = v, v, v, 255
		}
	}
}

// drawOverlay disegna ritaglio del modello, punti del viso e iridi, se sono
// quelli del frame mostrato.
func (a *App) drawOverlay(img *image.RGBA, r image.Rectangle) {
	res := &a.snap.Res
	W, H := a.snap.PixW, a.snap.PixH
	sx, sy := float64(r.Dx())/float64(W), float64(r.Dy())/float64(H)
	ox, oy := float64(r.Min.X), float64(r.Min.Y)
	tx := func(x, y float64) (float64, float64) {
		if a.mirror {
			x = float64(W) - x
		}
		return ox + x*sx, oy + y*sy
	}
	ov := a.snap.Overlay
	if c := res.Crop; ov && c.Side > 0 {
		var pts [4][2]float64
		for k, uv := range [4][2]float64{{0, 0}, {1, 0}, {1, 1}, {0, 1}} {
			x, y := c.ToImage(uv[0]*headpose.ModelInput, uv[1]*headpose.ModelInput, headpose.ModelInput)
			pts[k][0], pts[k][1] = tx(x, y)
		}
		for k := 0; k < 4; k++ {
			q := pts[(k+1)%4]
			x11ui.ThinLine(img, r, pts[k][0], pts[k][1], q[0], q[1], colCrop, 0.85)
		}
	}
	if ov && res.Face && a.meshMode != meshNone {
		lm := &res.LM
		line := func(e headpose.MeshEdge, c color.RGBA, alpha float64) {
			x0, y0 := tx(lm[e[0]][0], lm[e[0]][1])
			x1, y1 := tx(lm[e[1]][0], lm[e[1]][1])
			x11ui.ThinLine(img, r, x0, y0, x1, y1, c, alpha)
		}
		switch a.meshMode {
		case meshTesselation:
			for _, e := range headpose.MeshTesselation {
				line(e, colMesh, 0.45)
			}
		case meshContours:
			for _, f := range headpose.MeshContours {
				c := colMesh
				switch f.Name {
				case "iride sinistra":
					c = colLeft
				case "iride destra":
					c = colRight
				}
				for _, e := range f.Edges {
					line(e, c, 0.9)
				}
			}
		}
		dotR := math.Max(1, a.pf(1.25))
		for i := 0; i < headpose.MeshPoints; i++ {
			x, y := tx(lm[i][0], lm[i][1])
			x11ui.Dot(img, r, x, y, dotR, colMesh, 1)
		}
		for k, c := range [2]int{headpose.LandmarkLeftIris, headpose.LandmarkRightIris} {
			for i := c; i < c+5; i++ {
				x, y := tx(lm[i][0], lm[i][1])
				rr := dotR
				if i == c {
					rr = math.Max(2, a.pf(2.6))
				}
				x11ui.Dot(img, r, x, y, rr, colEyeCol[k], 1)
			}
		}
	}
	// stato in basso a destra, come nella telemetria del driver
	s, c := "viso "+num(res.Score, 3)+" · "+num(res.InferMs, 1)+" ms", colOK
	switch {
	case !ov && a.snap.Searching:
		s, c = "ricerca del viso in corso…", colWarn
	case !ov:
		s, c = "in attesa del modello…", colWarn
	case !res.Face:
		s, c = "nessun viso · "+res.Mode, colWarn
	}
	tw := a.f.small.Measure(s)
	box := image.Rect(r.Max.X-tw-a.px(22), r.Max.Y-a.px(28), r.Max.X-a.px(8), r.Max.Y-a.px(8))
	x11ui.RoundRect(img, box, a.pf(4), x11ui.Alpha(x11ui.Hex(0x202326), 0.8))
	x11ui.Text(img, a.f.small, box.Min.X+a.px(7), box.Max.Y-a.px(6), s, c, x11ui.Left)
	if a.paused {
		p := "IN PAUSA"
		box := image.Rect(r.Min.X+a.px(8), r.Min.Y+a.px(8), r.Min.X+a.px(22)+a.f.small.Measure(p), r.Min.Y+a.px(28))
		x11ui.RoundRect(img, box, a.pf(4), x11ui.Alpha(x11ui.Hex(0x202326), 0.8))
		x11ui.Text(img, a.f.small, box.Min.X+a.px(7), box.Max.Y-a.px(6), p, colWarn, x11ui.Left)
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
			x11ui.Circle(img, cx, cy, a.pf(3), c)
		}
		t := x11ui.Text(img, a.f.small, x+a.px(15), y, label, colDim, x11ui.Left)
		x = t.Max.X + a.px(14)
	}
	n := "478 punti del viso"
	if a.meshMode == meshNone {
		n = "punti nascosti"
	}
	item(colMesh, false, n)
	item(colLeft, false, "iride sx")
	item(colRight, false, "iride dx")
	item(colCrop, true, "ritaglio del modello (256×256)")
}

// ---------------------------------------------------------------------------
// Grafici nel tempo

type series struct {
	name string
	c    color.RGBA
	get  func(h *HistPoint) (float64, bool)
}

const plotWindow = 10 * time.Second

func (a *App) drawPlots(img *image.RGBA, b image.Rectangle) {
	g := a.px(18)
	w := (b.Dx() - 2*g) / 3
	r1 := image.Rect(b.Min.X, b.Min.Y, b.Min.X+w, b.Max.Y)
	r2 := image.Rect(r1.Max.X+g, b.Min.Y, r1.Max.X+g+w, b.Max.Y)
	r3 := image.Rect(r2.Max.X+g, b.Min.Y, b.Max.X, b.Max.Y)
	pose := []series{
		{"pitch", colPitch, func(h *HistPoint) (float64, bool) { return h.Pitch, h.PoseOK }},
		{"yaw", colYaw, func(h *HistPoint) (float64, bool) { return h.Yaw, h.PoseOK }},
		{"roll", colRoll, func(h *HistPoint) (float64, bool) { return h.Roll, h.PoseOK }},
	}
	ear := []series{
		{"sx", colLeft, func(h *HistPoint) (float64, bool) { return h.EAR[0], h.Face }},
		{"dx", colRight, func(h *HistPoint) (float64, bool) { return h.EAR[1], h.Face }},
	}
	inf := []series{{"", colText, func(h *HistPoint) (float64, bool) { return h.InferMs, h.Ran }}}
	a.plot(img, r1, "Rotazione della testa, °", pose, 10, 1, false)
	a.plot(img, r2, "Apertura degli occhi (EAR)", ear, 0.2, 2, true)
	a.plot(img, r3, "Tempo del modello per frame, ms", inf, 10, 1, true)
}

// plot disegna le serie degli ultimi 10 s con la scala sui dati visibili;
// i buchi (viso assente) restano vuoti. Con positive la scala parte da 0.
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
			x = tr.Min.X - a.px(10)
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
	// griglia: etichette al più ogni 14 px logici
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
// Istogramma della luminanza

func (a *App) drawHist(img *image.RGBA, r image.Rectangle, st *ImageStats) {
	x11ui.Fill(img, r, colPlotBg)
	if r.Dx() < 64 || r.Dy() < 8 {
		return
	}
	var top float32
	for _, v := range st.Hist {
		top = max(top, v)
	}
	if top <= 0 {
		return
	}
	bw := float64(r.Dx()) / 64
	for i, v := range st.Hist {
		h := int(float32(r.Dy()-2) * v / top)
		x0 := r.Min.X + int(float64(i)*bw)
		x1 := r.Min.X + int(float64(i+1)*bw)
		if x1-x0 > 2 {
			x1--
		}
		// la barra ha il grigio che rappresenta, schiarito per restare
		// visibile sullo sfondo scuro
		g := byte(70 + i*4*(242-70)/255)
		x11ui.Fill(img, image.Rect(x0, r.Max.Y-1-h, x1, r.Max.Y-1), color.RGBA{g, g, g, 255})
	}
	mark := func(v int, c color.RGBA) {
		xx := r.Min.X + int(float64(v)/256*float64(r.Dx()))
		x11ui.Fill(img, image.Rect(xx, r.Min.Y, xx+1, r.Max.Y), c)
	}
	mark(st.P1, colWarn)
	mark(st.P99, colWarn)
	mark(int(st.Mean), colOK)
	if st.HaveFace {
		mark(int(st.FaceMean), colMesh)
	}
}

// ---------------------------------------------------------------------------
// Testa stilizzata

// Punti di una testa stilizzata nel suo sistema: x a destra del soggetto,
// y in alto, il naso verso −z (verso la camera).
var gizmoOval, gizmoEyes, gizmoMouth = func() ([][3]float64, [2][][3]float64, [][3]float64) {
	var oval [][3]float64
	for i := 0; i <= 36; i++ {
		t := float64(i) / 36 * 2 * math.Pi
		oval = append(oval, [3]float64{0.72 * math.Sin(t), 0.95 * math.Cos(t), 0})
	}
	// +x è la destra del soggetto: l'occhio sinistro (indice 0) sta a −x
	var eyes [2][][3]float64
	for k, cx := range [2]float64{-0.32, 0.32} {
		for i := 0; i <= 16; i++ {
			t := float64(i) / 16 * 2 * math.Pi
			eyes[k] = append(eyes[k], [3]float64{cx + 0.13*math.Cos(t), 0.22 + 0.06*math.Sin(t), -0.42})
		}
	}
	var mouth [][3]float64
	for i := 0; i <= 10; i++ {
		t := float64(i)/10*1.4 - 0.7
		mouth = append(mouth, [3]float64{0.3 * math.Sin(t), -0.5 - 0.06*math.Cos(t*2), -0.35})
	}
	return oval, eyes, mouth
}()

// rotate applica pitch (attorno a x), yaw (y) e roll (z), destrorse.
func rotate(p [3]float64, pitch, yaw, roll float64) [3]float64 {
	sp, cp := math.Sincos(pitch)
	p[1], p[2] = p[1]*cp-p[2]*sp, p[1]*sp+p[2]*cp
	sy, cy := math.Sincos(yaw)
	p[0], p[2] = p[0]*cy+p[2]*sy, -p[0]*sy+p[2]*cy
	sr, cr := math.Sincos(roll)
	p[0], p[1] = p[0]*cr-p[1]*sr, p[0]*sr+p[1]*cr
	return p
}

// drawGizmo disegna la testa ruotata vista dalla webcam: con l'anteprima a
// specchio la destra del soggetto sta a destra, come nella telemetria.
func (a *App) drawGizmo(img *image.RGBA, r image.Rectangle, pitch, yaw, roll float64, ok bool) {
	x11ui.Fill(img, r, colPlotBg)
	cx, cy := float64(r.Min.X)+float64(r.Dx())/2, float64(r.Min.Y)+float64(r.Dy())/2
	sc := float64(min(r.Dx(), r.Dy())) * 0.42
	x11ui.Fill(img, image.Rect(r.Min.X, int(cy), r.Max.X, int(cy)+1), colLine)
	x11ui.Fill(img, image.Rect(int(cx), r.Min.Y, int(cx)+1, r.Max.Y), colLine)
	c := colText
	if !ok {
		pitch, yaw, roll = 0, 0, 0
		c = colFaint
	}
	proj := func(p [3]float64) (float64, float64) {
		q := rotate(p, pitch, yaw, roll)
		if !a.mirror {
			q[0] = -q[0]
		}
		return cx + q[0]*sc, cy - q[1]*sc
	}
	poly := func(pts [][3]float64, col color.RGBA, w float64) {
		for i := 1; i < len(pts); i++ {
			x0, y0 := proj(pts[i-1])
			x1, y1 := proj(pts[i])
			x11ui.Line(img, x0, y0, x1, y1, w, col)
		}
	}
	poly(gizmoOval, x11ui.Alpha(c, 0.75), a.pf(1.6))
	for k := range gizmoEyes {
		col := colEyeCol[k]
		if !ok {
			col = colFaint
		}
		poly(gizmoEyes[k], col, a.pf(1.6))
	}
	x0, y0 := proj([3]float64{0, 0.1, -0.45})
	x1, y1 := proj([3]float64{0, -0.2, -0.95})
	nose := colWarn
	if !ok {
		nose = colFaint
	}
	x11ui.Line(img, x0, y0, x1, y1, a.pf(2.2), nose)
	x11ui.Circle(img, x1, y1, a.pf(3), nose)
	poly(gizmoMouth, x11ui.Alpha(c, 0.75), a.pf(1.6))
}
