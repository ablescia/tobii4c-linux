package telemetry

import (
	"image"
	"image/color"
	"math"
	"time"

	"github.com/ablescia/tobii4c-linux/internal/gaze"
	"github.com/ablescia/tobii4c-linux/internal/ttp"
	"github.com/ablescia/tobii4c-linux/internal/x11ui"
)

var colPlotBg = x11ui.Hex(0x2e3135)

// --- sguardo sullo schermo ---------------------------------------------------

func (a *App) drawGaze(img *image.RGBA, b image.Rectangle) {
	aspect := 16.0 / 9
	si := &a.snap.Session
	if d := si.Display; d != nil && d.Height() > 0 {
		aspect = math.Max(1, math.Min(2.5, d.Width()/d.Height()))
	}
	legH := a.px(44)
	w := b.Dx()
	h := int(float64(w) / aspect)
	if h > b.Dy()-legH {
		h = b.Dy() - legH
		w = int(float64(h) * aspect)
	}
	r := image.Rect(b.Min.X, b.Min.Y, b.Min.X+w, b.Min.Y+h)
	x11ui.Fill(img, r, colPlotBg)
	for i := 1; i < 3; i++ {
		x := r.Min.X + r.Dx()*i/3
		y := r.Min.Y + r.Dy()*i/3
		x11ui.Fill(img, image.Rect(x, r.Min.Y, x+1, r.Max.Y), colLine)
		x11ui.Fill(img, image.Rect(r.Min.X, y, r.Max.X, y+1), colLine)
	}
	x11ui.Frame(img, r, 1, colKey)
	if d := si.Display; d != nil {
		x11ui.Text(img, a.f.small, r.Max.X-a.px(6), r.Max.Y-a.px(6), num(d.Width(), 0)+" × "+num(d.Height(), 0)+" mm", colFaint, x11ui.Right)
	}
	// appena fuori dallo schermo (entro il 5%) il punto resta sul bordo
	at := func(p [2]float32) (float64, float64) {
		x := math.Max(0, math.Min(1, float64(p[0])))
		y := math.Max(0, math.Min(1, float64(p[1])))
		return float64(r.Min.X) + x*float64(r.Dx()-1), float64(r.Min.Y) + y*float64(r.Dy()-1)
	}
	in := func(p [2]float32) bool { return p[0] >= -0.05 && p[0] <= 1.05 && p[1] >= -0.05 && p[1] <= 1.05 }
	clip := r
	// scia dello sguardo filtrato: ultimi 600 ms
	if n := len(a.snap.Hist); n > 1 {
		end := a.snap.Hist[n-1].HostNs
		var px, py float64
		have := false
		for _, hp := range a.snap.Hist {
			age := float64(end-hp.HostNs) / 1e9
			if age > 0.6 || !hp.GazeOK || !in(hp.Gaze) {
				have = false
				continue
			}
			x, y := at(hp.Gaze)
			if have {
				x11ui.ThinLine(img, clip, px, py, x, y, colText, 0.6*(1-age/0.6))
			}
			px, py, have = x, y, true
		}
	}
	s := &a.snap.Sample
	if a.snap.HaveSample {
		for k, e := range [2]*gaze.Eye{&s.Left, &s.Right} {
			if e.Gaze2DValid && in(e.Gaze2D) {
				x, y := at(e.Gaze2D)
				x11ui.Dot(img, clip, x, y, a.pf(3.5), colEyeCol[k], 1)
			}
		}
		if s.GazeRawValid && in(s.GazeRaw) {
			x, y := at(s.GazeRaw)
			x11ui.Ring(img, x, y, a.pf(7), a.pf(1.5), colDim)
		}
		if s.GazeValid && in(s.Gaze) {
			x, y := at(s.Gaze)
			x11ui.Circle(img, x, y, a.pf(6), colText)
			x11ui.Circle(img, x, y, a.pf(2), colBg)
		}
	}
	// legenda con i valori
	y := r.Max.Y + a.px(18)
	pt := func(p [2]float32, ok bool) string {
		if !ok {
			return "—"
		}
		return "(" + num(float64(p[0]), 3) + "; " + num(float64(p[1]), 3) + ")"
	}
	x := b.Min.X
	legend := func(c color.RGBA, ring bool, label, val string) {
		cx, cy := float64(x)+a.pf(5), float64(y)-a.pf(4)
		if ring {
			x11ui.Ring(img, cx, cy, a.pf(4.5), a.pf(1.4), c)
		} else {
			x11ui.Circle(img, cx, cy, a.pf(4), c)
		}
		t := x11ui.Text(img, a.f.small, x+a.px(14), y, label, colDim, x11ui.Left)
		t = x11ui.Text(img, a.f.monoS, t.Max.X+a.px(4), y, val, colText, x11ui.Left)
		x = t.Max.X + a.px(14)
	}
	legend(colText, false, "filtrato", pt(s.Gaze, a.snap.HaveSample && s.GazeValid))
	legend(colDim, true, "reattivo", pt(s.GazeRaw, a.snap.HaveSample && s.GazeRawValid))
	y += a.px(19)
	x = b.Min.X
	legend(colLeft, false, "sinistro", pt(s.Left.Gaze2D, a.snap.HaveSample && s.Left.Gaze2DValid))
	legend(colRight, false, "destro", pt(s.Right.Gaze2D, a.snap.HaveSample && s.Right.Gaze2DValid))
}

// --- track box ---------------------------------------------------------------

// defaultTrackBox è quello del 4C, se il firmware non risponde all'op 0x578.
var defaultTrackBox = ttp.TrackBox{Corners: [8][3]float64{
	{150, 100, 450}, {-150, 100, 450}, {-150, -100, 450}, {150, -100, 450},
	{275, 200, 900}, {-275, 200, 900}, {-275, -200, 900}, {275, -200, 900},
}}

func (a *App) drawTrackBox(img *image.RGBA, b image.Rectangle) {
	tb := defaultTrackBox
	if a.snap.Session.TrackBox != nil {
		tb = *a.snap.Session.TrackBox
	}
	g := a.px(12)
	textH := a.px(26)
	vw := (b.Dx() - g) / 2
	vh := b.Dy() - textH - a.px(16)
	top := image.Rect(b.Min.X, b.Min.Y+a.px(16), b.Min.X+vw, b.Min.Y+a.px(16)+vh)
	side := image.Rect(top.Max.X+g, top.Min.Y, top.Max.X+g+vw, top.Max.Y)
	x11ui.Text(img, a.f.small, top.Min.X, b.Min.Y+a.px(10), "dall'alto (x, z)", colFaint, x11ui.Left)
	x11ui.Text(img, a.f.small, side.Min.X, b.Min.Y+a.px(10), "di lato (z, y)", colFaint, x11ui.Left)
	for _, r := range []image.Rectangle{top, side} {
		x11ui.Fill(img, r, colPlotBg)
	}
	s := &a.snap.Sample
	eyes := [2]*gaze.Eye{&s.Left, &s.Right}
	var disp *ttp.DisplayArea = a.snap.Session.Display

	// vista dall'alto: x orizzontale (+x a destra dell'utente), z verso il basso
	far := tb.Far()
	xr := 0.0
	for _, c := range tb.Corners {
		xr = math.Max(xr, math.Abs(c[0]))
	}
	xr += 40
	z0, z1 := -80.0, far+60
	sc := math.Min(float64(top.Dx())/(2*xr), float64(top.Dy())/(z1-z0))
	ox := float64(top.Min.X) + float64(top.Dx())/2
	oy := float64(top.Min.Y) + (float64(top.Dy())-(z1-z0)*sc)/2 - z0*sc
	tp := func(x, z float64) (float64, float64) { return ox + x*sc, oy + z*sc }
	// piano vicino e lontano: (+x) (−x) agli estremi
	n0x, n0z := tp(tb.Corners[0][0], tb.Corners[0][2])
	n1x, _ := tp(tb.Corners[1][0], tb.Corners[1][2])
	f0x, f0z := tp(tb.Corners[4][0], tb.Corners[4][2])
	f1x, _ := tp(tb.Corners[5][0], tb.Corners[5][2])
	tx0, tz0 := tp(0, 0)
	x11ui.ThinLine(img, top, tx0, tz0, f0x, f0z, colKey, 0.5)
	x11ui.ThinLine(img, top, tx0, tz0, f1x, f0z, colKey, 0.5)
	poly := [][2]float64{{n0x, n0z}, {n1x, n0z}, {f1x, f0z}, {f0x, f0z}}
	for i := range poly {
		q := poly[(i+1)%4]
		x11ui.Line(img, poly[i][0], poly[i][1], q[0], q[1], a.pf(1.2), colKey)
	}
	if disp != nil {
		x0, y0 := tp(disp.TopLeft[0], disp.TopLeft[2])
		x1, y1 := tp(disp.TopRight[0], disp.TopRight[2])
		x11ui.Line(img, x0, y0, x1, y1, a.pf(2.5), colDim)
	}
	tx, ty := tp(0, 0)
	x11ui.RoundRect(img, image.Rect(int(tx-a.pf(14)), int(ty-a.pf(3)), int(tx+a.pf(14)), int(ty+a.pf(3))), a.pf(2), colText)

	// vista di lato: z orizzontale (tracker a sinistra), y in alto
	yr0, yr1 := -260.0, 420.0
	if disp != nil {
		yr1 = math.Max(yr1, disp.TopLeft[1]+40)
	}
	sc2 := math.Min(float64(side.Dx())/(z1-z0), float64(side.Dy())/(yr1-yr0))
	ox2 := float64(side.Min.X) + (float64(side.Dx())-(z1-z0)*sc2)/2 - z0*sc2
	oy2 := float64(side.Min.Y) + (float64(side.Dy())-(yr1-yr0)*sc2)/2 + yr1*sc2
	sp := func(z, y float64) (float64, float64) { return ox2 + z*sc2, oy2 - y*sc2 }
	a0x, a0y := sp(tb.Corners[0][2], tb.Corners[0][1])
	_, a1y := sp(tb.Corners[3][2], tb.Corners[3][1])
	b0x, b0y := sp(tb.Corners[4][2], tb.Corners[4][1])
	_, b1y := sp(tb.Corners[7][2], tb.Corners[7][1])
	s0x, s0y := sp(0, 0)
	x11ui.ThinLine(img, side, s0x, s0y, b0x, b0y, colKey, 0.5)
	x11ui.ThinLine(img, side, s0x, s0y, b0x, b1y, colKey, 0.5)
	poly = [][2]float64{{a0x, a0y}, {b0x, b0y}, {b0x, b1y}, {a0x, a1y}}
	for i := range poly {
		q := poly[(i+1)%4]
		x11ui.Line(img, poly[i][0], poly[i][1], q[0], q[1], a.pf(1.2), colKey)
	}
	if disp != nil {
		x0, y0 := sp(disp.BottomLeft[2], disp.BottomLeft[1])
		x1, y1 := sp(disp.TopLeft[2], disp.TopLeft[1])
		x11ui.Line(img, x0, y0, x1, y1, a.pf(2.5), colDim)
	}
	sx, sy := sp(0, 0)
	x11ui.RoundRect(img, image.Rect(int(sx-a.pf(3)), int(sy-a.pf(8)), int(sx+a.pf(3)), int(sy+a.pf(8))), a.pf(2), colText)

	// occhi (centro della cornea, sistema del tracker)
	for k, e := range eyes {
		if !a.snap.HaveSample || !e.OriginValid {
			continue
		}
		o := e.OriginMM
		x, y := tp(float64(o[0]), float64(o[2]))
		x11ui.Dot(img, top, x, y, a.pf(4), colEyeCol[k], 1)
		x, y = sp(float64(o[2]), float64(o[1]))
		x11ui.Dot(img, side, x, y, a.pf(4), colEyeCol[k], 1)
	}
	// riga di testo: distanza e posizione nel track box
	y := b.Max.Y - a.px(4)
	txt := "occhi non visti"
	c := colFaint
	if mid, ok := s.EyesTrackerMM(); a.snap.HaveSample && ok {
		d := math.Sqrt(float64(mid[0]*mid[0] + mid[1]*mid[1] + mid[2]*mid[2]))
		txt = "distanza dal tracker " + num(d, 0) + " mm"
		c = colText
		inBox := true
		for _, e := range eyes {
			for _, v := range e.TrackBox {
				if !e.TrackBoxValid || v < 0 || v > 1 {
					inBox = false
				}
			}
		}
		if inBox {
			txt += " · nel track box"
		} else {
			txt += " · fuori dal track box"
			c = colWarn
		}
	}
	x11ui.Text(img, a.f.small, b.Min.X, y, txt, c, x11ui.Left)
	nw, nh := tb.Size(false)
	fw, fh := tb.Size(true)
	x11ui.Text(img, a.f.small, b.Max.X, y, num(nw, 0)+"×"+num(nh, 0)+" → "+num(fw, 0)+"×"+num(fh, 0)+" mm", colFaint, x11ui.Right)
}

// --- grafici nel tempo -------------------------------------------------------

// series è una serie da disegnare in un grafico.
type series struct {
	c   color.RGBA
	get func(h *HistPoint) (float64, bool)
}

func (a *App) drawPlots(img *image.RGBA, b image.Rectangle) {
	g := a.px(10)
	ph := (b.Dy() - g) / 2
	r1 := image.Rect(b.Min.X, b.Min.Y, b.Max.X, b.Min.Y+ph)
	r2 := image.Rect(b.Min.X, r1.Max.Y+g, b.Max.X, b.Max.Y)
	last := func(get func(h *HistPoint) (float64, bool)) (float64, bool) {
		for i := len(a.snap.Hist) - 1; i >= 0 && i >= len(a.snap.Hist)-3; i-- {
			if v, ok := get(&a.snap.Hist[i]); ok {
				return v, true
			}
		}
		return 0, false
	}
	pupil := [2]series{
		{colLeft, func(h *HistPoint) (float64, bool) { return float64(h.Pupil[0]), h.PupilOK[0] }},
		{colRight, func(h *HistPoint) (float64, bool) { return float64(h.Pupil[1]), h.PupilOK[1] }},
	}
	open := [2]series{
		{colLeft, func(h *HistPoint) (float64, bool) { return float64(h.Open[0]), h.OpenOK[0] }},
		{colRight, func(h *HistPoint) (float64, bool) { return float64(h.Open[1]), h.OpenOK[1] }},
	}
	vals := func(ss [2]series) [2]string {
		var out [2]string
		for k, s := range ss {
			if v, ok := last(s.get); ok {
				out[k] = num(v, 2)
			} else {
				out[k] = "—"
			}
		}
		return out
	}
	a.plot(img, r1, "Diametro della pupilla, mm", vals(pupil), pupil[:], 1, 0.5)
	a.plot(img, r2, "Apertura della palpebra, mm", vals(open), open[:], 2, 1)
}

// plot disegna serie nel tempo (ultimi 10 s): per ogni colonna di pixel il
// tratto tra minimo e massimo dei campioni che vi cadono, così a 90 Hz non si
// perde nessun picco. Gli intervalli senza dati restano vuoti.
func (a *App) plot(img *image.RGBA, r image.Rectangle, title string, vals [2]string, ss []series, minSpan, step float64) {
	base := r.Min.Y + a.px(11)
	t := x11ui.Text(img, a.f.small, r.Min.X, base, title, colDim, x11ui.Left)
	x := r.Max.X
	for k := len(vals) - 1; k >= 0; k-- {
		tr := x11ui.Text(img, a.f.monoS, x, base, vals[k], colEyeCol[k], x11ui.Right)
		x = tr.Min.X - a.px(4)
		tr = x11ui.Text(img, a.f.small, x, base, [2]string{"sx", "dx"}[k], colFaint, x11ui.Right)
		x = tr.Min.X - a.px(12)
	}
	_ = t
	labW := a.px(30)
	pr := image.Rect(r.Min.X+labW, base+a.px(8), r.Max.X, r.Max.Y)
	x11ui.Fill(img, pr, colPlotBg)
	// scala verticale sui dati visibili
	lo, hi := math.Inf(1), math.Inf(-1)
	for i := range a.snap.Hist {
		for _, s := range ss {
			if v, ok := s.get(&a.snap.Hist[i]); ok && v >= 0 {
				lo, hi = math.Min(lo, v), math.Max(hi, v)
			}
		}
	}
	noData := math.IsInf(lo, 1)
	if noData {
		lo, hi = 0, minSpan*2
	}
	mid := (lo + hi) / 2
	span := math.Max(minSpan, (hi-lo)*1.2)
	lo, hi = math.Floor((mid-span/2)/step)*step, math.Ceil((mid+span/2)/step)*step
	lo = math.Max(lo, 0)
	yAt := func(v float64) float64 {
		return float64(pr.Max.Y-1) - (v-lo)/(hi-lo)*float64(pr.Dy()-2)
	}
	every := max(1, int(math.Ceil((hi-lo)/step/4)))
	for v := lo; v <= hi+1e-9; v += step {
		y := int(yAt(v))
		x11ui.Fill(img, image.Rect(pr.Min.X, y, pr.Max.X, y+1), colLine)
		if int(math.Round(v/step))%every == 0 {
			x11ui.Text(img, a.f.monoS, pr.Min.X-a.px(4), y+a.px(4), num(v, decimalsFor(step)), colFaint, x11ui.Right)
		}
	}
	// marcatori dei secondi
	now := time.Now().UnixNano()
	window := int64(10 * time.Second)
	for sec := int64(1); sec < 10; sec++ {
		xx := pr.Max.X - int(float64(pr.Dx())*float64(sec)/10)
		x11ui.Fill(img, image.Rect(xx, pr.Max.Y-a.px(4), xx+1, pr.Max.Y), colKey)
	}
	if noData {
		x11ui.Text(img, a.f.small, pr.Min.X+pr.Dx()/2, pr.Min.Y+pr.Dy()/2+a.px(4), "nessun valore valido negli ultimi 10 s", colFaint, x11ui.Center)
		return
	}
	w := pr.Dx()
	for _, s := range ss {
		prevCol, prevY := -10, 0.0
		colLo, colHi := math.Inf(1), math.Inf(-1)
		curCol := -1
		flush := func() {
			if curCol < 0 || math.IsInf(colLo, 1) {
				return
			}
			y0, y1 := yAt(colHi), yAt(colLo)
			if prevCol == curCol-1 { // unisce alla colonna precedente
				y0, y1 = math.Min(y0, prevY), math.Max(y1, prevY)
			}
			xx := pr.Min.X + curCol
			for y := int(math.Floor(y0)); y <= int(math.Ceil(y1)); y++ {
				x11ui.Blend(img, pr, xx, y, s.c, 1)
			}
			prevCol, prevY = curCol, yAt((colLo+colHi)/2)
		}
		for i := range a.snap.Hist {
			h := &a.snap.Hist[i]
			age := now - h.HostNs
			if age > window || age < 0 {
				continue
			}
			col := int(float64(w-1) * (1 - float64(age)/float64(window)))
			v, ok := s.get(h)
			if col != curCol {
				flush()
				curCol, colLo, colHi = col, math.Inf(1), math.Inf(-1)
			}
			if ok && v >= 0 {
				colLo, colHi = math.Min(colLo, v), math.Max(colHi, v)
			} else {
				prevCol = -10 // buco: non unire
			}
		}
		flush()
	}
}

func decimalsFor(step float64) int {
	if step >= 1 {
		return 0
	}
	return 1
}

// --- posa della testa --------------------------------------------------------

// gizmoPts sono i punti di una testa stilizzata nel suo sistema (x a destra,
// y in alto, il naso verso −z cioè verso lo schermo).
var gizmoOval, gizmoEyes = func() ([][3]float64, [2][][3]float64) {
	var oval [][3]float64
	for i := 0; i <= 36; i++ {
		t := float64(i) / 36 * 2 * math.Pi
		oval = append(oval, [3]float64{0.72 * math.Sin(t), 0.95 * math.Cos(t), 0})
	}
	// +x è la destra dell'utente: l'occhio sinistro (indice 0) sta a −x
	var eyes [2][][3]float64
	for k, cx := range [2]float64{-0.32, 0.32} {
		for i := 0; i <= 16; i++ {
			t := float64(i) / 16 * 2 * math.Pi
			eyes[k] = append(eyes[k], [3]float64{cx + 0.13*math.Cos(t), 0.22 + 0.06*math.Sin(t), -0.42})
		}
	}
	return oval, eyes
}()

// rotate applica pitch (attorno a x), yaw (y) e roll (z), destrorse.
func rotate(p [3]float64, pitch, yaw, roll float64) [3]float64 {
	sp, cp := math.Sincos(pitch)
	y, z := p[1]*cp-p[2]*sp, p[1]*sp+p[2]*cp
	p[1], p[2] = y, z
	sy, cy := math.Sincos(yaw)
	x, z2 := p[0]*cy+p[2]*sy, -p[0]*sy+p[2]*cy
	p[0], p[2] = x, z2
	sr, cr := math.Sincos(roll)
	x, y = p[0]*cr-p[1]*sr, p[0]*sr+p[1]*cr
	p[0], p[1] = x, y
	return p
}

// drawGizmo disegna la testa ruotata vista dallo schermo, a specchio come
// l'anteprima della camera (la destra dell'utente a destra).
func (a *App) drawGizmo(img *image.RGBA, r image.Rectangle, pitch, yaw, roll float64, ok [3]bool) {
	x11ui.Fill(img, r, colPlotBg)
	cx, cy := float64(r.Min.X)+float64(r.Dx())/2, float64(r.Min.Y)+float64(r.Dy())/2
	sc := float64(min(r.Dx(), r.Dy())) * 0.42
	x11ui.Fill(img, image.Rect(r.Min.X, int(cy), r.Max.X, int(cy)+1), colLine)
	x11ui.Fill(img, image.Rect(int(cx), r.Min.Y, int(cx)+1, r.Max.Y), colLine)
	if !ok[0] {
		pitch = 0
	}
	if !ok[1] {
		yaw = 0
	}
	if !ok[2] {
		roll = 0
	}
	proj := func(p [3]float64) (float64, float64, float64) {
		q := rotate(p, pitch, yaw, roll)
		return cx + q[0]*sc, cy - q[1]*sc, q[2]
	}
	c := colText
	if !ok[0] && !ok[1] && !ok[2] {
		c = colFaint
	}
	poly := func(pts [][3]float64, col color.RGBA, w float64) {
		for i := 1; i < len(pts); i++ {
			x0, y0, _ := proj(pts[i-1])
			x1, y1, _ := proj(pts[i])
			x11ui.Line(img, x0, y0, x1, y1, w, col)
		}
	}
	poly(gizmoOval, x11ui.Alpha(c, 0.75), a.pf(1.6))
	for k := range gizmoEyes {
		poly(gizmoEyes[k], colEyeCol[k], a.pf(1.6))
	}
	// naso: dalla radice alla punta, verso lo schermo
	x0, y0, _ := proj([3]float64{0, 0.1, -0.45})
	x1, y1, _ := proj([3]float64{0, -0.2, -0.95})
	x11ui.Line(img, x0, y0, x1, y1, a.pf(2.2), colWarn)
	x11ui.Circle(img, x1, y1, a.pf(3), colWarn)
	// bocca
	var mouth [][3]float64
	for i := 0; i <= 10; i++ {
		t := float64(i)/10*1.4 - 0.7
		mouth = append(mouth, [3]float64{0.3 * math.Sin(t), -0.5 - 0.06*math.Cos(t*2), -0.35})
	}
	poly(mouth, x11ui.Alpha(c, 0.75), a.pf(1.6))
}
