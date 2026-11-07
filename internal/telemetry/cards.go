package telemetry

import (
	"fmt"
	"image"
	"image/color"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ablescia/tobii4c-linux/internal/gaze"
	"github.com/ablescia/tobii4c-linux/internal/headpose"
	"github.com/ablescia/tobii4c-linux/internal/ttp"
	"github.com/ablescia/tobii4c-linux/internal/x11ui"
)

// kv disegna una riga etichetta → valore; restituisce la y della riga dopo.
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

// wrap disegna un valore lungo su più righe.
func (k *kvList) wrap(label, value string, c color.RGBA, maxLines int) {
	a := k.a
	lines := wrap(a.f.small, value, k.b.Dx()-k.labelW)
	if len(lines) == 0 {
		lines = []string{"—"}
	}
	if len(lines) > maxLines {
		lines = lines[:maxLines]
		lines[maxLines-1] += " …"
	}
	x11ui.Text(k.img, a.f.small, k.b.Min.X, k.y, fit(a.f.small, label, k.labelW-a.px(6)), colDim, x11ui.Left)
	for _, l := range lines {
		if k.y > k.b.Max.Y {
			return
		}
		x11ui.Text(k.img, a.f.small, k.b.Min.X+k.labelW, k.y, l, c, x11ui.Left)
		k.y += a.px(15)
	}
	k.y += k.lh - a.px(15)
}

func (k *kvList) gap() { k.y += k.a.px(5) }

func vec(v [3]float64, d int) string {
	return num(v[0], d) + "; " + num(v[1], d) + "; " + num(v[2], d)
}

func vec32(v [3]float32, d int) string {
	return vec([3]float64{float64(v[0]), float64(v[1]), float64(v[2])}, d)
}

// --- testa -------------------------------------------------------------------

func (a *App) headNote() string {
	switch a.snap.Sample.Head.Source {
	case gaze.HeadDerived:
		return "dagli occhi e dal viso"
	case gaze.HeadDevice:
		return "dal dispositivo"
	}
	return "nessuna posa"
}

func (a *App) drawHead(img *image.RGBA, b image.Rectangle) {
	s := &a.snap.Sample
	h := s.Head
	if !a.snap.HaveSample {
		h = gaze.Head{}
	}
	gs := min(a.px(118), b.Dx()*2/5)
	gr := image.Rect(b.Min.X, b.Min.Y+a.px(4), b.Min.X+gs, b.Min.Y+a.px(4)+gs)
	a.drawGizmo(img, gr, float64(h.RotationRad[0]), float64(h.RotationRad[1]), float64(h.RotationRad[2]), h.RotationValid)
	// angoli accanto all'indicatore
	x := gr.Max.X + a.px(14)
	y := gr.Min.Y + a.px(22)
	for i, name := range [3]string{"Pitch", "Yaw", "Roll"} {
		val, c, src := "—", colFaint, ""
		if h.RotationValid[i] {
			val, c = signed(deg(float64(h.RotationRad[i])), 1)+"°", colText
			src = "occhi"
			if h.RotationImage[i] {
				src = "immagine"
			}
		}
		x11ui.Text(img, a.f.small, x, y, name, colDim, x11ui.Left)
		x11ui.Text(img, a.f.big, x+a.px(44), y+a.px(2), val, c, x11ui.Left)
		x11ui.Text(img, a.f.small, b.Max.X, y, src, colFaint, x11ui.Right)
		y += a.px(36)
	}
	k := a.newKV(img, image.Rect(b.Min.X, gr.Max.Y+a.px(10), b.Max.X, b.Max.Y), 122)
	if h.PositionValid {
		k.row("Posizione, schermo", vec32(h.PositionMM, 0)+" mm", colText, true)
		k.row("Distanza schermo", num(float64(h.PositionMM[2]), 0)+" mm", colText, true)
	} else {
		k.row("Posizione, schermo", "—", colFaint, true)
		k.row("Distanza schermo", "—", colFaint, true)
	}
	if mid, ok := s.EyesTrackerMM(); a.snap.HaveSample && ok {
		k.row("Posizione, tracker", vec32(mid, 0)+" mm", colText, true)
	} else {
		k.row("Posizione, tracker", "—", colFaint, true)
	}
	if l, r := s.Left, s.Right; a.snap.HaveSample && l.OriginValid && r.OriginValid {
		d := 0.0
		for i := range l.OriginMM {
			d += float64((l.OriginMM[i] - r.OriginMM[i]) * (l.OriginMM[i] - r.OriginMM[i]))
		}
		k.row("Distanza cornee", num(math.Sqrt(d), 1)+" mm", colText, true)
	} else {
		k.row("Distanza cornee", "—", colFaint, true)
	}
	res := a.pv.res
	if res.Valid {
		k.row("Pitch senza filtro", signed(deg(res.RawPitch), 1)+"°", colText, true)
		k.row("Yaw, roll dal viso", signed(deg(res.Yaw), 1)+"°  "+signed(deg(res.Roll), 1)+"°", colText, true)
	} else {
		k.row("Pitch senza filtro", "—", colFaint, true)
		k.row("Yaw, roll dal viso", "—", colFaint, true)
	}
	if h.Source == gaze.HeadDerived && h.RotationValid[1] && !h.RotationImage[1] {
		k.row("Nota", "yaw dagli occhi: sul 4C è sottostimato", colWarn, false)
	}
}

// --- modello del viso ----------------------------------------------------------

func (a *App) drawFaceModel(img *image.RGBA, b image.Rectangle) {
	k := a.newKV(img, b, 118)
	if a.Col.HP == nil {
		k.row("Modello", "disattivato", colFaint, false)
		return
	}
	r := &a.rates
	res := a.pv.res
	score := a.pv.p.Score
	c := colOK
	if score < 0.5 {
		c = colWarn
	}
	k.row("Probabilità del viso", num(score, 3), c, true)
	k.row("Inferenza", num(res.InferenceMs, 1)+" ms · media "+num(r.hp.MeanProcessMs, 1)+" ms", colText, true)
	k.row("Immagini ricevute", num(r.hpImgHz, 1)+"/s · scartate "+num(r.hpDropHz, 1)+"/s", colText, true)
	k.row("Elaborate", num(r.hpProcHz, 1)+"/s · ricerche "+num(r.hpSearchHz, 1)+"/s", colText, true)
	k.row("Viso trovato", num(r.trackedPct, 0)+" % delle immagini", colText, true)
	if p := &a.pv.p; a.pv.have {
		k.row("Ultima immagine", strconv.FormatInt(p.DeviceUs, 10)+" µs · "+itoa(p.W)+"×"+itoa(p.H), colText, true)
		if a.snap.HaveSample {
			lag := float64(a.snap.Sample.DeviceTimeUs-p.DeviceUs) / 1000
			k.row("Ritardo sul gaze", num(lag, 1)+" ms (orologio del 4C)", colText, true)
		}
	}
	k.gap()
	st := a.snap.HPStatus
	switch {
	case !a.snap.HaveHP:
		k.row("Camera", "lettura dello stato…", colFaint, false)
	case st.Camera == nil:
		k.row("Camera", "non calibrata: si calibra da sola", colWarn, false)
		k.row("Campioni raccolti", itoa(r.coverage.Samples)+" in "+itoa(r.coverage.Cells)+" celle", colText, true)
		k.row("", "pitch non disponibile; vedi headpose-calibrate", colFaint, false)
	default:
		cam := st.Camera
		q := st.Quality
		k.row("Camera", "calibrata", colOK, false)
		k.row("Focale, centro", num(cam.F, 0)+" px · "+num(cam.CX, 0)+"; "+num(cam.CY, 0), colText, true)
		k.row("Camera nel tracker", vec(cam.C, 0)+" mm", colText, true)
		mirror := "no"
		if cam.Mirror {
			mirror = "sì"
		}
		k.row("Inclinazione", signed(headpose.CameraTiltDeg(cam), 1)+"° · speculare "+mirror, colText, true)
		k.row("Qualità", "RMS "+num(q.RMSPx, 2)+" px · σ "+num(q.SigmaDeg, 2)+"°", colText, true)
		k.row("Coppie, profondità", itoa(q.Pairs)+" · "+num(q.DepthMM, 0)+" mm", colText, true)
	}
	if st.HasOffset {
		k.row("Zero del pitch", signed(deg(st.Offset), 1)+"°", colText, true)
	} else if a.snap.HaveHP {
		k.row("Zero del pitch", "non impostato", colFaint, false)
	}
	if r.haveDiff {
		d := r.diff
		k.row("Yaw immagine − occhi", signed(d.YawDiffDeg, 1)+"° · rms "+num(d.YawRMSDeg, 1)+"°", colText, true)
		k.row("Roll immagine − occhi", signed(d.RollDiffDeg, 1)+"° · rms "+num(d.RollRMSDeg, 1)+"°", colText, true)
	}
}

// --- presenza ----------------------------------------------------------------

func presenceText(p gaze.Presence) (string, color.RGBA) {
	switch p {
	case gaze.PresencePresent:
		return "presente", colOK
	case gaze.PresenceAway:
		return "assente", colWarn
	}
	return "sconosciuta", colFaint
}

func (a *App) drawPresence(img *image.RGBA, b image.Rectangle) {
	s := &a.snap.Sample
	state, c := presenceText(s.Presence)
	y := b.Min.Y + a.px(22)
	x11ui.Circle(img, float64(b.Min.X)+a.pf(7), float64(y)-a.pf(7), a.pf(6), c)
	t := x11ui.Text(img, a.f.big, b.Min.X+a.px(22), y, state, colText, x11ui.Left)
	if n := len(a.snap.Presence); n > 0 {
		ev := a.snap.Presence[n-1]
		x11ui.Text(img, a.f.small, t.Max.X+a.px(12), y-a.px(2), "da "+fmtDuration(time.Since(ev.Time))+
			" · messaggio "+strconv.FormatInt(ev.DeviceUs, 10)+" µs", colDim, x11ui.Left)
	}
	if a.snap.HavePres {
		if v, ok := a.snap.PresRow.Get(gaze.PresenceKeyValue); ok {
			x11ui.Text(img, a.f.small, b.Max.X, y-a.px(2), "valore "+fmtValue(v, ""), colFaint, x11ui.Right)
		}
	}
	// cambi di stato
	y += a.px(24)
	x := b.Min.X
	x11ui.Text(img, a.f.small, x, y, "Cambi:", colFaint, x11ui.Left)
	x += a.px(48)
	for i := len(a.snap.Presence) - 1; i >= 0 && y <= b.Max.Y; i-- {
		ev := a.snap.Presence[i]
		st, c := presenceText(ev.State)
		item := ev.Time.Format("15:04:05") + " " + st
		w := a.f.small.Measure(item) + a.px(16)
		if x+w > b.Max.X {
			x = b.Min.X + a.px(48)
			y += a.px(16)
			if y > b.Max.Y {
				break
			}
		}
		x11ui.Circle(img, float64(x)+a.pf(3), float64(y)-a.pf(4), a.pf(3), c)
		x11ui.Text(img, a.f.small, x+a.px(10), y, item, colDim, x11ui.Left)
		x += w
	}
}

// --- colonne dello stream gaze ------------------------------------------------

func (a *App) columnsNote() string {
	if !a.snap.HaveRow {
		return "nessun frame"
	}
	r := a.snap.GazeRow
	return itoa(r.N) + " colonne · " + itoa(a.snap.Streams[ttp.StreamGaze].LastSize) + " byte"
}

func (a *App) drawColumns(img *image.RGBA, b image.Rectangle) {
	row := &a.snap.GazeRow
	colW := (b.Dx() - a.px(14)) / 2
	x0 := b.Min.X + a.px(14)
	x1 := x0 + colW
	y := b.Min.Y + a.px(10)
	x11ui.Text(img, a.f.head, x0, y, "SINISTRO", colLeft, x11ui.Left)
	x11ui.Text(img, a.f.head, x1, y, "DESTRO", colRight, x11ui.Left)
	y += a.px(8)
	if len(a.rows) == 0 {
		x11ui.Text(img, a.f.label, b.Min.X, y+a.px(24), "Colonne non ancora lette dal dispositivo", colFaint, x11ui.Left)
		return
	}
	for _, cr := range a.rows {
		if y+a.px(30) > b.Max.Y {
			break
		}
		y += a.px(14)
		label := cr.label
		if label == "" || a.fwNames {
			label = cr.name
		}
		lt := x11ui.Text(img, a.f.small, b.Min.X, y, label, colDim, x11ui.Left)
		if !cr.exported {
			x11ui.Text(img, a.f.small, lt.Max.X+a.px(6), y, "non esportata", colFaint, x11ui.Left)
		}
		// chiavi e unità a destra
		var keys []string
		for _, c := range cr.c {
			if c.key != 0 {
				keys = append(keys, fmt.Sprintf("%02x", c.key))
			}
		}
		meta := "0x" + strings.Join(keys, "/")
		if cr.unit != "" {
			meta = cr.unit + " · " + meta
		}
		x11ui.Text(img, a.f.monoS, b.Max.X, y, meta, colFaint, x11ui.Right)
		y += a.px(16)
		cells := 1
		if cr.eye {
			cells = 2
		}
		for side := 0; side < cells; side++ {
			c := cr.c[side]
			x := x0
			if side == 1 {
				x = x1
			}
			if c.key == 0 {
				continue
			}
			if !a.snap.HaveRow {
				x11ui.Text(img, a.f.mono, x, y, "—", colFaint, x11ui.Left)
				continue
			}
			v, ok := row.Get(c.key)
			if !ok {
				x11ui.Text(img, a.f.mono, x, y, "assente", colFaint, x11ui.Left)
				continue
			}
			vc := colText
			if c.valid != 0 {
				vv, _ := row.Get(c.valid)
				valid := vv.Kind == ttp.KindU32 && vv.U == 1
				dc := colOK
				if !valid {
					dc, vc = colBad, colFaint
				}
				x11ui.Circle(img, float64(x)-a.pf(8), float64(y)-a.pf(4), a.pf(3), dc)
			}
			txt := fmtValue(v, cr.unit)
			if cr.status && v.Kind == ttp.KindU32 {
				txt = statusText(v.U)
				if v.U != 0 {
					vc = colWarn
				}
			}
			w := colW - a.px(6)
			if !cr.eye {
				w = b.Max.X - x
			}
			x11ui.Text(img, a.f.mono, x, y, fit(a.f.mono, txt, w), vc, x11ui.Left)
		}
	}
}

// --- stream ------------------------------------------------------------------

func (a *App) drawStreams(img *image.RGBA, b image.Rectangle) {
	si := &a.snap.Session
	y := b.Min.Y + a.px(12)
	cat := si.Catalogue
	if len(cat) == 0 {
		x11ui.Text(img, a.f.label, b.Min.X, y+a.px(4), "Catalogo non ancora letto", colFaint, x11ui.Left)
		return
	}
	xName := b.Min.X + a.px(52)
	xState := b.Min.X + a.px(160)
	xHz := b.Max.X - a.px(62)
	for _, r := range cat {
		if y > b.Max.Y {
			return
		}
		id := r.ID
		x11ui.Text(img, a.f.monoS, b.Min.X, y, fmt.Sprintf("0x%04x", id), colFaint, x11ui.Left)
		x11ui.Text(img, a.f.label, xName, y, fit(a.f.label, r.Name, xState-xName-a.px(4)), colText, x11ui.Left)
		state, c := "non sottoscritto", colFaint
		if slices.Contains(si.Subscribed, id) {
			state, c = "sottoscritto", colOK
		} else if e, ok := si.Refused[id]; ok {
			state, c = "rifiutato: "+e, colBad
		}
		x11ui.Text(img, a.f.small, xState, y, fit(a.f.small, state, xHz-xState-a.px(60)), c, x11ui.Left)
		if st, ok := a.snap.Streams[id]; ok {
			x11ui.Text(img, a.f.monoS, xHz, y, num(a.rates.streamHz[id], 1)+" Hz", colText, x11ui.Right)
			x11ui.Text(img, a.f.monoS, b.Max.X, y, sizeText(st.LastSize), colDim, x11ui.Right)
		}
		y += a.px(15)
		// colonne dichiarate dal firmware
		if cols := si.Columns[id]; len(cols) > 0 && id != ttp.StreamGaze {
			var names []string
			for _, c := range cols {
				n := c.Name
				if n == "" {
					n = fmt.Sprintf("0x%02x", c.ID)
				}
				names = append(names, n)
			}
			for _, l := range x11ui.Wrap(a.f.small, strings.Join(names, " · "), b.Max.X-xName) {
				if y > b.Max.Y {
					return
				}
				x11ui.Text(img, a.f.small, xName, y, l, colFaint, x11ui.Left)
				y += a.px(14)
			}
		}
		// ultima riga degli stream senza un pannello proprio
		if row, ok := a.snap.Other[id]; ok {
			names := map[uint32]string{}
			for _, c := range si.Columns[id] {
				names[c.ID] = c.Name
			}
			for i := 0; i < row.N && y <= b.Max.Y; i++ {
				col := row.Cols[i]
				x11ui.Text(img, a.f.monoS, xName, y, fit(a.f.monoS, "↳ "+names[col.Key]+" = "+fmtValue(col.Value, ""), b.Max.X-xName), colDim, x11ui.Left)
				y += a.px(14)
			}
		}
		y += a.px(5)
	}
	if len(a.snap.Notes) > 0 && y <= b.Max.Y {
		y += a.px(4)
		x11ui.Text(img, a.f.small, b.Min.X, y, "Notifiche del dispositivo", colDim, x11ui.Left)
		y += a.px(15)
		for i := len(a.snap.Notes) - 1; i >= 0 && y <= b.Max.Y; i-- {
			n := a.snap.Notes[i]
			x11ui.Text(img, a.f.monoS, b.Min.X, y, n.Time.Format("15:04:05")+fmt.Sprintf("  id %d  %v", n.ID, n.Values), colText, x11ui.Left)
			y += a.px(14)
		}
	}
}

func sizeText(n int) string {
	switch {
	case n >= 10*1024:
		return itoa(n/1024) + " KiB"
	default:
		return itoa(n) + " B"
	}
}

// --- dispositivo -------------------------------------------------------------

func (a *App) drawDevice(img *image.RGBA, b image.Rectangle) {
	si := &a.snap.Session
	k := a.newKV(img, b, 104)
	str := func(i int) string {
		if i < len(si.Strings) && si.Strings[i] != "" {
			return si.Strings[i]
		}
		return "—"
	}
	if si.Sessions == 0 && !si.Connected {
		k.row("Stato", "in attesa della prima connessione", colFaint, false)
		return
	}
	k.row("Modello", str(1)+" · generazione "+str(2), colText, false)
	k.row("Seriale", str(0), colText, true)
	k.row("Firmware", str(3), colText, true)
	u := si.USB
	k.row("USB", fmt.Sprintf("%s:%s · bus %d indirizzo %d · %s", u.VendorID, u.ProductID, u.Bus, u.Address, u.Speed), colText, true)
	k.row("Descrittori USB", strings.TrimSpace(u.Manufacturer+" · "+u.Product), colText, false)
	proto := "—"
	if len(si.Protocol) > 0 {
		proto = fmt.Sprintf("0x%x", si.Protocol[0])
	}
	realm := "—"
	if len(si.Realm) > 0 {
		realm = fmt.Sprint(si.Realm[0])
		if si.Realm[0] == 0 {
			realm += " (nessuna autenticazione)"
		}
	}
	k.row("Protocollo, realm", proto+" · "+realm, colText, true)
	rate := "—"
	if len(si.OutputRate) > 0 {
		var parts []string
		for _, v := range si.OutputRate {
			parts = append(parts, itoa(int(v)))
		}
		rate = strings.Join(parts, "/") + " Hz"
	}
	eyes := "—"
	if len(si.EnabledEye) > 0 {
		eyes = map[uint32]string{1: "sinistro", 2: "destro", 3: "entrambi"}[si.EnabledEye[0]]
		if eyes == "" {
			eyes = itoa(int(si.EnabledEye[0]))
		} else {
			eyes += " (" + itoa(int(si.EnabledEye[0])) + ")"
		}
	}
	k.row("Frequenza, occhi", rate+" · "+eyes, colText, true)
	if d := si.Display; d != nil {
		k.row("Display area", num(d.Width(), 1)+" × "+num(d.Height(), 1)+" mm", colText, true)
		k.row("  alto sinistra", vec(d.TopLeft, 1), colDim, true)
		k.row("  alto destra", vec(d.TopRight, 1), colDim, true)
		k.row("  basso sinistra", vec(d.BottomLeft, 1), colDim, true)
	} else {
		k.row("Display area", "—", colFaint, true)
	}
	if m := si.Mounting; m != nil {
		k.row("Montaggio", fmt.Sprintf("%d segni guida a %s mm · camera a %s°", m.Guides, num(m.WidthMM, 0), num(m.AngleDeg, 0)), colText, false)
		k.row("  esterno, interno", vec(m.ExternalMM, 1)+" · "+vec(m.InternalMM, 1), colDim, true)
	}
	if tb := si.TrackBox; tb != nil {
		nw, nh := tb.Size(false)
		fw, fh := tb.Size(true)
		k.row("Track box", num(nw, 0)+"×"+num(nh, 0)+" → "+num(fw, 0)+"×"+num(fh, 0)+" mm, z "+num(tb.Near(), 0)+"–"+num(tb.Far(), 0)+" mm", colText, true)
	}
	props := func(ps []ttp.Property) string {
		var parts []string
		for _, p := range ps {
			v := p.Value
			if v == "" {
				v = `""`
			}
			parts = append(parts, fmt.Sprintf("%x=%s", p.Key, v))
		}
		return strings.Join(parts, "  ")
	}
	k.wrap("Proprietà 0x532", props(si.Properties), colText, 2)
	k.wrap("Stato 0x5d2", props(si.Status), colText, 2)
	if len(si.Probes) > 0 {
		var parts []string
		for n, e := range si.Probes {
			parts = append(parts, n+": "+e)
		}
		slices.Sort(parts)
		k.wrap("Non disponibili", strings.Join(parts, " · "), colWarn, 2)
	}
}

// --- comunicazione -----------------------------------------------------------

func (a *App) drawPipeline(img *image.RGBA, b image.Rectangle) {
	c := a.counters()
	base := a.base
	r := &a.rates
	half := (b.Dx() - a.px(10)) / 2
	left := image.Rect(b.Min.X, b.Min.Y, b.Min.X+half, b.Max.Y)
	right := image.Rect(b.Min.X+half+a.px(10), b.Min.Y, b.Max.X, b.Max.Y)
	kl, kr := a.newKV(img, left, 96), a.newKV(img, right, 96)
	warn := func(v uint64) color.RGBA {
		if v > 0 {
			return colWarn
		}
		return colText
	}
	cnt := func(v, b uint64) string { return strconv.FormatUint(since(v, b), 10) }
	kl.row("Frequenza gaze", num(r.gazeHz, 1)+" Hz", colText, true)
	kr.row("Frame inoltrati", cnt(c.gaze, base.gaze), colText, true)
	kl.row("Persi dal 4C", cnt(c.devDrops, base.devDrops), warn(since(c.devDrops, base.devDrops)), true)
	kr.row("Scartati (host)", cnt(c.hostDrops, base.hostDrops), warn(since(c.hostDrops, base.hostDrops)), true)
	kl.row("Pause (comandi)", cnt(c.devPauses, base.devPauses), colText, true)
	kr.row("Errori decodif.", cnt(c.decodeErr, base.decodeErr), warn(since(c.decodeErr, base.decodeErr)), true)
	kl.row("Resync", cnt(c.resyncs, base.resyncs), warn(since(c.resyncs, base.resyncs)), true)
	kr.row("Risposte orfane", cnt(c.orphan, base.orphan), colText, true)
	kl.row("USB", num(r.usbPerS, 0)+" trasf./s", colText, true)
	kr.row("Banda USB", num(r.usbBps/1e6, 2)+" MB/s", colText, true)
	lat := "—"
	if r.latMax > 0 {
		lat = itoa(int(r.lat50/1000)) + "/" + itoa(int(r.lat99/1000)) + "/" + itoa(int(r.latMax/1000)) + " µs"
	}
	kl.row("Latenza pipeline", lat, colText, true)
	ka := "—"
	if r.ka > 0 {
		ka = num(float64(r.ka)/1000, 2) + " ms"
	}
	kr.row("Keep-alive RTT", ka, colText, true)
	s := &a.snap.Sample
	age := "—"
	if a.snap.HaveSample {
		age = num(float64(time.Since(time.Unix(0, s.HostTimeNs)).Microseconds())/1000, 1) + " ms"
	}
	kl.row("Età del campione", age, colText, true)
	kr.row("Riconnessioni", cnt(c.reconnects, base.reconnects), warn(since(c.reconnects, base.reconnects)), true)
	kl.row("Notifiche", cnt(c.notes, base.notes), colText, true)
	kr.row("Errori dei sink", cnt(c.sinkErr, base.sinkErr), warn(since(c.sinkErr, base.sinkErr)), true)
	fc, step := "—", "—"
	if a.snap.HaveSample && s.HasCounter {
		fc = strconv.FormatUint(uint64(s.FrameCounter), 10)
	}
	if a.snap.CounterStep > 0 {
		step = itoa(int(a.snap.CounterStep))
	}
	kl.row("Frame counter", fc, colText, true)
	kr.row("Passo del counter", step, colText, true)
	clock := "—"
	if a.snap.HaveSample {
		clock = num(float64(s.DeviceTimeUs)/1e6, 3) + " s"
	}
	kl.row("Orologio del 4C", clock, colText, true)
	kr.row("Seq dell'host", strconv.FormatUint(s.Seq, 10), colText, true)
	kl.row("Altri messaggi", cnt(c.other, base.other), colText, true)
	kr.row("Sessioni USB", itoa(a.snap.Session.Sessions), colText, true)
	kl.row("Campioni ricevuti", strconv.FormatUint(since(a.snap.Samples, base.samples), 10), colText, true)
}

// --- registro ----------------------------------------------------------------

func (a *App) logNote() string {
	fw := 0
	for _, l := range a.snap.Logs {
		if l.Firmware {
			fw++
		}
	}
	return itoa(fw) + " del firmware · " + itoa(len(a.snap.Logs)-fw) + " del driver"
}

func (a *App) drawLog(img *image.RGBA, b image.Rectangle) {
	lh := a.px(14)
	tagW := a.px(30)
	timeW := a.f.monoS.Measure("00:00:00") + a.px(8)
	textX := b.Min.X + timeW + tagW
	textW := b.Max.X - textX
	y := b.Max.Y - a.px(2)
	if len(a.snap.Logs) == 0 {
		x11ui.Text(img, a.f.label, b.Min.X, b.Min.Y+a.px(14), "Nessun messaggio", colFaint, x11ui.Left)
		return
	}
	for i := len(a.snap.Logs) - 1; i >= 0; i-- {
		l := a.snap.Logs[i]
		lines := wrap(a.f.small, l.Text, textW)
		if len(lines) == 0 {
			continue
		}
		if len(lines) > 3 {
			lines = append(lines[:2], fit(a.f.small, strings.Join(lines[2:], " "), textW))
		}
		top := y - (len(lines)-1)*lh
		if top-lh < b.Min.Y {
			return
		}
		c := colText
		switch l.Level {
		case 'W':
			c = colWarn
		case 'E':
			c = colBad
		}
		tag, tc := "DRV", colDim
		if l.Firmware {
			tag, tc = "FW", colSensor
		}
		x11ui.Text(img, a.f.monoS, b.Min.X, top, l.Time.Format("15:04:05"), colFaint, x11ui.Left)
		x11ui.Text(img, a.f.monoS, b.Min.X+timeW, top, tag, tc, x11ui.Left)
		for j, ln := range lines {
			x11ui.Text(img, a.f.small, textX, top+j*lh, ln, c, x11ui.Left)
		}
		y = top - lh - a.px(2)
	}
}
