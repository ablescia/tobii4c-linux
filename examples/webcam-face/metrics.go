//go:build linux

package main

import (
	"math"
	"sort"
	"time"

	"github.com/ablescia/tobii4c-linux/internal/headpose"
)

// Misure ricavate dai 478 landmark del viso. Le distanze in mm usano come
// righello il diametro dell'iride, quasi uguale in tutti gli adulti
// (11,7 mm, lo stesso valore di MediaPipe Iris).
const irisDiameterMM = 11.7

// eyeIdx sono i landmark di un occhio. "Verso destra" e "verso sinistra"
// sono riferiti al soggetto.
type eyeIdx struct {
	toRight, toLeft int    // angoli dell'occhio
	up, down        int    // centro delle palpebre superiore e inferiore
	ear             [6]int // p1..p6 dell'eye aspect ratio
	iris            int    // centro dell'iride; i 4 punti del bordo seguono
}

// Indice 0: occhio sinistro del soggetto, 1: destro (come i colori del driver).
var eyeLM = [2]eyeIdx{
	{toRight: 362, toLeft: 263, up: 386, down: 374, ear: [6]int{362, 385, 387, 263, 373, 380}, iris: headpose.LandmarkLeftIris},
	{toRight: 33, toLeft: 133, up: 159, down: 145, ear: [6]int{33, 160, 158, 133, 153, 144}, iris: headpose.LandmarkRightIris},
}

// Landmark della bocca: interno delle labbra (13 sopra, 14 sotto), angoli
// interni (78, 308) ed esterni (61, 291); angoli esterni degli occhi.
const (
	lmLipTop, lmLipBottom    = 13, 14
	lmMouthInL, lmMouthInR   = 78, 308
	lmMouthOutL, lmMouthOutR = 61, 291
	lmEyeOuterR, lmEyeOuterL = 33, 263
)

// EyeMetrics sono le misure di un occhio.
type EyeMetrics struct {
	Iris   [2]float64 // centro dell'iride, pixel
	IrisPx float64    // diametro dell'iride, pixel
	EAR    float64    // eye aspect ratio: ~0,3 aperto, <0,2 chiuso
	OpenMM float64    // distanza tra le palpebre, mm
	PosH   float64    // iride nell'occhio: −1 angolo verso la destra del soggetto, +1 verso la sinistra
	PosV   float64    // −1 palpebra superiore, +1 inferiore
}

// Metrics sono le misure del viso in un frame.
type Metrics struct {
	Pose    headpose.FacePose
	PoseOK  bool
	DistMM  float64 // distanza della testa dalla camera, dal fit del modello canonico
	IrisMM  float64 // distanza dell'iride più vicina, dal suo diametro
	MMPerPx float64 // scala dal diametro dell'iride
	Eyes    [2]EyeMetrics
	IPDPx   float64 // distanza tra i centri delle iridi
	IPDMM   float64

	MouthOpenMM  float64
	MouthWidthMM float64
	MAR          float64 // apertura / larghezza interna della bocca

	Box       [4]float64 // riquadro dei 468 punti: x0, y0, x1, y1 (pixel)
	EyeLine   float64    // inclinazione della linea degli angoli esterni nell'immagine, rad
	DepthSpan float64    // escursione della z dei landmark (pixel), dal naso alle guance
}

func dist2(a, b [3]float64) float64 { return math.Hypot(a[0]-b[0], a[1]-b[1]) }

// earOf è l'eye aspect ratio di Soukupová e Čech:
// (|p2−p6| + |p3−p5|) / (2·|p1−p4|).
func earOf(lm *headpose.Landmarks, p [6]int) float64 {
	w := dist2(lm[p[0]], lm[p[3]])
	if w < 1e-6 {
		return 0
	}
	return (dist2(lm[p[1]], lm[p[5]]) + dist2(lm[p[2]], lm[p[4]])) / (2 * w)
}

// computeMetrics ricava le misure dai landmark di un'immagine w×h (non
// speculare); focal è la focale ipotizzata della webcam in pixel.
func computeMetrics(lm *headpose.Landmarks, w, h int, focal float64) Metrics {
	var m Metrics
	m.Pose, m.PoseOK = headpose.PoseInCamera(lm, w, h, focal)
	if m.PoseOK {
		c := m.Pose.Center
		m.DistMM = math.Sqrt(c[0]*c[0] + c[1]*c[1] + c[2]*c[2])
	}
	irisPx := 0.0
	for k, e := range eyeLM {
		em := &m.Eyes[k]
		c := lm[e.iris]
		em.Iris = [2]float64{c[0], c[1]}
		// diametro: media delle due coppie opposte del bordo
		em.IrisPx = (dist2(lm[e.iris+1], lm[e.iris+3]) + dist2(lm[e.iris+2], lm[e.iris+4])) / 2
		irisPx = math.Max(irisPx, em.IrisPx) // l'occhio meno di scorcio
		em.EAR = earOf(lm, e.ear)
		// posizione dell'iride sull'asse degli angoli e sulla sua normale
		a, b := lm[e.toRight], lm[e.toLeft]
		dx, dy := b[0]-a[0], b[1]-a[1]
		l2 := dx*dx + dy*dy
		if l2 > 1e-6 {
			t := ((c[0]-a[0])*dx + (c[1]-a[1])*dy) / l2
			em.PosH = (t - 0.5) * 2
			// normale verso il basso dell'immagine (y scende)
			l := math.Sqrt(l2)
			nx, ny := -dy/l, dx/l
			if ny < 0 {
				nx, ny = -nx, -ny
			}
			up, dn := lm[e.up], lm[e.down]
			mx, my := (up[0]+dn[0])/2, (up[1]+dn[1])/2
			if open := dist2(up, dn); open > 1e-3 {
				em.PosV = ((c[0]-mx)*nx + (c[1]-my)*ny) / (open / 2)
			}
		}
	}
	if irisPx > 0.5 {
		m.MMPerPx = irisDiameterMM / irisPx
		m.IrisMM = focal * irisDiameterMM / irisPx
	}
	for k, e := range eyeLM {
		m.Eyes[k].OpenMM = dist2(lm[e.up], lm[e.down]) * m.MMPerPx
	}
	m.IPDPx = dist2(lm[headpose.LandmarkLeftIris], lm[headpose.LandmarkRightIris])
	m.IPDMM = m.IPDPx * m.MMPerPx

	open := dist2(lm[lmLipTop], lm[lmLipBottom])
	if inner := dist2(lm[lmMouthInL], lm[lmMouthInR]); inner > 1e-3 {
		m.MAR = open / inner
	}
	m.MouthOpenMM = open * m.MMPerPx
	m.MouthWidthMM = dist2(lm[lmMouthOutL], lm[lmMouthOutR]) * m.MMPerPx

	m.Box = [4]float64{math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)}
	zlo, zhi := math.Inf(1), math.Inf(-1)
	for i := 0; i < headpose.MeshPoints; i++ {
		p := lm[i]
		m.Box[0], m.Box[1] = math.Min(m.Box[0], p[0]), math.Min(m.Box[1], p[1])
		m.Box[2], m.Box[3] = math.Max(m.Box[2], p[0]), math.Max(m.Box[3], p[1])
		zlo, zhi = math.Min(zlo, p[2]), math.Max(zhi, p[2])
	}
	m.DepthSpan = zhi - zlo
	r, l := lm[lmEyeOuterR], lm[lmEyeOuterL]
	m.EyeLine = math.Atan2(l[1]-r[1], l[0]-r[0])
	return m
}

// ---------------------------------------------------------------------------
// Ammiccamenti

// blinker riconosce gli ammiccamenti dall'EAR medio dei due occhi: chiuso
// sotto il 70 % del riferimento a occhi aperti, riaperto sopra l'80 %; un
// ammiccamento dura al più 700 ms. Il riferimento è il 75° percentile
// dell'EAR negli ultimi 5 s: un ammiccamento (pochi frame) non lo sposta,
// mentre si adatta in pochi secondi se l'EAR cala perché il soggetto guarda
// in basso.
type blinker struct {
	recent  []earSample
	sorted  []float64
	base    float64
	closed  bool
	since   time.Time
	times   []time.Time // ammiccamenti dell'ultimo minuto
	lastDur time.Duration
	total   uint64
}

type earSample struct {
	t time.Time
	v float64
}

const (
	maxBlink   = 700 * time.Millisecond
	blinkSpan  = 5 * time.Second
	blinkClose = 0.7
	blinkOpen  = 0.8
)

func (b *blinker) update(ear float64, t time.Time, ok bool) {
	if !ok || ear <= 0 {
		b.closed = false
		return
	}
	cut := 0
	for cut < len(b.recent) && t.Sub(b.recent[cut].t) > blinkSpan {
		cut++
	}
	b.recent = append(b.recent[:0], b.recent[cut:]...)
	b.recent = append(b.recent, earSample{t, ear})
	b.sorted = b.sorted[:0]
	for _, s := range b.recent {
		b.sorted = append(b.sorted, s.v)
	}
	sort.Float64s(b.sorted)
	b.base = b.sorted[(len(b.sorted)-1)*3/4]
	switch {
	case !b.closed && ear < blinkClose*b.base:
		b.closed, b.since = true, t
	case b.closed && ear > blinkOpen*b.base:
		b.closed = false
		if d := t.Sub(b.since); d <= maxBlink {
			b.lastDur = d
			b.total++
			b.times = append(b.times, t)
		}
	}
	old := 0
	for old < len(b.times) && t.Sub(b.times[old]) > time.Minute {
		old++
	}
	b.times = append(b.times[:0], b.times[old:]...)
}

// ---------------------------------------------------------------------------
// Statistiche dell'immagine

// ImageStats descrive la luminanza del frame (campionato a passo 2) e del
// riquadro del viso.
type ImageStats struct {
	Mean, Std           float64
	Min, P1, P50, P99   int
	Max                 int
	Dark, Bright        float64 // frazione dei pixel ≤ 5 e ≥ 250
	Hist                [64]float32
	FaceMean, FaceStd   float64
	FaceSharp           float64 // varianza del laplaciano nel riquadro del viso
	FaceP1, FaceP99     int
	HaveFace            bool
	SampledPixels       int
	ComputeMicroseconds float64
}

func imageStats(pix []byte, w, h int, box *[4]float64) ImageStats {
	t0 := time.Now()
	var st ImageStats
	var hist [256]int
	n := 0
	var sum, sum2 float64
	for y := 0; y < h; y += 2 {
		row := pix[y*w : (y+1)*w]
		for x := 0; x < w; x += 2 {
			v := row[x]
			hist[v]++
			f := float64(v)
			sum += f
			sum2 += f * f
			n++
		}
	}
	if n == 0 {
		return st
	}
	st.SampledPixels = n
	st.Mean = sum / float64(n)
	st.Std = math.Sqrt(math.Max(0, sum2/float64(n)-st.Mean*st.Mean))
	st.Min, st.Max = 255, 0
	for v, c := range hist {
		if c > 0 {
			st.Min = min(st.Min, v)
			st.Max = max(st.Max, v)
		}
		st.Hist[v/4] += float32(c) / float32(n)
		if v <= 5 {
			st.Dark += float64(c)
		}
		if v >= 250 {
			st.Bright += float64(c)
		}
	}
	st.Dark /= float64(n)
	st.Bright /= float64(n)
	st.P1, st.P50, st.P99 = pct(&hist, n, 0.01), pct(&hist, n, 0.5), pct(&hist, n, 0.99)

	if box != nil {
		x0, y0 := max(1, int(box[0])), max(1, int(box[1]))
		x1, y1 := min(w-1, int(box[2])), min(h-1, int(box[3]))
		if x1-x0 > 8 && y1-y0 > 8 {
			var fh [256]int
			var fs, fs2, ls, ls2 float64
			fn, ln := 0, 0
			for y := y0; y < y1; y++ {
				for x := x0; x < x1; x++ {
					o := y*w + x
					v := float64(pix[o])
					if (x|y)&1 == 0 {
						fh[pix[o]]++
						fs += v
						fs2 += v * v
						fn++
					}
					lap := 4*v - float64(pix[o-1]) - float64(pix[o+1]) - float64(pix[o-w]) - float64(pix[o+w])
					ls += lap
					ls2 += lap * lap
					ln++
				}
			}
			if fn > 0 && ln > 0 {
				st.HaveFace = true
				st.FaceMean = fs / float64(fn)
				st.FaceStd = math.Sqrt(math.Max(0, fs2/float64(fn)-st.FaceMean*st.FaceMean))
				lm := ls / float64(ln)
				st.FaceSharp = ls2/float64(ln) - lm*lm
				st.FaceP1, st.FaceP99 = pct(&fh, fn, 0.01), pct(&fh, fn, 0.99)
			}
		}
	}
	st.ComputeMicroseconds = float64(time.Since(t0).Nanoseconds()) / 1000
	return st
}

func pct(h *[256]int, total int, q float64) int {
	target := int(q * float64(total))
	acc := 0
	for i, c := range h {
		acc += c
		if acc > target {
			return i
		}
	}
	return 255
}

// ---------------------------------------------------------------------------

// timing tiene gli ultimi tempi di elaborazione per media e percentili.
type timing struct {
	v [120]float64
	n int
}

func (t *timing) add(ms float64) {
	t.v[t.n%len(t.v)] = ms
	t.n++
}

// stats restituisce media, 95° percentile e massimo.
func (t *timing) stats() (mean, p95, maxv float64) {
	n := min(t.n, len(t.v))
	if n == 0 {
		return 0, 0, 0
	}
	s := make([]float64, n)
	copy(s, t.v[:n])
	sort.Float64s(s)
	for _, v := range s {
		mean += v
	}
	return mean / float64(n), s[min(n-1, int(0.95*float64(n)))], s[n-1]
}
