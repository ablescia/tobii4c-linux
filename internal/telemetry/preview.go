package telemetry

import (
	"image"
	"image/color"
	"image/draw"
	"math"
	"time"

	"github.com/ablescia/tobii4c-linux/internal/gaze"
	"github.com/ablescia/tobii4c-linux/internal/headpose"
	"github.com/ablescia/tobii4c-linux/internal/x11ui"
)

// preview è l'anteprima della camera: l'ultima immagine elaborata dal
// modello del viso, ingrandita a specchio, con le sovrapposizioni già
// disegnate (si ridisegnano solo quando arriva un'immagine nuova).
type preview struct {
	p       headpose.Preview
	res     headpose.Result
	have    bool
	dirty   bool
	resized bool
	lastNew time.Time
	img     *image.RGBA
	rect    image.Rectangle // riquadro dell'immagine nel canvas
	lut     [256]byte
	// campionamento bilineare: per ogni colonna/riga d'uscita il pixel
	// sorgente e il peso (0..256) del successivo
	xs, ys []int32
	xw, yw []int32
	srcW   int
	srcH   int
}

func (a *App) cameraNote() string {
	im := a.snap.Image
	if !im.OK {
		if im.Err != "" {
			return "errore: " + im.Err
		}
		return "nessuna immagine"
	}
	return itoa(im.W) + "×" + itoa(im.H) + " · " + itoa(im.BPP) + " bit · " + num(a.rates.streamHz[0x501], 1) + " Hz · " +
		itoa(im.Bytes/1024) + " KiB"
}

// drawCamera disegna l'immagine e la legenda.
func (a *App) drawCamera(img *image.RGBA, b image.Rectangle) {
	legendH := a.px(22)
	area := image.Rect(b.Min.X, b.Min.Y, b.Max.X, b.Max.Y-legendH)
	w := area.Dx()
	h := w * 3 / 4
	if h > area.Dy() {
		h = area.Dy()
		w = h * 4 / 3
	}
	r := image.Rect(area.Min.X+(area.Dx()-w)/2, area.Min.Y, area.Min.X+(area.Dx()-w)/2+w, area.Min.Y+h)
	pv := &a.pv
	if r != pv.rect || pv.img == nil {
		pv.rect = r
		pv.img = image.NewRGBA(image.Rect(0, 0, w, h))
		pv.resized, pv.dirty = true, true
	}
	if !pv.have || pv.p.W == 0 {
		x11ui.Fill(img, r, x11ui.Hex(0x202326))
		msg := "In attesa delle immagini della camera…"
		switch {
		case a.Prompt != nil:
			msg = ""
		case a.Col.HP == nil:
			msg = "Anteprima disattivata: il modello del viso è spento (-headpose off)"
		}
		x11ui.Text(img, a.f.label, r.Min.X+r.Dx()/2, r.Min.Y+r.Dy()/2, msg, colDim, x11ui.Center)
		a.drawCameraLegend(img, b, legendH)
		return
	}
	if pv.dirty || pv.resized {
		a.renderPreview()
		pv.dirty, pv.resized = false, false
	}
	draw.Draw(img, r, pv.img, image.Point{}, draw.Src)
	// immagine vecchia: lo stream è fermo (utente assente, sospensione)
	if age := time.Since(pv.lastNew); age > 1500*time.Millisecond {
		s := "nessuna immagine nuova da " + fmtDuration(age)
		tw := a.f.label.Measure(s)
		box := image.Rect(r.Min.X+a.px(10), r.Min.Y+a.px(10), r.Min.X+a.px(26)+tw, r.Min.Y+a.px(36))
		x11ui.RoundRect(img, box, a.pf(5), x11ui.Hex(0x202326))
		x11ui.Text(img, a.f.label, box.Min.X+a.px(8), box.Max.Y-a.px(8), s, colWarn, x11ui.Left)
	}
	a.drawCameraLegend(img, b, legendH)
}

func (a *App) drawCameraLegend(img *image.RGBA, b image.Rectangle, h int) {
	y := b.Max.Y - a.px(6)
	x := b.Min.X
	item := func(c color.RGBA, shape int, label string) {
		cx, cy := float64(x)+a.pf(5), float64(y)-a.pf(4)
		switch shape {
		case 0:
			x11ui.Circle(img, cx, cy, a.pf(3), c)
		case 1:
			x11ui.Ring(img, cx, cy, a.pf(4.5), a.pf(1.6), c)
		case 2:
			x11ui.Line(img, cx-a.pf(6), cy, cx+a.pf(6), cy, a.pf(1.6), c)
		}
		t := x11ui.Text(img, a.f.small, x+a.px(15), y, label, colDim, x11ui.Left)
		x = t.Max.X + a.px(14)
	}
	n := "478 punti del viso"
	if a.meshMode == meshNone {
		n = "punti nascosti"
	}
	item(colMesh, 0, n)
	item(colLeft, 0, "iride sx")
	item(colRight, 0, "iride dx")
	item(colCrop, 2, "ritaglio del modello")
	item(x11ui.Hex(0xff5dff), 1, "cornee dal tracker")
	if _, ok := a.sensorPoints(); ok {
		item(colSensor, 1, "0x19/0x1a")
	}
}

// renderPreview ricostruisce l'immagine ingrandita con le sovrapposizioni.
func (a *App) renderPreview() {
	pv := &a.pv
	p := &pv.p
	dst := pv.img
	w, h := dst.Rect.Dx(), dst.Rect.Dy()
	// guadagno automatico (le immagini IR sono scure): 0,5% dei pixel saturi
	var hist [256]int
	for i := 0; i < len(p.Pix); i += 7 {
		hist[p.Pix[i]]++
	}
	total, acc, hi := len(p.Pix)/7, 0, 255
	for v := 255; v >= 0; v-- {
		acc += hist[v]
		if acc > total/200 {
			hi = v
			break
		}
	}
	hi = max(hi, 24)
	for v := range pv.lut {
		pv.lut[v] = byte(min(255, v*255/hi))
	}
	if pv.resized || pv.srcW != p.W || pv.srcH != p.H {
		pv.xs, pv.xw = bilinearTable(pv.xs, pv.xw, w, p.W, true)
		pv.ys, pv.yw = bilinearTable(pv.ys, pv.yw, h, p.H, false)
		pv.srcW, pv.srcH = p.W, p.H
	}
	for y := 0; y < h; y++ {
		sy, wy := int(pv.ys[y]), pv.yw[y]
		r0 := p.Pix[sy*p.W : (sy+1)*p.W]
		r1 := r0
		if sy+1 < p.H {
			r1 = p.Pix[(sy+1)*p.W : (sy+2)*p.W]
		}
		row := dst.Pix[y*dst.Stride : y*dst.Stride+4*w]
		for x := 0; x < w; x++ {
			sx, wx := int(pv.xs[x]), pv.xw[x]
			sx1 := sx - 1 // a specchio: il "successivo" sta a sinistra
			if sx1 < 0 {
				sx1 = 0
			}
			top := int32(r0[sx])*(256-wx) + int32(r0[sx1])*wx
			bot := int32(r1[sx])*(256-wx) + int32(r1[sx1])*wx
			v := pv.lut[(top*(256-wy)+bot*wy)>>16]
			o := 4 * x
			row[o], row[o+1], row[o+2], row[o+3] = v, v, v, 255
		}
	}
	clip := dst.Rect
	sx, sy := float64(w)/float64(p.W), float64(h)/float64(p.H)
	// a specchio: x_schermo = (W − x) · scala
	tx := func(x, y float64) (float64, float64) { return (float64(p.W) - x) * sx, y * sy }

	if p.Crop.Side > 0 {
		var pts [4][2]float64
		for k, uv := range [4][2]float64{{0, 0}, {1, 0}, {1, 1}, {0, 1}} {
			x, y := p.Crop.ToImage(uv[0]*headpose.ModelInput, uv[1]*headpose.ModelInput, headpose.ModelInput)
			pts[k][0], pts[k][1] = tx(x, y)
		}
		for k := 0; k < 4; k++ {
			q := pts[(k+1)%4]
			x11ui.ThinLine(dst, clip, pts[k][0], pts[k][1], q[0], q[1], colCrop, 0.85)
		}
	}
	if p.Face && a.meshMode != meshNone {
		lm := &p.LM
		line := func(e headpose.MeshEdge, c color.RGBA, alpha float64) {
			x0, y0 := tx(lm[e[0]][0], lm[e[0]][1])
			x1, y1 := tx(lm[e[1]][0], lm[e[1]][1])
			x11ui.ThinLine(dst, clip, x0, y0, x1, y1, c, alpha)
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
			x11ui.Dot(dst, clip, x, y, dotR, colMesh, 1)
		}
		for k, c := range [2]int{headpose.LandmarkLeftIris, headpose.LandmarkRightIris} {
			col := colEyeCol[k]
			for i := c; i < c+5; i++ {
				x, y := tx(lm[i][0], lm[i][1])
				r := dotR
				if i == c {
					r = math.Max(2, a.pf(2.6))
				}
				x11ui.Dot(dst, clip, x, y, r, col, 1)
			}
		}
	}
	// centri corneali del tracker proiettati con la camera calibrata
	if cam := a.snap.HPStatus.Camera; cam != nil && a.snap.HaveSample {
		for k, e := range [2]*gaze.Eye{&a.snap.Sample.Left, &a.snap.Sample.Right} {
			if !e.OriginValid {
				continue
			}
			o := e.OriginMM
			u, v, ok := cam.Project([3]float64{float64(o[0]), float64(o[1]), float64(o[2])})
			if !ok {
				continue
			}
			x, y := tx(u, v)
			x11ui.Ring(dst, x, y, a.pf(6), a.pf(1.6), x11ui.Hex(0xff5dff))
			_ = k
		}
	}
	// posizione degli occhi sul sensore (0x19/0x1a), se il firmware la riempie
	if pts, ok := a.sensorPoints(); ok {
		for k, q := range pts {
			if q[0] < 0 {
				continue
			}
			x, y := q[0]*float64(p.W), q[1]*float64(p.H)
			if p.Mirror {
				x = float64(p.W) - x
			}
			X, Y := tx(x, y)
			x11ui.Ring(dst, X, Y, a.pf(8), a.pf(1.4), colSensor)
			x11ui.Text(dst, a.f.small, int(X)+a.px(10), int(Y)+a.px(4), [2]string{"sx", "dx"}[k], colSensor, x11ui.Left)
		}
	}
	// riga di stato in basso nell'immagine
	if p.Face || p.Score > 0 {
		s := "viso " + num(p.Score, 3) + " · " + num(pv.res.InferenceMs, 1) + " ms"
		tw := a.f.small.Measure(s)
		box := image.Rect(w-tw-a.px(22), h-a.px(28), w-a.px(8), h-a.px(8))
		x11ui.RoundRect(dst, box, a.pf(4), x11ui.Alpha(x11ui.Hex(0x202326), 0.8))
		c := colOK
		if !p.Face {
			c = colWarn
		}
		x11ui.Text(dst, a.f.small, box.Min.X+a.px(7), box.Max.Y-a.px(6), s, c, x11ui.Left)
	}
}

// sensorPoints restituisce le posizioni degli occhi sul sensore (colonne
// 0x19/0x1a, normalizzate) se almeno una è nell'intervallo [0,1]: sull'ET5
// valgono −1 anche con l'utente tracciato.
func (a *App) sensorPoints() ([2][2]float64, bool) {
	out := [2][2]float64{{-1, -1}, {-1, -1}}
	if !a.snap.HaveRow {
		return out, false
	}
	ok := false
	for k, key := range [2]uint32{0x19, 0x1a} {
		v, found := a.snap.GazeRow.Get(key)
		if !found || v.N < 2 {
			continue
		}
		if v.F[0] >= 0 && v.F[0] <= 1 && v.F[1] >= 0 && v.F[1] <= 1 && (v.F[0] != 0 || v.F[1] != 0) {
			out[k] = [2]float64{v.F[0], v.F[1]}
			ok = true
		}
	}
	return out, ok
}

// bilinearTable prepara il campionamento di n pixel d'uscita da src pixel
// sorgente; con mirror la colonna d'uscita 0 corrisponde all'ultima sorgente.
func bilinearTable(idx, wt []int32, n, src int, mirror bool) ([]int32, []int32) {
	idx, wt = idx[:0], wt[:0]
	for i := 0; i < n; i++ {
		f := (float64(i)+0.5)*float64(src)/float64(n) - 0.5
		f = math.Max(0, math.Min(float64(src-1), f))
		i0 := int(f)
		w := int32((f - float64(i0)) * 256)
		if mirror {
			// sorgente letta da destra: indice speculare, il vicino è a sinistra
			m := float64(src-1) - f
			i0 = int(math.Ceil(m))
			w = int32((float64(i0) - m) * 256)
			if i0 > src-1 {
				i0 = src - 1
			}
		}
		idx = append(idx, int32(i0))
		wt = append(wt, w)
	}
	return idx, wt
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
