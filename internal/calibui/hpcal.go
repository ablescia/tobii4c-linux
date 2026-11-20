package calibui

import (
	"fmt"
	"image"
	"image/color"
	"math"
	"time"

	"github.com/ablescia/tobii4c-linux/internal/gaze"
	"github.com/ablescia/tobii4c-linux/internal/headpose"
	"github.com/ablescia/tobii4c-linux/internal/x11ui"
)

// Applicazione guidata per il pitch della testa (tobii4c headpose-calibrate):
// raccoglie i campioni per calibrare la camera del 4C con movimenti guidati a
// tempo, calcola la calibrazione, misura lo zero personale del pitch e
// verifica pitch e presenza. Ogni fase mostra il tempo che resta, la distanza
// e la posizione dal vivo, e l'immagine della camera.

// HeadPoseStart è la schermata iniziale dell'applicazione.
func HeadPoseStart() Screen { return newHPIntro(&hpSession{}) }

// hpMode è il percorso scelto nel menu.
type hpMode int

const (
	hpFull   hpMode = iota // camera + viso + verifica
	hpFace                 // solo lo zero del viso + verifica del pitch
	hpVerify               // solo verifica, senza salvare
)

// hpSession conserva i risultati tra una schermata e l'altra.
type hpSession struct {
	mode hpMode

	cam        *headpose.Camera
	q          headpose.CalibQuality
	calibrated bool
	saved      bool
	saveErr    error

	pitchDone            bool
	neutral, up, down    float64 // pitch grezzo medio (rad)
	yawDiff, rollDiff    float64 // immagine − occhi, a testa dritta (rad)
	offsetSaved          bool
	presenceDone         bool
	awaySec, backSec     float64
	presenceOK, returnOK bool
}

// --- elementi comuni --------------------------------------------------------

// hpLayout: anteprima della camera a sinistra, istruzioni e strumenti a destra.
type hpLayout struct {
	preview image.Rectangle
	right   image.Rectangle
	prev    headpose.Preview
	lut     [256]byte
	haveImg bool
}

func (l *hpLayout) static(a *App, img *image.RGBA, title, subtitle string) {
	a.ShowCursor(false)
	x11ui.Text(img, a.F.Title, a.W/2, a.Px(105), title, colText, x11ui.Center)
	if subtitle != "" {
		x11ui.Text(img, a.F.Body, a.W/2, a.Px(150), subtitle, colDim, x11ui.Center)
	}
	pw, ph := a.Px(640), a.Px(480)
	l.preview = image.Rect(a.Px(110), a.Px(210), a.Px(110)+pw, a.Px(210)+ph)
	x11ui.RoundRect(img, l.preview.Inset(-a.Px(6)), a.Pf(12), colPanel)
	x11ui.Text(img, a.F.Small, l.preview.Min.X, l.preview.Max.Y+a.Px(34),
		"Camera del 4C (a specchio). Verde: i punti del viso trovati dal modello.", colFaint, x11ui.Left)
	l.right = image.Rect(a.Px(850), a.Px(210), a.W-a.Px(110), a.Px(900))
}

// frame disegna l'anteprima della camera; restituisce se c'è un viso.
func (l *hpLayout) frame(a *App, img *image.RGBA) bool {
	if a.HP == nil {
		return false
	}
	if a.HP.LatestPreview(&l.prev) {
		// guadagno automatico: le immagini IR sono scure
		var hist [256]int
		for i := 0; i < len(l.prev.Pix); i += 7 {
			hist[l.prev.Pix[i]]++
		}
		total, acc, hi := len(l.prev.Pix)/7, 0, 255
		for v := 255; v >= 0; v-- {
			acc += hist[v]
			if acc > total/200 {
				hi = v
				break
			}
		}
		hi = max(hi, 24)
		for v := range l.lut {
			l.lut[v] = byte(min(255, v*255/hi))
		}
		l.haveImg = true
	}
	r := l.preview
	if !l.haveImg || l.prev.W == 0 {
		x11ui.Fill(img, r, colPanel)
		a.Dyn(r)
		a.Dyn(x11ui.Text(img, a.F.Body, r.Min.X+r.Dx()/2, r.Min.Y+r.Dy()/2, "In attesa della camera…", colDim, x11ui.Center))
		return false
	}
	p := &l.prev
	w, h := r.Dx(), r.Dy()
	for y := 0; y < h; y++ {
		sy := y * p.H / h
		row := img.Pix[(r.Min.Y+y-img.Rect.Min.Y)*img.Stride+(r.Min.X-img.Rect.Min.X)*4:]
		src := p.Pix[sy*p.W : (sy+1)*p.W]
		for x := 0; x < w; x++ {
			v := l.lut[src[p.W-1-x*p.W/w]] // a specchio
			o := 4 * x
			row[o], row[o+1], row[o+2], row[o+3] = v, v, v, 255
		}
	}
	a.Dyn(r)
	if !p.Face {
		return false
	}
	sx, sy := float64(w)/float64(p.W), float64(h)/float64(p.H)
	for i := 0; i < 468; i += 2 {
		x := float64(r.Min.X) + (float64(p.W)-p.LM[i][0])*sx
		y := float64(r.Min.Y) + p.LM[i][1]*sy
		if x > float64(r.Min.X) && x < float64(r.Max.X)-1 && y > float64(r.Min.Y) && y < float64(r.Max.Y)-1 {
			img.Set(int(x), int(y), color.RGBA{90, 230, 120, 255})
		}
	}
	for _, i := range []int{468, 473} {
		x := float64(r.Min.X) + (float64(p.W)-p.LM[i][0])*sx
		y := float64(r.Min.Y) + p.LM[i][1]*sy
		x11ui.Circle(img, x, y, a.Pf(3), color.RGBA{80, 170, 255, 255})
	}
	return true
}

// live è lo stato dal vivo del tracker.
type live struct {
	s        gaze.Sample
	fresh    bool
	distMM   float64 // distanza degli occhi dal tracker
	xMM, yMM float64 // posizione della testa dal centro dello schermo
	haveEyes bool
}

func readLive(a *App) live {
	var l live
	s, ok := a.Ctrl.Hub.Latest()
	l.s = s
	l.fresh = ok && time.Since(time.Unix(0, s.HostTimeNs)) < 300*time.Millisecond
	if !l.fresh {
		return l
	}
	if mid, ok := s.EyesTrackerMM(); ok {
		l.distMM = norm(mid)
		l.haveEyes = true
	}
	if s.Head.PositionValid {
		l.xMM, l.yMM = float64(s.Head.PositionMM[0]), float64(s.Head.PositionMM[1])
	}
	return l
}

// gauge è una scala orizzontale con zona buona, bersaglio e valore attuale.
type gauge struct {
	r              image.Rectangle
	lo, hi         float64 // estremi della scala
	goodLo, goodHi float64
	ticks          []float64
	label          func(v float64) string
}

func (g *gauge) xAt(v float64) float64 {
	t := (v - g.lo) / (g.hi - g.lo)
	return float64(g.r.Min.X) + math.Max(0, math.Min(1, t))*float64(g.r.Dx())
}

func (g *gauge) static(a *App, img *image.RGBA) {
	x11ui.RoundRect(img, g.r, a.Pf(4), colPanel)
	if g.goodHi > g.goodLo {
		x11ui.RoundRect(img, image.Rect(int(g.xAt(g.goodLo)), g.r.Min.Y, int(g.xAt(g.goodHi)), g.r.Max.Y), a.Pf(4), x11ui.Alpha(colOK, 0.45))
	}
	for _, t := range g.ticks {
		x := g.xAt(t)
		x11ui.Line(img, x, float64(g.r.Max.Y)+a.Pf(4), x, float64(g.r.Max.Y)+a.Pf(12), a.Pf(1.5), colFaint)
		x11ui.Text(img, a.F.Small, int(x), g.r.Max.Y+a.Px(36), g.label(t), colFaint, x11ui.Center)
	}
}

// frame disegna bersaglio (anello) e valore attuale (disco con etichetta).
func (g *gauge) frame(a *App, img *image.RGBA, target float64, haveTarget bool, value float64, haveValue bool, near bool) {
	cy := float64(g.r.Min.Y) + float64(g.r.Dy())/2
	if haveTarget {
		x := g.xAt(target)
		a.Dyn(x11ui.Ring(img, x, cy, a.Pf(22), a.Pf(4), colText))
		a.Dyn(x11ui.Text(img, a.F.Small, int(x), g.r.Min.Y-a.Px(40), "bersaglio "+g.label(target), colDim, x11ui.Center))
	}
	if haveValue {
		x := g.xAt(value)
		c := colWarn
		if near {
			c = colOK
		}
		a.Dyn(x11ui.Circle(img, x, cy, a.Pf(13), c))
		a.Dyn(x11ui.Text(img, a.F.Heading, int(x), g.r.Max.Y+a.Px(76), g.label(value), c, x11ui.Center))
	}
}

// countdown disegna i secondi che restano e la barra della fase.
func countdown(a *App, img *image.RGBA, x, y, width int, remaining, total time.Duration, label string) {
	sec := int(math.Ceil(remaining.Seconds()))
	a.Dyn(x11ui.Text(img, a.F.Big, x, y, fmt.Sprintf("%d s", max(sec, 0)), colText, x11ui.Left))
	a.Dyn(x11ui.Text(img, a.F.Body, x+a.Px(190), y-a.Px(10), label, colDim, x11ui.Left))
	bar := image.Rect(x, y+a.Px(26), x+width, y+a.Px(34))
	x11ui.RoundRect(img, bar, a.Pf(4), colPanel)
	f := 1 - remaining.Seconds()/total.Seconds()
	f = math.Max(0, math.Min(1, f))
	if f > 0 {
		x11ui.RoundRect(img, image.Rect(bar.Min.X, bar.Min.Y, bar.Min.X+int(f*float64(bar.Dx())), bar.Max.Y), a.Pf(4), colDim)
	}
	a.Dyn(bar)
}

func cm(mm float64) string { return it(mm/10, 0) + " cm" }

// --- 1. menu iniziale ---------------------------------------------------------

type hpIntro struct {
	ses    *hpSession
	lay    hpLayout
	dist   gauge
	status headpose.Status
}

func newHPIntro(ses *hpSession) *hpIntro { return &hpIntro{ses: ses} }

func (s *hpIntro) Static(a *App, img *image.RGBA) {
	s.lay.static(a, img, "Pitch della testa: calibrazione",
		"Le fasi sono a tempo e il tempo che resta è sempre a video. Guarda sempre lo schermo.")
	s.status = a.HP.Status()
	r := s.lay.right
	y := r.Min.Y + a.Px(20)
	st := s.status
	camLine, camC := "Camera del 4C: non calibrata", colWarn
	if st.Calibrated {
		camLine, camC = fmt.Sprintf("Camera del 4C: calibrata (errore %s px, incertezza %s°)", it(st.Quality.RMSPx, 1), it(st.Quality.SigmaDeg, 2)), colOK
	}
	faceLine, faceC := "Zero del viso: non misurato", colWarn
	if st.HasOffset {
		faceLine, faceC = "Zero del viso: misurato ("+it(deg(st.Offset), 1)+"°)", colOK
	}
	x11ui.Text(img, a.F.Body, r.Min.X, y, camLine, camC, x11ui.Left)
	x11ui.Text(img, a.F.Body, r.Min.X, y+a.Px(38), faceLine, faceC, x11ui.Left)
	y += a.Px(110)
	items := []struct{ key, title, what string }{
		{"1", "Calibrazione completa (circa 2 minuti)",
			"Camera del 4C: ti muovi con calma avanti e indietro, di lato, su e giù finché le caselle sono piene; poi zero del viso e verifica. Da fare la prima volta su ogni computer o con un altro 4C."},
		{"2", "Ricalibra solo il viso (20 secondi)",
			"Misura di nuovo lo zero del pitch: per un'altra persona o un'altra postura. Richiede la camera calibrata."},
		{"3", "Solo verifica (pitch e presenza)", "Controlla il pitch con mento alzato e abbassato e la presenza, senza salvare nulla."},
	}
	for _, it := range items {
		c := colText
		if it.key != "1" && !st.Calibrated {
			c = colFaint
		}
		kw := a.Px(40)
		x11ui.RoundRect(img, image.Rect(r.Min.X, y-a.Px(28), r.Min.X+kw, y+a.Px(10)), a.Pf(6), colKey)
		x11ui.Text(img, a.F.Heading, r.Min.X+kw/2, y, it.key, colText, x11ui.Center)
		x11ui.Text(img, a.F.Heading, r.Min.X+kw+a.Px(18), y, it.title, c, x11ui.Left)
		y = a.paragraph(img, a.F.Small, r.Min.X+kw+a.Px(18), y+a.Px(32), r.Dx()-kw-a.Px(18), it.what, colDim, x11ui.Left) + a.Px(28)
	}
	s.dist = gauge{r: image.Rect(r.Min.X, y+a.Px(70), r.Max.X, y+a.Px(78)), lo: 400, hi: 1000,
		goodLo: 500, goodHi: 850, ticks: []float64{400, 500, 600, 700, 800, 900, 1000}, label: cm}
	x11ui.Text(img, a.F.Small, r.Min.X, s.dist.r.Min.Y-a.Px(50), "Distanza dei tuoi occhi dal sensore", colDim, x11ui.Left)
	s.dist.static(a, img)
	a.drawKeys(img, a.W/2, a.H-a.Px(70), []keyHint{{"1", "completa"}, {"2", "solo viso"}, {"3", "verifica"}, {"Esc", "esci"}}, x11ui.Center)
}

func (s *hpIntro) Frame(a *App, img *image.RGBA, now time.Time) {
	face := s.lay.frame(a, img)
	lv := readLive(a)
	near := lv.haveEyes && lv.distMM >= 500 && lv.distMM <= 850
	s.dist.frame(a, img, 0, false, lv.distMM, lv.haveEyes, near)
	msg, c := "Non vedo i tuoi occhi: guarda lo schermo", colWarn
	switch {
	case !lv.haveEyes:
	case !face:
		msg = "Vedo gli occhi ma non il viso: spostati finché il viso è intero nell'immagine a sinistra"
	case !near:
		msg = "Portati tra 50 e 85 cm dal sensore"
	default:
		msg, c = "Pronto: scegli 1, 2 o 3", colOK
	}
	a.Dyn(x11ui.Text(img, a.F.Heading, a.W/2, a.H-a.Px(130), msg, c, x11ui.Center))
}

func (s *hpIntro) Event(a *App, ev x11ui.Event) {
	if ev.Kind != x11ui.KeyDown {
		return
	}
	ses := &hpSession{}
	switch ev.Sym {
	case x11ui.KeyEscape:
		a.Quit()
	case '1', x11ui.KeyReturn, x11ui.KeyKPEnter:
		ses.mode = hpFull
		a.HP.ResetSamples()
		a.Flow(
			func() Screen { return newHPMove(ses, moveDepth) },
			func() Screen { return newHPMove(ses, moveSide) },
			func() Screen { return newHPMove(ses, moveHeight) },
			func() Screen { return newHPCompute(ses) },
		)
	case '2', '3':
		if !s.status.Calibrated {
			return
		}
		ses.mode = hpFace
		next := []func() Screen{func() Screen { return newHPPitch(ses) }}
		if ev.Sym == '3' {
			ses.mode = hpVerify
			next = append(next, func() Screen { return newHPPresence(ses) })
		}
		next = append(next, func() Screen { return newHPSummary(ses) })
		a.Flow(next...)
	}
}

// --- 2. movimenti guidati ---------------------------------------------------
//
// Nessun bersaglio da inseguire: sopra la scala ci sono delle caselle, e
// ognuna si riempie mentre la testa resta nella sua fascia con viso e occhi
// visibili. Ci si muove con calma, nell'ordine che si vuole; la fase finisce
// quando le caselle sono piene (o allo scadere del tempo massimo).

type moveKind int

const (
	moveDepth moveKind = iota
	moveSide
	moveHeight
)

const (
	hpLead     = 3 * time.Second // preparazione prima di ogni fase
	binDwell   = 1.5             // secondi per riempire una casella
	skipFilled = 0.7             // con Invio si passa avanti da questa frazione di caselle piene
)

type hpMove struct {
	ses    *hpSession
	kind   moveKind
	lay    hpLayout
	g      gauge
	start  time.Time
	last   time.Time
	maxDur time.Duration
	base   float64 // altezza all'inizio della fase
	based  bool
	title  string
	what   string
	edges  []float64 // limiti delle caselle (len = caselle + 1)
	fill   []float64 // secondi passati in ogni casella
	done   bool
}

func newHPMove(ses *hpSession, k moveKind) *hpMove {
	m := &hpMove{ses: ses, kind: k}
	switch k {
	case moveDepth:
		m.maxDur = 75 * time.Second
		m.title = "Fase 1 di 3 · Distanza"
		m.what = "Avvicinati e allontanati con calma dal sensore, guardando lo schermo, finché tutte le caselle sono piene. L'ordine non conta."
		m.edges = []float64{500, 550, 600, 650, 700, 750, 800, 850}
	case moveSide:
		m.maxDur = 60 * time.Second
		m.title = "Fase 2 di 3 · Di lato"
		m.what = "Spostati con il busto verso destra e verso sinistra, guardando lo schermo, finché tutte le caselle sono piene."
		m.edges = []float64{-200, -150, -100, -50, 0, 50, 100, 150, 200}
	case moveHeight:
		m.maxDur = 50 * time.Second
		m.title = "Fase 3 di 3 · Su e giù"
		m.what = "Alzati e abbassati un poco sulla sedia (raddrizza la schiena, poi scivola giù), guardando lo schermo, finché le caselle sono piene."
		m.edges = []float64{-60, -40, -20, 0, 20, 40, 60}
	}
	m.fill = make([]float64, len(m.edges)-1)
	return m
}

func (m *hpMove) Static(a *App, img *image.RGBA) {
	m.lay.static(a, img, m.title, "Raccolta dei campioni per la calibrazione della camera")
	r := m.lay.right
	a.paragraph(img, a.F.Body, r.Min.X, r.Min.Y+a.Px(20), r.Dx(), m.what, colText, x11ui.Left)
	gy := r.Min.Y + a.Px(400)
	switch m.kind {
	case moveDepth:
		m.g = gauge{r: image.Rect(r.Min.X, gy, r.Max.X, gy+a.Px(8)), lo: 400, hi: 950,
			ticks: []float64{400, 500, 600, 700, 800, 900}, label: cm}
		x11ui.Text(img, a.F.Small, r.Min.X, gy-a.Px(120), "Distanza dei tuoi occhi dal sensore", colDim, x11ui.Left)
	case moveSide:
		m.g = gauge{r: image.Rect(r.Min.X, gy, r.Max.X, gy+a.Px(8)), lo: -280, hi: 280,
			ticks: []float64{-250, -150, -50, 50, 150, 250}, label: func(v float64) string {
				switch {
				case v < -5:
					return it(-v/10, 0) + " cm sin."
				case v > 5:
					return it(v/10, 0) + " cm des."
				}
				return "centro"
			}}
		x11ui.Text(img, a.F.Small, r.Min.X, gy-a.Px(120), "Posizione della testa rispetto al centro dello schermo", colDim, x11ui.Left)
	case moveHeight:
		m.g = gauge{r: image.Rect(r.Min.X, gy, r.Max.X, gy+a.Px(8)), lo: -90, hi: 90,
			ticks: []float64{-80, -40, 0, 40, 80}, label: func(v float64) string {
				switch {
				case v < -5:
					return it(-v/10, 0) + " cm più giù"
				case v > 5:
					return it(v/10, 0) + " cm più su"
				}
				return "partenza"
			}}
		x11ui.Text(img, a.F.Small, r.Min.X, gy-a.Px(120), "Altezza della testa rispetto all'inizio (sinistra = più giù, destra = più su)",
			colDim, x11ui.Left)
	}
	m.g.static(a, img)
	a.drawKeys(img, a.W/2, a.H-a.Px(70), []keyHint{{"Invio", "avanti (con il 70% delle caselle piene)"}, {"Esc", "torna al menu"}}, x11ui.Center)
	m.start = time.Now()
	m.last = m.start
}

func (m *hpMove) filled() (full int, frac float64) {
	for _, f := range m.fill {
		if f >= binDwell {
			full++
		}
		frac += math.Min(f/binDwell, 1)
	}
	return full, frac / float64(len(m.fill))
}

func (m *hpMove) Frame(a *App, img *image.RGBA, now time.Time) {
	face := m.lay.frame(a, img)
	lv := readLive(a)
	dt := now.Sub(m.last).Seconds()
	m.last = now
	el := now.Sub(m.start)
	r := m.lay.right
	var value float64
	have := false
	switch m.kind {
	case moveDepth:
		value, have = lv.distMM, lv.haveEyes
	case moveSide:
		value, have = lv.xMM, lv.s.Head.PositionValid && lv.fresh
	case moveHeight:
		if lv.s.Head.PositionValid && lv.fresh {
			if !m.based || el < hpLead {
				m.base, m.based = lv.yMM, true
			}
			value, have = lv.yMM-m.base, true
		}
	}
	lead := el < hpLead
	cur := -1
	if have {
		for i := 0; i+1 < len(m.edges); i++ {
			if value >= m.edges[i] && value < m.edges[i+1] {
				cur = i
			}
		}
	}
	if !lead && cur >= 0 && face && lv.haveEyes && dt < 0.2 {
		m.fill[cur] += dt
	}

	// caselle sopra la scala
	by := m.g.r.Min.Y - a.Px(80)
	for i, f := range m.fill {
		x0, x1 := int(m.g.xAt(m.edges[i]))+a.Px(3), int(m.g.xAt(m.edges[i+1]))-a.Px(3)
		box := image.Rect(x0, by, x1, by+a.Px(44))
		a.Dyn(x11ui.RoundRect(img, box, a.Pf(6), colPanel))
		fr := math.Min(f/binDwell, 1)
		if fr > 0 {
			c := x11ui.Alpha(colOK, 0.45)
			if fr >= 1 {
				c = colOK
			}
			a.Dyn(x11ui.RoundRect(img, image.Rect(x0, by, x0+int(fr*float64(x1-x0)), by+a.Px(44)), a.Pf(6), c))
		}
		if i == cur {
			a.Dyn(x11ui.Ring(img, float64(x0+x1)/2, float64(by)+a.Pf(22), a.Pf(8), a.Pf(2.5), colText))
		}
	}
	full, frac := m.filled()
	m.g.frame(a, img, 0, false, value, have, cur >= 0)

	cy := r.Min.Y + a.Px(200)
	if lead {
		countdown(a, img, r.Min.X, cy, r.Dx(), hpLead-el, hpLead, "si parte tra poco")
	} else {
		countdown(a, img, r.Min.X, cy, r.Dx(), m.maxDur-(el-hpLead), m.maxDur, "tempo massimo")
	}
	a.Dyn(x11ui.Text(img, a.F.Heading, r.Min.X, r.Min.Y+a.Px(560),
		fmt.Sprintf("Caselle piene: %d di %d", full, len(m.fill)), colText, x11ui.Left))

	// suggerimento: verso la casella vuota più vicina
	msg, c := "", colOK
	switch {
	case !lv.haveEyes:
		msg, c = "Non vedo i tuoi occhi: guarda lo schermo", colWarn
	case !face:
		msg, c = "Il viso non è intero nell'immagine", colWarn
	case full == len(m.fill):
		msg = "Fatto!"
	default:
		best, bd := -1, math.Inf(1)
		for i, f := range m.fill {
			if f >= binDwell {
				continue
			}
			mid := (m.edges[i] + m.edges[i+1]) / 2
			if d := math.Abs(mid - value); d < bd {
				best, bd = i, d
			}
		}
		if best == cur {
			msg = "Resta qui un momento"
		} else {
			mid := (m.edges[best] + m.edges[best+1]) / 2
			up := mid > value
			msg = map[moveKind][2]string{moveDepth: {"Avvicinati", "Allontanati"}, moveSide: {"Vai verso sinistra", "Vai verso destra"},
				moveHeight: {"Abbassati un po'", "Alzati un po'"}}[m.kind][map[bool]int{false: 0, true: 1}[up]]
			c = colWarn
		}
	}
	a.Dyn(x11ui.Text(img, a.F.Heading, r.Min.X, r.Min.Y+a.Px(610), msg, c, x11ui.Left))

	cov := a.HP.Coverage()
	a.Dyn(x11ui.Text(img, a.F.Small, r.Min.X, r.Min.Y+a.Px(660), fmt.Sprintf("%d campioni per la calibrazione", cov.Samples), colDim, x11ui.Left))

	if !m.done && !lead && (full == len(m.fill) || el-hpLead >= m.maxDur) {
		m.finish(a, frac)
	}
}

func (m *hpMove) finish(a *App, frac float64) {
	m.done = true
	cov := a.HP.Coverage()
	a.Log.Info("head pose calibration phase done", "phase", m.title, "filled_pct", math.Round(100*frac),
		"seconds", math.Round(time.Since(m.start).Seconds()), "samples", cov.Samples, "cells", cov.Cells)
	a.Next()
}

func (m *hpMove) Event(a *App, ev x11ui.Event) {
	if ev.Kind != x11ui.KeyDown {
		return
	}
	switch ev.Sym {
	case x11ui.KeyEscape:
		a.Home()
	case x11ui.KeyReturn, x11ui.KeyKPEnter:
		if _, frac := m.filled(); frac >= skipFilled && !m.done {
			m.finish(a, frac)
		}
	}
}

// --- 3. calcolo della calibrazione ------------------------------------------

type hpCompute struct {
	ses     *hpSession
	lay     hpLayout
	busy    bool
	err     error
	cam     *headpose.Camera
	q       headpose.CalibQuality
	started time.Time
}

func newHPCompute(ses *hpSession) *hpCompute { return &hpCompute{ses: ses} }

func (s *hpCompute) Static(a *App, img *image.RGBA) {
	s.lay.static(a, img, "Calcolo della calibrazione della camera", "")
	r := s.lay.right
	if !s.busy && s.cam == nil && s.err == nil {
		s.busy, s.started = true, time.Now()
		a.HP.CalibrateNow(func(cam *headpose.Camera, q headpose.CalibQuality, err error) {
			a.Post(func() {
				s.busy, s.cam, s.q, s.err = false, cam, q, err
				a.RefreshStatic()
			})
		})
	}
	y := r.Min.Y + a.Px(30)
	switch {
	case s.busy:
		x11ui.Text(img, a.F.Heading, r.Min.X, y, "Sto calcolando…", colText, x11ui.Left)
		return
	case s.err != nil:
		x11ui.Text(img, a.F.Heading, r.Min.X, y, "Calibrazione non riuscita", colBad, x11ui.Left)
		a.paragraph(img, a.F.Body, r.Min.X, y+a.Px(50), r.Dx(), s.err.Error(), colText, x11ui.Left)
		a.drawKeys(img, a.W/2, a.H-a.Px(70), []keyHint{{"R", "aggiungi campioni"}, {"Esc", "esci"}}, x11ui.Center)
		return
	}
	ok := headpose.CalibrationReliable(s.q)
	head, hc := "Calibrazione affidabile", colOK
	if !ok {
		head, hc = "Calibrazione non abbastanza precisa", colWarn
	}
	x11ui.Text(img, a.F.Heading, r.Min.X, y, head, hc, x11ui.Left)
	rows := [][2]string{
		{"Errore di riproiezione", it(s.q.RMSPx, 2) + " px (soglia 4)"},
		{"Incertezza della rotazione", it(s.q.SigmaDeg, 2) + "° (soglia 1°)"},
		{"Focale", it(s.cam.F, 0) + " px"},
		{"Punto principale", it(s.cam.CX, 0) + ", " + it(s.cam.CY, 0) + " px (centro 320, 240)"},
		{"Posizione della camera nel tracker", fmt.Sprintf("%s, %s, %s mm", it(s.cam.C[0], 0), it(s.cam.C[1], 0), it(s.cam.C[2], 0))},
		{"Inclinazione dell'asse ottico", it(headpose.CameraTiltDeg(s.cam), 1) + "°"},
		{"Immagine speculare", map[bool]string{true: "sì", false: "no"}[s.cam.Mirror]},
		{"Campioni / escursione in profondità", fmt.Sprintf("%d / %s cm", s.q.Pairs/2, it(s.q.DepthMM/10, 0))},
	}
	yy := y + a.Px(60)
	for _, row := range rows {
		x11ui.Text(img, a.F.Body, r.Min.X, yy, row[0], colDim, x11ui.Left)
		x11ui.Text(img, a.F.Body, r.Min.X+a.Px(420), yy, row[1], colText, x11ui.Left)
		yy += a.Px(40)
	}
	if ok {
		a.paragraph(img, a.F.Body, r.Min.X, yy+a.Px(20), r.Dx(), "Invio la salva e passa alla verifica del pitch.", colText, x11ui.Left)
		a.drawKeys(img, a.W/2, a.H-a.Px(70), []keyHint{{"Invio", "salva e continua"}, {"R", "aggiungi campioni"}, {"Esc", "esci senza salvare"}}, x11ui.Center)
	} else {
		a.paragraph(img, a.F.Body, r.Min.X, yy+a.Px(20), r.Dx(),
			"Con R rifai le tre fasi: i nuovi campioni si aggiungono a quelli già raccolti e la stima migliora. Puoi anche provarla senza salvarla.", colText, x11ui.Left)
		a.drawKeys(img, a.W/2, a.H-a.Px(70), []keyHint{{"R", "aggiungi campioni"}, {"P", "prova senza salvare"}, {"Esc", "esci"}}, x11ui.Center)
	}
}

func (s *hpCompute) Frame(a *App, img *image.RGBA, now time.Time) {
	s.lay.frame(a, img)
	if s.busy {
		r := s.lay.right
		a.Dyn(x11ui.Text(img, a.F.Body, r.Min.X, r.Min.Y+a.Px(80), fmt.Sprintf("%.0f s", now.Sub(s.started).Seconds()), colDim, x11ui.Left))
	}
}

func (s *hpCompute) Event(a *App, ev x11ui.Event) {
	if ev.Kind != x11ui.KeyDown || s.busy {
		return
	}
	ok := s.cam != nil && headpose.CalibrationReliable(s.q)
	switch ev.Sym {
	case x11ui.KeyEscape:
		a.Quit()
	case 'r': // altre fasi, tenendo i campioni già raccolti
		ses := s.ses
		a.Flow(
			func() Screen { return newHPMove(ses, moveDepth) },
			func() Screen { return newHPMove(ses, moveSide) },
			func() Screen { return newHPMove(ses, moveHeight) },
			func() Screen { return newHPCompute(ses) },
		)
	case x11ui.KeyReturn, x11ui.KeyKPEnter:
		if !ok {
			return
		}
		s.ses.cam, s.ses.q, s.ses.calibrated = s.cam, s.q, true
		s.ses.saveErr = a.HP.Adopt(s.cam, s.q, true)
		s.ses.saved = s.ses.saveErr == nil
		a.Flow(func() Screen { return newHPPitch(s.ses) }, func() Screen { return newHPPresence(s.ses) },
			func() Screen { return newHPSummary(s.ses) })
	case 'p':
		if s.cam == nil {
			return
		}
		s.ses.cam, s.ses.q, s.ses.calibrated = s.cam, s.q, true
		_ = a.HP.Adopt(s.cam, s.q, false)
		a.Flow(func() Screen { return newHPPitch(s.ses) }, func() Screen { return newHPPresence(s.ses) },
			func() Screen { return newHPSummary(s.ses) })
	}
}

// --- 4. zero e verifica del pitch -------------------------------------------

type pitchStep struct {
	title, what string
	dur         time.Duration
}

var pitchSteps = []pitchStep{
	{"Testa dritta", "Guarda il punto bianco al centro dello schermo, con la testa dritta e rilassata.", 7 * time.Second},
	{"Alza il mento", "Continua a guardare il punto e alza il mento, come per guardare un po' più in alto.", 6 * time.Second},
	{"Abbassa il mento", "Continua a guardare il punto e abbassa il mento verso il petto, senza esagerare.", 6 * time.Second},
}

type hpPitch struct {
	ses   *hpSession
	step  int
	start time.Time
	sum   [3]float64
	n     [3]int
	dyaw  float64
	droll float64
	nd    int
	g     gauge
}

func newHPPitch(ses *hpSession) *hpPitch { return &hpPitch{ses: ses} }

func (p *hpPitch) Static(a *App, img *image.RGBA) {
	a.ShowCursor(false)
	st := pitchSteps[p.step]
	name := "Zero del viso e verifica del pitch"
	if p.ses.mode == hpVerify {
		name = "Verifica del pitch"
	}
	x11ui.Text(img, a.F.Title, a.W/2, a.Px(150), fmt.Sprintf("%s · %d di 3 · %s", name, p.step+1, st.title), colText, x11ui.Center)
	a.paragraph(img, a.F.Body, a.W/2, a.Px(205), a.Px(1100), st.what, colDim, x11ui.Center)
	p.g = gauge{r: image.Rect(a.W/2-a.Px(450), a.H-a.Px(260), a.W/2+a.Px(450), a.H-a.Px(252)), lo: -40, hi: 40,
		ticks: []float64{-40, -20, 0, 20, 40}, label: func(v float64) string { return it(v, 0) + "°" }}
	x11ui.Text(img, a.F.Small, p.g.r.Min.X, p.g.r.Min.Y-a.Px(60), "Pitch della testa dal vivo (positivo = verso l'alto)", colDim, x11ui.Left)
	p.g.static(a, img)
	a.drawKeys(img, a.W/2, a.H-a.Px(70), []keyHint{{"Esc", "esci"}}, x11ui.Center)
	p.start = time.Now()
}

func (p *hpPitch) Frame(a *App, img *image.RGBA, now time.Time) {
	st := pitchSteps[p.step]
	cx, cy := float64(a.W)/2, float64(a.H)/2
	a.drawDot(img, cx, cy, dotStyle{radius: 14, progress: -1, pop: -1})
	el := now.Sub(p.start)
	countdown(a, img, a.W/2-a.Px(150), a.Px(330), a.Px(300), st.dur-el, st.dur, "")
	res := a.HP.Last()
	lv := readLive(a)
	valid := res.Valid && lv.fresh && time.Duration(lv.s.DeviceTimeUs-res.DeviceUs)*time.Microsecond < 300*time.Millisecond
	// testa dritta: rispetto allo zero salvato; poi rispetto a quella appena misurata
	pitchDeg := (res.RawPitch - a.HP.PitchOffset()) * 180 / math.Pi
	if p.step > 0 && p.ses.mode != hpVerify {
		pitchDeg = (res.RawPitch - p.ses.neutral) * 180 / math.Pi
	}
	p.g.frame(a, img, 0, false, pitchDeg, valid, true)
	if !valid {
		a.Dyn(x11ui.Text(img, a.F.Heading, a.W/2, a.H-a.Px(330), "Pitch non disponibile: guarda il punto", colWarn, x11ui.Center))
	}
	// si scarta il primo secondo e mezzo di ogni passo (assestamento)
	if valid && el > 1500*time.Millisecond {
		p.sum[p.step] += res.RawPitch
		p.n[p.step]++
		if p.step == 0 && lv.s.Head.RotationValid[1] {
			p.dyaw += res.Yaw - float64(lv.s.Head.RotationRad[1])
			p.droll += res.Roll - float64(lv.s.Head.RotationRad[2])
			p.nd++
		}
	}
	if el < st.dur {
		return
	}
	if p.step == 0 && p.n[0] > 0 {
		p.ses.neutral = p.sum[0] / float64(p.n[0])
	}
	if p.step < len(pitchSteps)-1 {
		p.step++
		a.RefreshStatic()
		return
	}
	mean := func(i int) float64 {
		if p.n[i] == 0 {
			return math.NaN()
		}
		return p.sum[i] / float64(p.n[i])
	}
	p.ses.neutral, p.ses.up, p.ses.down = mean(0), mean(1), mean(2)
	if p.nd > 0 {
		p.ses.yawDiff, p.ses.rollDiff = p.dyaw/float64(p.nd), p.droll/float64(p.nd)
	}
	p.ses.pitchDone = true
	if p.ses.mode != hpVerify && !math.IsNaN(p.ses.neutral) {
		p.ses.offsetSaved = a.HP.SetPitchOffset(p.ses.neutral) == nil
	}
	a.Log.Info("head pose pitch check", "neutral_raw_deg", it(deg(p.ses.neutral), 1),
		"up_deg", it(deg(p.ses.up-p.ses.neutral), 1), "down_deg", it(deg(p.ses.down-p.ses.neutral), 1),
		"yaw_img_minus_eyes_deg", it(deg(p.ses.yawDiff), 1), "roll_img_minus_eyes_deg", it(deg(p.ses.rollDiff), 1))
	a.Next()
}

func (p *hpPitch) Event(a *App, ev x11ui.Event) {
	if ev.Kind == x11ui.KeyDown && ev.Sym == x11ui.KeyEscape {
		a.Quit()
	}
}

func deg(r float64) float64 { return r * 180 / math.Pi }

// --- 5. presenza ------------------------------------------------------------

type hpPresence struct {
	ses   *hpSession
	lay   hpLayout
	phase int // 0 allontanati, 1 torna
	start time.Time
	t0    time.Time
}

const presenceTimeout = 25 * time.Second

func newHPPresence(ses *hpSession) *hpPresence { return &hpPresence{ses: ses} }

func (s *hpPresence) Static(a *App, img *image.RGBA) {
	title := "Verifica della presenza · Allontanati"
	what := "Alzati e allontanati dal sensore, fuori dalla sua vista. Torna quando te lo dico (lo vedi da lontano: il riquadro diventa verde)."
	if s.phase == 1 {
		title = "Verifica della presenza · Torna"
		what = "Torna a sederti davanti allo schermo e guardalo."
	}
	s.lay.static(a, img, title, "")
	r := s.lay.right
	a.paragraph(img, a.F.Body, r.Min.X, r.Min.Y+a.Px(20), r.Dx(), what, colText, x11ui.Left)
	a.drawKeys(img, a.W/2, a.H-a.Px(70), []keyHint{{"S", "salta"}, {"Esc", "esci"}}, x11ui.Center)
	s.start = time.Now()
	if s.t0.IsZero() {
		s.t0 = s.start
	}
}

func (s *hpPresence) Frame(a *App, img *image.RGBA, now time.Time) {
	s.lay.frame(a, img)
	lv := readLive(a)
	r := s.lay.right
	pres := lv.s.Presence
	el := now.Sub(s.start)
	countdown(a, img, r.Min.X, r.Min.Y+a.Px(230), r.Dx(), presenceTimeout-el, presenceTimeout, "al massimo")
	label := map[gaze.Presence]string{gaze.PresenceUnknown: "sconosciuta", gaze.PresenceAway: "ASSENTE", gaze.PresencePresent: "PRESENTE"}[pres]
	c := colWarn
	if (s.phase == 0 && pres == gaze.PresenceAway) || (s.phase == 1 && pres == gaze.PresencePresent) {
		c = colOK
	}
	box := image.Rect(r.Min.X, r.Min.Y+a.Px(320), r.Max.X, r.Min.Y+a.Px(520))
	a.Dyn(x11ui.RoundRect(img, box, a.Pf(16), x11ui.Alpha(c, 0.35)))
	a.Dyn(x11ui.Text(img, a.F.Big, box.Min.X+box.Dx()/2, box.Min.Y+box.Dy()/2+a.Px(25), "Presenza: "+label, colText, x11ui.Center))
	done := false
	switch s.phase {
	case 0:
		if pres == gaze.PresenceAway && el > time.Second {
			s.ses.awaySec, s.ses.presenceOK = el.Seconds(), true
			done = true
		}
	case 1:
		if pres == gaze.PresencePresent && lv.haveEyes {
			s.ses.backSec, s.ses.returnOK = el.Seconds(), true
			done = true
		}
	}
	if el >= presenceTimeout {
		done = true
	}
	if !done {
		return
	}
	if s.phase == 0 {
		s.phase = 1
		a.RefreshStatic()
		return
	}
	s.ses.presenceDone = true
	a.Next()
}

func (s *hpPresence) Event(a *App, ev x11ui.Event) {
	if ev.Kind != x11ui.KeyDown {
		return
	}
	switch ev.Sym {
	case x11ui.KeyEscape:
		a.Quit()
	case 's':
		a.Next()
	}
}

// --- 6. riepilogo -----------------------------------------------------------

type hpSummary struct{ ses *hpSession }

func newHPSummary(ses *hpSession) *hpSummary { return &hpSummary{ses: ses} }

func (s *hpSummary) Static(a *App, img *image.RGBA) {
	a.ShowCursor(false)
	x11ui.Text(img, a.F.Title, a.W/2, a.Px(140), "Riepilogo", colText, x11ui.Center)
	ses := s.ses
	x0, xv := a.W/2-a.Px(600), a.W/2+a.Px(20)
	y := a.Px(240)
	line := func(k, v string, c color.RGBA) {
		x11ui.Text(img, a.F.Body, x0, y, k, colDim, x11ui.Left)
		x11ui.Text(img, a.F.Body, xv, y, v, c, x11ui.Left)
		y += a.Px(44)
	}
	okc := func(b bool) color.RGBA {
		if b {
			return colOK
		}
		return colWarn
	}
	if ses.calibrated {
		saved := "non salvata (prova)"
		if ses.saved {
			saved = "salvata"
		} else if ses.saveErr != nil {
			saved = "errore nel salvataggio: " + ses.saveErr.Error()
		}
		line("Calibrazione della camera", fmt.Sprintf("errore %s px, incertezza %s° · %s", it(ses.q.RMSPx, 2), it(ses.q.SigmaDeg, 2), saved),
			okc(headpose.CalibrationReliable(ses.q)))
	}
	if ses.pitchDone {
		up, down := deg(ses.up-ses.neutral), deg(ses.down-ses.neutral)
		signOK := up > 5 && down < -5
		if ses.mode == hpVerify {
			off := a.HP.PitchOffset()
			z := deg(ses.neutral - off)
			line("Pitch a testa dritta (rispetto allo zero salvato)", it(z, 1)+"°", okc(math.Abs(z) < 4))
		} else {
			line("Zero personale del pitch", it(deg(ses.neutral), 1)+"° "+map[bool]string{true: "(salvato)", false: "(non salvato)"}[ses.offsetSaved], colText)
		}
		line("Mento alzato / abbassato", fmt.Sprintf("%s° / %s°", it(up, 1), it(down, 1)), okc(signOK))
		verdict := "il pitch cambia nel verso giusto"
		if !signOK {
			verdict = "il pitch NON cambia come atteso"
		}
		line("", verdict, okc(signOK))
		cons := math.Abs(deg(ses.yawDiff)) < 5 && math.Abs(deg(ses.rollDiff)) < 5
		line("Yaw e roll: immagine − occhi", fmt.Sprintf("%s° / %s°", it(deg(ses.yawDiff), 1), it(deg(ses.rollDiff), 1)), okc(cons))
	}
	if ses.presenceDone {
		line("Presenza assente quando ti allontani", map[bool]string{true: fmt.Sprintf("sì, dopo %s s", it(ses.awaySec, 1)), false: "no"}[ses.presenceOK], okc(ses.presenceOK))
		line("Presenza presente quando torni", map[bool]string{true: fmt.Sprintf("sì, dopo %s s", it(ses.backSec, 1)), false: "no"}[ses.returnOK], okc(ses.returnOK))
	}
	a.paragraph(img, a.F.Small, a.W/2, y+a.Px(30), a.Px(1100),
		"I dati di questa sessione (campioni, landmark, immagini annotate) sono nella cartella indicata nel terminale.", colFaint, x11ui.Center)
	a.drawKeys(img, a.W/2, a.H-a.Px(70), []keyHint{{"R", "ricomincia"}, {"Esc", "esci"}}, x11ui.Center)
}

func (s *hpSummary) Frame(a *App, img *image.RGBA, now time.Time) {}

func (s *hpSummary) Event(a *App, ev x11ui.Event) {
	if ev.Kind != x11ui.KeyDown {
		return
	}
	switch ev.Sym {
	case x11ui.KeyEscape, x11ui.KeyReturn:
		a.Quit()
	case 'r':
		a.Home()
	}
}
